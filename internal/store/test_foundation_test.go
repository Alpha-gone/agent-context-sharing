package store

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIntegrationDatabasePolicy(t *testing.T) {
	if os.Getenv("ACS_TEST_FOUNDATION_CHILD") == "policy" {
		integrationDatabaseURL(t)
		return
	}
	for _, test := range []struct {
		name, address, required string
		fail, skip              bool
	}{
		{name: "optional missing skips", skip: true},
		{name: "required missing fails", required: "1", fail: true},
		{name: "nonempty required also fails", required: "0", fail: true},
		{name: "configured returns address", address: "postgres://synthetic.invalid/test", required: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := foundationSubprocess(t, "TestIntegrationDatabasePolicy",
				"ACS_TEST_FOUNDATION_CHILD=policy", "TEST_DATABASE_URL="+test.address,
				"TEST_DATABASE_REQUIRED="+test.required)
			if (err != nil) != test.fail || strings.Contains(output, "--- SKIP:") != test.skip {
				t.Fatalf("DB 환경 판정: fail=%t skip=%t, want fail=%t skip=%t\n%s",
					err != nil, strings.Contains(output, "--- SKIP:"), test.fail, test.skip, output)
			}
		})
	}
}

func TestIsolatedIntegrationStoreLifecycleIntegration(t *testing.T) {
	admin := foundationAdmin(t)
	var sharedName string
	if err := admin.QueryRow(t.Context(), "SELECT current_database()").Scan(&sharedName); err != nil {
		t.Fatal("관리 연결의 DB를 확인하지 못했다")
	}
	var tablesBefore int
	const tableCount = "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'"
	if err := admin.QueryRow(t.Context(), tableCount).Scan(&tablesBefore); err != nil {
		t.Fatal(err)
	}
	type observation struct {
		name string
		ctx  context.Context
	}
	observed := make(chan observation, 2)
	t.Run("parallel fixtures", func(t *testing.T) {
		for _, name := range []string{"first", "second"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				database := newIsolatedIntegrationStore(t)
				var actual string
				if err := database.pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&actual); err != nil || actual == sharedName {
					t.Fatal("시험 전용 DB로 분리되지 않았다")
				}
				actor := newTestID(t)
				createTestAccount(t, database, actor)
				graph := createTestGraph(t, database, actor)
				if _, err := database.Graph(t.Context(), graph); err != nil {
					t.Fatal("정식 마이그레이션 뒤 그래프를 읽지 못했다")
				}
				observed <- observation{name: actual, ctx: t.Context()}
			})
		}
	})
	close(observed)
	names := make(map[string]bool)
	for item := range observed {
		if names[item.name] || item.ctx.Err() == nil {
			t.Fatal("시험 DB 이름이 겹치거나 시험 컨텍스트가 취소되지 않았다")
		}
		names[item.name] = true
		assertFoundationDatabaseRemoved(t, admin, item.name)
	}
	if len(names) != 2 {
		t.Fatalf("관찰한 시험 DB = %d, want 2", len(names))
	}
	var tablesAfter int
	if err := admin.QueryRow(t.Context(), tableCount).Scan(&tablesAfter); err != nil || tablesBefore != tablesAfter {
		t.Fatal("공유 DB의 스키마가 변경됐다")
	}
}

func TestIsolatedIntegrationStoreFailureCleanupIntegration(t *testing.T) {
	if os.Getenv("ACS_TEST_FOUNDATION_CHILD") == "failure" {
		database := newIsolatedIntegrationStore(t)
		var name string
		if err := database.pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Log("FIXTURE_DATABASE=" + name)
		t.Fatal("intentional fixture failure")
	}
	admin := foundationAdmin(t)
	output, err := foundationSubprocess(t, "TestIsolatedIntegrationStoreFailureCleanupIntegration", "ACS_TEST_FOUNDATION_CHILD=failure")
	if err == nil || !strings.Contains(output, "intentional fixture failure") {
		t.Fatalf("의도한 시험 실패까지 도달하지 못했다: %v\n%s", err, output)
	}
	_, rest, found := strings.Cut(output, "FIXTURE_DATABASE=")
	if !found {
		t.Fatal("실패한 시험의 DB 식별자가 없다")
	}
	name, _, _ := strings.Cut(rest, "\n")
	assertFoundationDatabaseRemoved(t, admin, strings.TrimSpace(name))
}

func foundationAdmin(t *testing.T) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(integrationDatabaseURL(t))
	if err != nil {
		t.Fatal("시험 DB 관리 설정 해석에 실패했다")
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("시험 DB 관리 풀 생성에 실패했다")
	}
	t.Cleanup(admin.Close)
	return admin
}

func assertFoundationDatabaseRemoved(t *testing.T, admin *pgxpool.Pool, name string) {
	t.Helper()
	var exists bool
	if err := admin.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil || exists {
		t.Fatalf("시험 DB가 정리되지 않았다: %s", name)
	}
}

func foundationSubprocess(t *testing.T, test string, environment ...string) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("시험 실행 파일을 찾지 못했다")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^"+test+"$", "-test.v")
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	return string(output), err
}
