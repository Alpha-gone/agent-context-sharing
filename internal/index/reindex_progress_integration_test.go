package index

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newIndexContractDatabase(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("필수 시험 DB 주소가 없다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 재색인 진행 시험을 건너뛴다")
	}
	admin, err := migrate.NewPool(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "idx07_" + strings.ReplaceAll(integrationID(t).String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Errorf("진행 시험 DB 정리: %v", err)
		}
	})
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	u.Path, u.RawPath = "/"+name, ""
	// dbname·database 옵션이 새 경로를 덮어쓰지 않도록 제거한다.
	queryOptions := u.Query()
	queryOptions.Del("dbname")
	queryOptions.Del("database")
	u.RawQuery = queryOptions.Encode()
	pool, err := migrate.NewPool(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var actual string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&actual); err != nil || actual != name {
		t.Fatal("실제 연결이 시험 전용 DB가 아니므로 마이그레이션을 중단한다")
	}
	err = migrate.WithLock(t.Context(), pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		loaded, applied, err := migrate.Prepare(ctx, conn, migrations.FS, ".", migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 128, IndexTargetLayers: "all_layers"})
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(loaded, applied)
		if err != nil {
			return err
		}
		for _, item := range pending {
			if err := migrate.Apply(ctx, conn, item); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.New(t.Context(), u.String(), "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	return database, pool
}

func TestWorkerReindexProgressWithoutOldVectorsIntegration(t *testing.T) {
	database, _ := newIndexContractDatabase(t)
	_, _, _, err := createSimilarEventGraph(t, database, integrationID(t))
	if err != nil {
		t.Fatal(err)
	}
	baseURL, _ := url.Parse("https://example.test")
	var output bytes.Buffer
	worker, err := New(database, Config{Provider: "gemini", APIKey: testGeminiKey, BaseURL: baseURL, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 128}, nil, slog.New(slog.NewJSONHandler(&output, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close(context.WithoutCancel(t.Context()))
	worker.logReindexProgress(t.Context())
	var event struct {
		Old     int `json:"old_model_embeddings_remaining"`
		Missing int `json:"missing_current_embeddings"`
		Pending int `json:"pending"`
		Failed  int `json:"failed"`
	}
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Old != 0 || event.Missing != 3 || event.Pending != 3 || event.Failed != 0 {
		t.Fatalf("옛 벡터 없는 진행 진단=%+v", event)
	}
}
