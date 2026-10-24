package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationDatabaseURL(t testing.TB) string {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 통합 테스트를 건너뛴다")
	}
	return address
}

func integrationGraphName() string {
	if name := os.Getenv("AGE_GRAPH_NAME"); name != "" {
		return name
	}
	return "agent_context"
}

func newSigningKeyIntegrationStore(t *testing.T) *Store {
	t.Helper()
	return newIsolatedIntegrationStore(t)
}

// 공유 DB는 생성 권한의 경계로만 사용한다. DB 생성 직후 정리를 등록해 이후의
// 연결·마이그레이션·시험 실패와 시험 컨텍스트 취소에도 자신이 만든 DB만 제거한다.
func newIsolatedIntegrationStore(t testing.TB) *Store {
	t.Helper()
	config, err := pgxpool.ParseConfig(integrationDatabaseURL(t))
	if err != nil {
		t.Fatal("시험 DB 연결 설정을 해석하지 못했다")
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("시험 DB 관리 연결을 준비하지 못했다")
	}
	t.Cleanup(admin.Close)
	name := "store_test_" + strings.ReplaceAll(newTestID(t).String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal("시험 전용 DB를 만들지 못했다(DB 생성 권한 필요)")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Error("시험 전용 DB를 정리하지 못했다")
		}
	})
	// 이미 해석한 설정의 DB만 바꾼다. URL 경로만 바꾸면 dbname 질의 옵션이
	// 경로를 다시 덮어쓸 수 있으므로 원래 접속 문자열을 재사용하지 않는다.
	isolated := config.Copy()
	isolated.ConnConfig.Database = name
	isolated.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		var actual string
		if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&actual); err != nil || actual != name {
			return fmt.Errorf("실제 연결이 시험 전용 DB가 아니다")
		}
		for _, statement := range migrate.AfterConnectSQL {
			if _, err := conn.Exec(ctx, statement); err != nil {
				return fmt.Errorf("시험 연결 준비: %w", err)
			}
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), isolated)
	if err != nil {
		t.Fatal("시험 전용 연결 풀을 준비하지 못했다")
	}
	t.Cleanup(pool.Close)
	graphName := integrationGraphName()
	loaded, err := migrate.Load(migrations.FS, ".", migrate.Config{
		GraphName: graphName, VectorType: "vector", VectorDim: 768,
	})
	if err != nil {
		t.Fatal("시험 전용 마이그레이션을 읽지 못했다")
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal("시험 전용 마이그레이션 이력을 준비하지 못했다")
	}
	for _, migration := range loaded {
		if err := migrate.Apply(t.Context(), pool, migration); err != nil {
			t.Fatal("시험 전용 마이그레이션을 적용하지 못했다")
		}
	}
	reservations, err := newReservations(isolated.MaxConns)
	if err != nil {
		t.Fatal("시험 전용 연결 예약을 준비하지 못했다")
	}
	return &Store{pool: pool, graphName: graphName, indexTargets: IndexTargetsAllLayers, reservations: reservations}
}
