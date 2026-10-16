package migrate_test

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/migrations"
)

func TestQueryStatisticsUpgradeIntegration(t *testing.T) {
	_, address := isolatedMigrationDatabase(t)
	pool := singleConnectionPool(t, address)
	cfg := targetConfig()
	previous := fstest.MapFS{}
	for _, name := range []string{"001_init.sql", "009_remove_legacy_connection.sql", "010_target_schema.sql"} {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: body}
	}
	if count, err := applyTarget(t.Context(), pool, previous, cfg); err != nil || count != 3 {
		t.Fatalf("이전 형상 준비=%d: %v", count, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.revoked_token
		SELECT 'synthetic-' || n, '2030-01-01'::timestamptz + n * interval '1 second'
		FROM generate_series(1,1000) AS n`); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM public.schema_migration AS m`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{1, 0} {
		if count, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err != nil || count != want {
			t.Fatalf("추가·반복 적용=%d want=%d: %v", count, want, err)
		}
	}
	var after string
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM public.schema_migration AS m WHERE version < 11`).Scan(&after); err != nil || before != after {
		t.Fatalf("기존 이력 변경: %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.revoked_token
		WHERE token_id='synthetic-' || extract(epoch FROM (expires_at-'2030-01-01'::timestamptz))::int`).Scan(&count); err != nil || count != 1000 {
		t.Fatalf("기존 토큰·만료 보존=%d: %v", count, err)
	}
	var analyzed float64
	if err := pool.QueryRow(t.Context(), `SELECT reltuples FROM pg_class WHERE oid='public.revoked_token'::regclass`).Scan(&analyzed); err != nil || analyzed != 1000 {
		t.Fatalf("같은 적용의 통계=%g: %v", analyzed, err)
	}
	var valid bool
	if err := pool.QueryRow(t.Context(), `SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid='public.revoked_token_expires_idx'::regclass`).Scan(&valid); err != nil || !valid {
		t.Fatalf("만료 인덱스 준비=%t: %v", valid, err)
	}
	files, err := migrate.Load(migrations.FS, ".", cfg)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	if _, err := tx.Exec(t.Context(), files[len(files)-1].Rendered); err != nil {
		t.Fatalf("011 SQL 재실행: %v", err)
	}
}
