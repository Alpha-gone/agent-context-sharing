package store

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 구성마다 자신이 만든 DB만 사용하고 공유 시험 DB의 타입·인덱스를 바꾸지 않는다.
func newEmbeddingContractStore(t *testing.T, vectorType string, dimension int) *Store {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 임베딩 DB 모듈 시험을 건너뛴다")
	}
	admin, err := migrate.NewPool(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "data07_" + strings.ReplaceAll(newTestID(t).String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Errorf("합성 임베딩 DB 정리: %v", err)
		}
	})
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := migrate.NewPool(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := migrate.Config{GraphName: "agent_context", VectorType: vectorType, VectorDim: dimension, IndexTargetLayers: "all_layers"}
	for _, want := range []int{5, 0} {
		count := 0
		err := migrate.WithLock(t.Context(), pool, func(ctx context.Context, conn *pgxpool.Conn) error {
			loaded, applied, err := migrate.Prepare(ctx, conn, migrations.FS, ".", cfg)
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
				count++
			}
			return nil
		})
		if err != nil || count != want {
			t.Fatalf("정식 적용=%d, want=%d: %v", count, want, err)
		}
	}
	database, err := New(t.Context(), u.String(), cfg.GraphName, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	return database
}

type embeddingQueryTracer struct {
	mu   sync.Mutex
	sql  string
	args []any
}

func (tracer *embeddingQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "AS similarity") {
		tracer.mu.Lock()
		tracer.sql, tracer.args = data.SQL, slices.Clone(data.Args)
		tracer.mu.Unlock()
	}
	return ctx
}

func (*embeddingQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func assertEmbeddingHNSWPlan(t *testing.T, database *Store, tracer *embeddingQueryTracer) {
	t.Helper()
	tracer.mu.Lock()
	query, args := tracer.sql, slices.Clone(tracer.args)
	tracer.mu.Unlock()
	if query == "" {
		t.Fatal("실제 의미 검색 SQL을 관찰하지 못했다")
	}
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), tx)
	// 작은 표본에서도 타입 변환 때문에 인덱스 경로 자체가 막혔는지 구분한다.
	// 이 설정의 계획·지연을 운영 기본 선택이나 성능 측정으로 보고하지 않는다.
	if _, err := tx.Exec(t.Context(), "SET LOCAL enable_seqscan=off; SET LOCAL enable_sort=off"); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan struct{} `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("실행 계획 해석: %v", err)
	}
	for _, tier := range []string{"hot", "cold"} {
		if !strings.Contains(string(raw), `"Index Name": "context_embedding_`+tier+`_embedding_idx"`) {
			t.Fatalf("%s HNSW를 사용할 수 없다: %s", tier, raw)
		}
	}
	t.Log("실제 의미 검색 SQL에서 양쪽 HNSW 인덱스 경로를 확인했다")
}

func TestEmbeddingStorageModelsIntegration(t *testing.T) {
	for _, cfg := range []struct {
		typ string
		dim int
	}{{"vector", 768}, {"vector", 2000}, {"halfvec", 3072}, {"halfvec", 4000}} {
		t.Run(fmt.Sprintf("%s/%d", cfg.typ, cfg.dim), func(t *testing.T) {
			database := newEmbeddingContractStore(t, cfg.typ, cfg.dim)
			if err := database.CheckEmbeddingSchema(t.Context(), cfg.typ, cfg.dim); err != nil {
				t.Fatal(err)
			}
			if err := database.CheckEmbeddingSchema(t.Context(), cfg.typ, cfg.dim-1); err == nil {
				t.Fatal("다른 차원 구성을 허용했다")
			}
			tracer := new(embeddingQueryTracer)
			poolCfg := database.pool.Config()
			database.pool.Close()
			poolCfg.ConnConfig.Tracer = tracer
			pool, err := pgxpool.NewWithConfig(t.Context(), poolCfg)
			if err != nil {
				t.Fatal(err)
			}
			database.pool = pool
			actor := newTestID(t)
			createTestAccount(t, database, actor)
			graph := createTestGraph(t, database, actor)
			var nodes []model.Context
			for range 2 {
				value, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://data07/"+newTestID(t).String()), nil)
				if err != nil {
					t.Fatal(err)
				}
				nodes = append(nodes, value)
			}
			vectors := [][]float64{make([]float64, cfg.dim), make([]float64, cfg.dim)}
			vectors[0][0], vectors[1][1] = 1, 1
			for i, node := range nodes {
				readyIndexTasks(t, database, graph, node.ID)
				result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
					return IndexTaskResult{Embedding: vectors[i], ModelID: "data07-current"}
				})
				if err != nil || !result.Succeeded {
					t.Fatalf("저장=%+v: %v", result, err)
				}
			}
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET storage_tier='cold' WHERE context_id=$1`, nodes[0].ID.String()); err != nil {
				t.Fatal(err)
			}
			var tiers int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(DISTINCT storage_tier) FROM public.context_embedding WHERE graph_id=$1`, graph.String()).Scan(&tiers); err != nil || tiers != 2 {
				t.Fatalf("부모의 양쪽 파티션 조회=%d: %v", tiers, err)
			}
			for _, tier := range []string{"hot", "cold"} {
				_, err := database.pool.Exec(t.Context(), `INSERT INTO public.context_embedding
					SELECT * FROM public.context_embedding WHERE graph_id=$1 AND storage_tier=$2`, graph.String(), tier)
				failure, ok := errors.AsType[*pgconn.PgError](err)
				if !ok || failure.Code != "23505" || failure.ConstraintName != "context_embedding_"+tier+"_context_idx" {
					t.Fatalf("%s 파티션의 DB 유일성=%v", tier, err)
				}
			}
			candidates, err := database.SemanticCandidates(t.Context(), graph, "data07-current", vectors[0], time.Now().UTC(), 10)
			if err != nil || len(candidates) != 2 || candidates[0].Context.ID != nodes[0].ID || candidates[0].Similarity != 1 || candidates[1].Similarity != 0 {
				t.Fatalf("양쪽 파티션 코사인 검색=%+v: %v", candidates, err)
			}
			assertEmbeddingHNSWPlan(t, database, tracer)
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET storage_tier='cold' WHERE context_id=$1`, nodes[0].ID.String()); err != nil {
				t.Fatal(err)
			}
			if err := database.ReindexGraph(t.Context(), graph); err != nil {
				t.Fatal(err)
			}
			readyIndexTasks(t, database, graph, nodes[0].ID)
			result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
				return IndexTaskResult{Embedding: vectors[1], ModelID: "data07-replaced"}
			})
			if err != nil || !result.Succeeded {
				t.Fatalf("cold 교체=%+v: %v", result, err)
			}
			var count int
			var tier, modelID string
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*),min(storage_tier),min(model_id) FROM public.context_embedding WHERE context_id=$1`, nodes[0].ID.String()).Scan(&count, &tier, &modelID); err != nil || count != 1 || tier != "hot" || modelID != "data07-replaced" {
				t.Fatalf("교체 뒤 단일 현재 임베딩=%d/%s/%s: %v", count, tier, modelID, err)
			}
			before := embeddingStateFingerprint(t, database, graph)
			if err := database.ReindexGraph(t.Context(), graph); err != nil {
				t.Fatal(err)
			}
			readyIndexTasks(t, database, graph, nodes[0].ID)
			_, err = database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
				return IndexTaskResult{Embedding: []float64{1}, ModelID: "invalid-dimension"}
			})
			if err == nil || embeddingStateFingerprint(t, database, graph) != before || indexTaskCount(t, database, nodes[0].ID) != 1 {
				t.Fatalf("저장 실패의 벡터·내용 판·작업 롤백: %v", err)
			}
			candidates, err = database.SemanticCandidates(t.Context(), graph, "data07-replaced", vectors[1], time.Now().UTC(), 10)
			if err != nil || len(candidates) != 1 || candidates[0].Context.ID != nodes[0].ID {
				t.Fatalf("현재 모델 필터=%+v: %v", candidates, err)
			}
			foreignGraph := createTestGraph(t, database, actor)
			foreign, err := database.CreateContext(t.Context(), foreignGraph, testSourceContext(t, foreignGraph, actor, "api://data07/foreign/"+graph.String()), nil)
			if err != nil {
				t.Fatal(err)
			}
			// 다른 그래프 정점을 가리키는 행과 컨텍스트가 없는 고아 행은 가장 가까운
			// 벡터여도 후보에 포함되면 안 된다.
			for _, id := range []model.ID{foreign.ID, newTestID(t)} {
				if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.context_embedding
					(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
					VALUES ($1,$2,$3,'data07-replaced',now(),now(),'cold')`, id.String(), graph.String(), vectorText(vectors[1])); err != nil {
					t.Fatal(err)
				}
			}
			candidates, err = database.SemanticCandidates(t.Context(), graph, "data07-replaced", vectors[1], time.Now().UTC(), 10)
			if err != nil || len(candidates) != 1 || candidates[0].Context.ID != nodes[0].ID {
				t.Fatalf("다른 그래프·고아 벡터 배제=%+v: %v", candidates, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestEmbeddingConfigurationRejectsBeforeDial(t *testing.T) {
	for _, cfg := range []struct {
		typ string
		dim int
	}{
		{"bit", 128}, {"vector", 0}, {"vector", -1}, {"vector", 2001}, {"halfvec", 4001}, {"invalid", 128},
	} {
		t.Run(fmt.Sprintf("%s/%d", cfg.typ, cfg.dim), func(t *testing.T) {
			if err := new(Store).CheckEmbeddingSchema(t.Context(), cfg.typ, cfg.dim); err == nil {
				t.Fatal("지원하지 않는 벡터 구성을 DB 연결 전에 거부하지 않았다")
			}
		})
	}
}

func embeddingStateFingerprint(t *testing.T, database *Store, graph model.ID) string {
	t.Helper()
	var value string
	if err := database.pool.QueryRow(t.Context(), `SELECT md5(content_revision::text ||
		(SELECT string_agg(row_to_json(e)::text,'' ORDER BY context_id,storage_tier) FROM public.context_embedding e WHERE graph_id=$1))
		FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestEmbeddingSchemaRejectsIncompleteStorageIntegration(t *testing.T) {
	for name, sql := range map[string]string{
		"missing_cold":   `ALTER TABLE public.context_embedding DETACH PARTITION public.context_embedding_cold`,
		"missing_unique": `DROP INDEX public.context_embedding_cold_context_idx`,
		"missing_hnsw":   `DROP INDEX public.context_embedding_embedding_hnsw_idx`,
		"wrong_distance": `DROP INDEX public.context_embedding_embedding_hnsw_idx; CREATE INDEX ON public.context_embedding USING hnsw (embedding public.vector_l2_ops)`,
		"nullable_model": `ALTER TABLE public.context_embedding ALTER COLUMN model_id DROP NOT NULL`,
	} {
		t.Run(name, func(t *testing.T) {
			database := newEmbeddingContractStore(t, "vector", 8)
			if _, err := database.pool.Exec(t.Context(), sql); err != nil {
				t.Fatal(err)
			}
			if err := database.CheckEmbeddingSchema(t.Context(), "vector", 8); err == nil {
				t.Fatal("불완전한 임베딩 저장 형상을 수용했다")
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
