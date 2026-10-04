package migrate_test

import (
	"testing"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/migrations"
)

func TestAuthorizationCodeAuthenticationTimeMigrationIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	config := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768}
	loaded, err := migrate.Load(migrations.FS, ".", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	// 001 형식의 코드가 존재하는 상태에서 008을 적용한다.
	for _, migration := range loaded {
		if migration.Version == 8 {
			break
		}
		if err := migrate.Apply(t.Context(), pool, migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.account (account_id, login_id, password_hash, created_at)
		VALUES ('019a0000-0000-7000-8000-000000000055', 'migrationtest', 'synthetic', now());
		INSERT INTO public.authorization_code
		(code_hash, client_id, account_id, redirect_uri, code_challenge, resource, issued_at, expires_at)
		VALUES ('legacy', 'client', '019a0000-0000-7000-8000-000000000055', 'http://127.0.0.1/callback', 'challenge', 'https://service.test/mcp', now(), now()+interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	var applied bool
	for _, migration := range loaded {
		if migration.Version != 8 {
			continue
		}
		if err := migrate.Apply(t.Context(), pool, migration); err != nil {
			t.Fatal(err)
		}
		// 실행기 이력뿐 아니라 SQL 자체도 다시 적용할 수 있는지 확인한다.
		if _, err := pool.Exec(t.Context(), migration.Rendered); err != nil {
			t.Fatal(err)
		}
		applied = true
	}
	if !applied {
		t.Fatal("008 마이그레이션이 없다")
	}
	var authenticatedAt *time.Time
	if err := pool.QueryRow(t.Context(), "SELECT authenticated_at FROM public.authorization_code WHERE code_hash='legacy'").Scan(&authenticatedAt); err != nil || authenticatedAt != nil {
		t.Fatalf("구형 코드의 최초 인증 시각이 추정됐다: %v", err)
	}
	var nullable, dataType string
	if err := pool.QueryRow(t.Context(), `SELECT is_nullable, data_type FROM information_schema.columns
		WHERE table_schema='public' AND table_name='authorization_code' AND column_name='authenticated_at'`).Scan(&nullable, &dataType); err != nil || nullable != "YES" || dataType != "timestamp with time zone" {
		t.Fatalf("추가 열의 계약 = %s, %s, %v", nullable, dataType, err)
	}
	loaded, history, err := migrate.Prepare(t.Context(), pool, migrations.FS, ".", config)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := migrate.Pending(loaded, history)
	if err != nil || len(pending) != 0 {
		t.Fatalf("두 번째 적용의 남은 파일 = %d, %v", len(pending), err)
	}
}
