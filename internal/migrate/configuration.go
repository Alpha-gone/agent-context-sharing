package migrate

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
)

// LoadRecorded는 과거 파일은 적용 당시 구성, 미적용 파일은 현재 구성으로 치환한다.
// 파일 변경 검사는 계속 당시의 렌더링 checksum으로 수행한다.
func LoadRecorded(fsys fs.FS, dir string, cfg Config, applied map[int]Applied) ([]Migration, error) {
	loaded, err := Load(fsys, dir, cfg)
	if err != nil {
		return nil, err
	}
	for i, current := range loaded {
		a, exists := applied[current.Version]
		if !exists {
			continue
		}
		if a.Config == nil {
			return nil, fmt.Errorf("버전 %d의 원래 치환 구성 복원이 필요하다", a.Version)
		}
		original, err := Load(fsys, dir, *a.Config)
		if err != nil {
			return nil, err
		}
		for _, m := range original {
			if m.Version == current.Version {
				loaded[i] = m
				break
			}
		}
	}
	if _, err := Pending(loaded, applied); err != nil {
		return nil, err
	}
	return loaded, nil
}

var embeddingTypePattern = regexp.MustCompile(`^(vector|halfvec)\(([0-9]+)\)$`)

// Prepare는 자문 잠금 안에서 이력의 치환 구성을 복원·검증하고 적용 목록을 만든다.
// 오래된 이력의 checksum이 실제 파일과 맞지 않으면 기록하거나 자동 수용하지 않는다.
func Prepare(ctx context.Context, db database, fsys fs.FS, dir string, cfg Config) ([]Migration, map[int]Applied, error) {
	current, err := Load(fsys, dir, cfg)
	if err != nil {
		return nil, nil, err
	}

	if err := EnsureHistory(ctx, db); err != nil {
		return nil, nil, err
	}
	applied, err := AppliedVersions(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	missing, err := restoreConfigurations(ctx, db, current, cfg, applied)
	if err != nil {
		return nil, nil, err
	}
	loaded, err := LoadRecorded(fsys, dir, cfg, applied)
	if err != nil {
		return nil, nil, err
	}
	// 모든 파일이 검증된 뒤에만 당시 구성을 기록한다. checksum·적용 시각은 바꾸지 않는다.
	if err := recordConfigurations(ctx, db, missing, applied); err != nil {
		return nil, nil, err
	}
	return loaded, applied, nil
}

// Inspect는 이력 테이블을 만들거나 구형 이력의 구성을 기록하지 않고 적용 목록을 계산한다.
// status 명령이 자문 잠금 없이 읽기 전용 트랜잭션에서 사용하므로 진행 중인 up을 기다리지
// 않고 읽기 전용 역할로도 실행할 수 있다. 구형 이력은 Prepare와 같은 방식으로 메모리에서만
// 복원해 검증한다.
func Inspect(ctx context.Context, db database, fsys fs.FS, dir string, cfg Config) ([]Migration, map[int]Applied, error) {
	current, err := Load(fsys, dir, cfg)
	if err != nil {
		return nil, nil, err
	}
	var exists, configured bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('public.schema_migration') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('public.schema_migration')
			AND attname = 'render_config' AND NOT attisdropped)`).Scan(&exists, &configured); err != nil {
		return nil, nil, fmt.Errorf("이력 테이블 확인: %w", err)
	}
	if !exists {
		return current, map[int]Applied{}, nil
	}
	applied, err := appliedVersions(ctx, db, configured)
	if err != nil {
		return nil, nil, err
	}
	if _, err := restoreConfigurations(ctx, db, current, cfg, applied); err != nil {
		return nil, nil, err
	}
	loaded, err := LoadRecorded(fsys, dir, cfg, applied)
	if err != nil {
		return nil, nil, err
	}
	return loaded, applied, nil
}

// restoreConfigurations는 치환 구성이 없는 구형 이력을 DB 열·tablespace로 복원해 applied에
// 채우고 복원한 버전을 돌려준다. 이력에는 쓰지 않는다.
func restoreConfigurations(ctx context.Context, db database, current []Migration, cfg Config, applied map[int]Applied) ([]int, error) {
	missing := make([]int, 0)
	for _, m := range current {
		if a, exists := applied[m.Version]; exists && a.Config == nil {
			missing = append(missing, m.Version)
		}
	}
	if len(missing) == 0 {
		return missing, nil
	}
	original := cfg
	var actual, tablespace string
	if err := db.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod)
		FROM pg_attribute WHERE attrelid = 'public.context_embedding'::regclass
		AND attname = 'embedding' AND NOT attisdropped`).Scan(&actual); err != nil {
		return nil, fmt.Errorf("기존 이력의 벡터 열 조회: %w", err)
	}
	match := embeddingTypePattern.FindStringSubmatch(actual)
	if match == nil {
		return nil, fmt.Errorf("이력의 벡터 타입·차원을 복원할 수 없다")
	}
	original.VectorType = match[1]
	dimension, err := strconv.Atoi(match[2])
	if err != nil {
		return nil, fmt.Errorf("이력 차원 복원 실패")
	}
	original.VectorDim = dimension
	if err := db.QueryRow(ctx, `SELECT COALESCE(t.spcname, '') FROM pg_class c
		LEFT JOIN pg_tablespace t ON t.oid = c.reltablespace
		WHERE c.oid = 'public.context_embedding_cold'::regclass`).Scan(&tablespace); err != nil {
		return nil, fmt.Errorf("이력 tablespace 조회: %w", err)
	}
	original.ColdTablespace = tablespace
	for _, version := range missing {
		a := applied[version]
		a.Config = new(original)
		applied[version] = a
	}
	return missing, nil
}

// 여러 구형 이력 중 하나라도 기록에 실패하면 보충한 구성 전체를 되돌린다.
func recordConfigurations(ctx context.Context, db database, missing []int, applied map[int]Applied) (err error) {
	if len(missing) == 0 {
		return nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("이력 구성 트랜잭션 시작: %w", err)
	}
	defer rollbackMigration(ctx, tx, &err)
	for _, version := range missing {
		encoded, err := json.Marshal(applied[version].Config)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE public.schema_migration SET render_config=$2::jsonb
			WHERE version=$1 AND render_config IS NULL`, version, string(encoded)); err != nil {
			return fmt.Errorf("이력 치환 구성 보존: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("이력 구성 커밋: %w", err)
	}
	return nil
}
