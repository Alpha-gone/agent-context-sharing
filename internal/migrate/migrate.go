// Package migrate는 번호를 붙인 마이그레이션 파일을 순서대로 적용한다.
//
// SDD.md의 「스키마 적용」이 정한 대로 이 패키지는 「패키지 경계」의 11개 패키지 밖에
// 있으며 store를 거치지 않는다. store가 데이터베이스 핸들을 밖으로 내보내지 않기로 한
// 계약을 지키기 위해서다.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config는 마이그레이션 파일에 치환할 배포 구성 값이다.
// 세 값 모두 식별자나 타입 자리에 들어가 매개변수로 묶을 수 없으므로 Validate가
// 통과한 값만 사용한다.
type Config struct {
	GraphName  string
	VectorType string
	VectorDim  int
}

var (
	graphNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	vectorTypes      = []string{"vector", "halfvec", "bit"}
	fileNamePattern  = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.sql$`)
)

// Validate는 구성 값이 치환해도 안전한 형태인지 확인한다.
func (c Config) Validate() error {
	if !graphNamePattern.MatchString(c.GraphName) {
		return fmt.Errorf("그래프 이름 %q가 형식 %s에 맞지 않는다", c.GraphName, graphNamePattern)
	}
	if !slices.Contains(vectorTypes, c.VectorType) {
		return fmt.Errorf("벡터 타입 %q는 %v 중 하나여야 한다", c.VectorType, vectorTypes)
	}
	if c.VectorDim <= 0 {
		return fmt.Errorf("벡터 차원 %d는 양의 정수여야 한다", c.VectorDim)
	}
	return nil
}

// Migration은 적용 대상 파일 하나다.
type Migration struct {
	Version  int
	Name     string
	Rendered string
	Checksum string
}

// Applied는 이미 적용된 마이그레이션의 기록이다.
type Applied struct {
	Version  int
	Name     string
	Checksum string
}

// AfterConnectSQL은 AGE를 쓰는 모든 연결이 거쳐야 하는 준비 문장이다.
// SDD.md의 「데이터베이스 연결」이 정한 것과 같으며, 그래프와 label 생성 함수가
// ag_catalog에 있으므로 마이그레이션 연결에도 필요하다.
var AfterConnectSQL = []string{
	"LOAD 'age'",
	`SET search_path = ag_catalog, "$user", public`,
}

// NewPool은 AGE 준비를 마친 연결만 내주는 풀을 만든다.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("접속 문자열 해석: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		for _, stmt := range AfterConnectSQL {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("연결 준비 %q: %w", stmt, err)
			}
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("풀 생성: %w", err)
	}
	return pool, nil
}

// AdvisoryLockKey는 마이그레이션 실행기가 쓰는 자문 잠금의 키다.
//
// 값 자체에 의미는 없고 같은 데이터베이스를 쓰는 다른 잠금과 겹치지 않는 것만 중요하다.
// SDD.md의 「주기 작업」이 쓸 잠금은 다른 값을 써야 한다.
const AdvisoryLockKey int64 = 4182026001

// WithLock은 자문 잠금을 잡은 채 fn을 실행하고 끝나면 놓는다.
//
// SDD.md의 「기동과 종료」가 확정한 대로 여러 인스턴스가 동시에 기동해도 한 번만
// 적용되게 한다. 이미 다른 실행기가 잡고 있으면 놓을 때까지 기다리므로, 호출자는
// 적용 이력 조회를 fn 안에서 해야 기다린 뒤 앞선 실행기의 결과를 본다.
//
// 자문 잠금은 세션 단위라 잡은 연결에서 놓아야 한다. pgxpool은 반납한 연결의 세션
// 상태를 초기화하지 않으므로 놓지 않으면 잠금이 풀에 남는다. fn이 오류를 돌려주거나
// ctx가 취소되어도 놓으며, 해제에 실패하면 그 오류를 fn의 오류와 함께 돌려준다.
func WithLock(ctx context.Context, pool *pgxpool.Pool, fn func(context.Context) error) (err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("잠금용 연결 획득: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, AdvisoryLockKey); err != nil {
		return fmt.Errorf("자문 잠금 획득: %w", err)
	}
	defer func() {
		// 취소된 뒤에도 놓아야 하므로 취소를 떼어낸 컨텍스트를 쓴다.
		if _, unlockErr := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, AdvisoryLockKey); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("자문 잠금 해제: %w", unlockErr))
		}
	}()

	return fn(ctx)
}

// Load는 파일 시스템에서 마이그레이션을 읽어 구성 값을 치환하고 버전 순으로 돌려준다.
func Load(fsys fs.FS, dir string, cfg Config) ([]Migration, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("마이그레이션 디렉터리 읽기: %w", err)
	}

	var migrations []Migration
	seen := make(map[int]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := fileNamePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("파일 이름 %q가 NNN_name.sql 형식이 아니다", entry.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("파일 %q의 버전 해석: %w", entry.Name(), err)
		}
		if prev, ok := seen[version]; ok {
			return nil, fmt.Errorf("버전 %d가 %q와 %q에 중복된다", version, prev, entry.Name())
		}
		seen[version] = entry.Name()

		raw, err := fs.ReadFile(fsys, path.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("파일 %q 읽기: %w", entry.Name(), err)
		}
		tmpl, err := template.New(entry.Name()).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("파일 %q 템플릿 해석: %w", entry.Name(), err)
		}
		var rendered strings.Builder
		if err := tmpl.Execute(&rendered, cfg); err != nil {
			return nil, fmt.Errorf("파일 %q 치환: %w", entry.Name(), err)
		}

		sum := sha256.Sum256([]byte(rendered.String()))
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     match[2],
			Rendered: rendered.String(),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	slices.SortFunc(migrations, func(a, b Migration) int { return a.Version - b.Version })
	return migrations, nil
}

// 스키마를 public으로 명시하는 이유는 AfterConnectSQL이 정한 search_path의 첫 항목이
// ag_catalog이기 때문이다. 명시하지 않으면 AGE의 카탈로그 스키마에 만들어진다.
const createHistoryTable = `
CREATE TABLE IF NOT EXISTS public.schema_migration (
    version    integer     PRIMARY KEY,
    name       text        NOT NULL,
    checksum   text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// EnsureHistory는 적용 이력 테이블을 없으면 만든다.
func EnsureHistory(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, createHistoryTable); err != nil {
		return fmt.Errorf("이력 테이블 생성: %w", err)
	}
	return nil
}

// AppliedVersions는 이미 적용된 기록을 버전으로 색인해 돌려준다.
func AppliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int]Applied, error) {
	rows, err := pool.Query(ctx, `SELECT version, name, checksum FROM public.schema_migration`)
	if err != nil {
		return nil, fmt.Errorf("이력 조회: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]Applied)
	for rows.Next() {
		var a Applied
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum); err != nil {
			return nil, fmt.Errorf("이력 행 읽기: %w", err)
		}
		applied[a.Version] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("이력 조회 완료: %w", err)
	}
	return applied, nil
}

// Pending은 아직 적용되지 않은 마이그레이션을 돌려준다.
// 이미 적용된 파일이 뒤에 수정되었으면 오류로 알린다.
func Pending(migrations []Migration, applied map[int]Applied) ([]Migration, error) {
	var pending []Migration
	for _, m := range migrations {
		a, ok := applied[m.Version]
		if !ok {
			pending = append(pending, m)
			continue
		}
		if a.Checksum != m.Checksum {
			return nil, fmt.Errorf("버전 %d(%s)가 적용된 뒤 수정되었다", m.Version, m.Name)
		}
	}
	return pending, nil
}

// Apply는 마이그레이션 하나를 이력 기록과 같은 트랜잭션에서 적용한다.
func Apply(ctx context.Context, pool *pgxpool.Pool, m Migration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, m.Rendered); err != nil {
		return fmt.Errorf("버전 %d(%s) 적용: %w", m.Version, m.Name, err)
	}
	const insert = `INSERT INTO public.schema_migration (version, name, checksum) VALUES ($1, $2, $3)`
	if _, err := tx.Exec(ctx, insert, m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("버전 %d(%s) 이력 기록: %w", m.Version, m.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("버전 %d(%s) 커밋: %w", m.Version, m.Name, err)
	}
	return nil
}
