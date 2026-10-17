package migrate_test

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"testing"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/migrations"
)

// TestConnectionRemovalMigrationScope는 제거 SQL이 연결 전용 테이블에만 한정되는지 확인한다.
func TestConnectionRemovalMigrationScope(t *testing.T) {
	data, err := migrations.FS.ReadFile("009_remove_legacy_connection.sql")
	if err != nil {
		t.Fatal(err)
	}
	var statements []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
			statements = append(statements, line)
		}
	}
	want := []string{
		"DROP TABLE IF EXISTS public.authorization_code;",
		"DROP TABLE IF EXISTS public.dpop_proof_replay;",
	}
	if strings.Join(statements, "\n") != strings.Join(want, "\n") {
		t.Fatalf("연결 제거 SQL 범위가 다르다: %q", statements)
	}
}

// TestConnectionRemovalMigrationIntegration은 새 DB의 제거·재적용과 공유 테이블 보존을 확인한다.
func TestConnectionRemovalMigrationIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 1024}
	loaded, err := migrate.Load(migrations.FS, ".", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	// 과거에 제거된 파일의 적용 번호가 남아 있어도 새 번호와 충돌하지 않아야 한다.
	if _, err := pool.Exec(t.Context(), "INSERT INTO public.schema_migration (version, name, checksum) VALUES (8, 'retired', 'retired')"); err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if err := migrate.Apply(t.Context(), pool, migration); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"authorization_code", "dpop_proof_replay"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&exists); err != nil || exists {
			t.Fatalf("제거 대상 %s가 남았다: exists=%t, err=%v", table, exists, err)
		}
	}
	for _, table := range []string{"account", "signing_key", "revoked_token", "context_graph", "idempotency_record"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&exists); err != nil || !exists {
			t.Fatalf("공유 테이블 %s가 없다: exists=%t, err=%v", table, exists, err)
		}
	}
	data, err := migrations.FS.ReadFile("009_remove_legacy_connection.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), string(data)); err != nil {
		t.Fatalf("제거 SQL 재적용: %v", err)
	}
	if err := migrate.WithLock(t.Context(), pool, func(ctx context.Context, locked *pgxpool.Conn) error {
		loaded, applied, err := migrate.Prepare(ctx, locked, migrations.FS, ".", cfg)
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(loaded, applied)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			return fmt.Errorf("다시 적용할 마이그레이션 = %d", len(pending))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
