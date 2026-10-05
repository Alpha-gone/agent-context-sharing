package migrate_test

import (
	"testing"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/migrations"
)

// TestAuthorizationCodeAuthenticationTimeColumnIntegration은 001이 인가 코드의 최초 인증
// 시각 열을 nullable timestamptz로 만들고 두 번째 실행에 남는 파일이 없는지 확인한다.
func TestAuthorizationCodeAuthenticationTimeColumnIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	config := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768}
	loaded, err := migrate.Load(migrations.FS, ".", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if err := migrate.Apply(t.Context(), pool, migration); err != nil {
			t.Fatal(err)
		}
	}
	var nullable, dataType string
	if err := pool.QueryRow(t.Context(), `SELECT is_nullable, data_type FROM information_schema.columns
		WHERE table_schema='public' AND table_name='authorization_code' AND column_name='authenticated_at'`).Scan(&nullable, &dataType); err != nil || nullable != "YES" || dataType != "timestamp with time zone" {
		t.Fatalf("최초 인증 시각 열의 계약 = %s, %s, %v", nullable, dataType, err)
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
