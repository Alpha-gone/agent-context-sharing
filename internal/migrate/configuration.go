package migrate

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
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
func Prepare(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir string, cfg Config) ([]Migration, map[int]Applied, error) {
	if err := EnsureHistory(ctx, pool); err != nil {
		return nil, nil, err
	}
	applied, err := AppliedVersions(ctx, pool)
	if err != nil {
		return nil, nil, err
	}
	current, err := Load(fsys, dir, cfg)
	if err != nil {
		return nil, nil, err
	}
	missing := make([]int, 0)
	for _, m := range current {
		if a, exists := applied[m.Version]; exists && a.Config == nil {
			missing = append(missing, m.Version)
		}
	}
	if len(missing) > 0 {
		original := cfg
		var actual, tablespace string
		if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod)
			FROM pg_attribute WHERE attrelid = 'public.context_embedding'::regclass
			AND attname = 'embedding' AND NOT attisdropped`).Scan(&actual); err != nil {
			return nil, nil, fmt.Errorf("기존 이력의 벡터 열 조회: %w", err)
		}
		match := embeddingTypePattern.FindStringSubmatch(actual)
		if match == nil {
			return nil, nil, fmt.Errorf("이력의 벡터 타입·차원을 복원할 수 없다")
		}
		original.VectorType = match[1]
		original.VectorDim, err = strconv.Atoi(match[2])
		if err != nil {
			return nil, nil, fmt.Errorf("이력 차원 복원 실패")
		}
		if err := pool.QueryRow(ctx, `SELECT COALESCE(t.spcname, '') FROM pg_class c
			LEFT JOIN pg_tablespace t ON t.oid = c.reltablespace
			WHERE c.oid = 'public.context_embedding_cold'::regclass`).Scan(&tablespace); err != nil {
			return nil, nil, fmt.Errorf("이력 tablespace 조회: %w", err)
		}
		original.ColdTablespace = tablespace
		for _, version := range missing {
			a := applied[version]
			a.Config = new(original)
			applied[version] = a
		}
	}
	loaded, err := LoadRecorded(fsys, dir, cfg, applied)
	if err != nil {
		return nil, nil, err
	}
	// 모든 파일이 검증된 뒤에만 당시 구성을 기록한다. checksum·적용 시각은 바꾸지 않는다.
	for _, version := range missing {
		encoded, err := json.Marshal(applied[version].Config)
		if err != nil {
			return nil, nil, err
		}
		if _, err := pool.Exec(ctx, `UPDATE public.schema_migration SET render_config=$2::jsonb
			WHERE version=$1 AND render_config IS NULL`, version, string(encoded)); err != nil {
			return nil, nil, fmt.Errorf("이력 치환 구성 보존: %w", err)
		}
	}
	return loaded, applied, nil
}
