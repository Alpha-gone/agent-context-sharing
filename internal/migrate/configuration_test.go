package migrate_test

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRecordedConfigurationPreservesChecksums(t *testing.T) {
	files := fstest.MapFS{
		"001_init.sql":                 {Data: []byte("SELECT '{{.VectorType}}({{.VectorDim}})';")},
		"007_embedding_dimensions.sql": {Data: []byte("SELECT '{{.VectorType}}({{.VectorDim}})';")},
	}
	original := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 1024}
	old, err := migrate.Load(files, ".", original)
	if err != nil {
		t.Fatal(err)
	}
	applied := map[int]migrate.Applied{1: {Version: 1, Name: old[0].Name, Checksum: old[0].Checksum, Config: new(original)}}
	target := original
	target.VectorDim = 768
	loaded, err := migrate.LoadRecorded(files, ".", target, applied)
	if err != nil || loaded[0].Checksum != old[0].Checksum || !strings.Contains(loaded[1].Rendered, "768") {
		t.Fatalf("기존 checksum·새 차원: %v", err)
	}
	files["001_init.sql"] = &fstest.MapFile{Data: []byte("SELECT 'changed';")}
	if _, err := migrate.LoadRecorded(files, ".", target, applied); err == nil {
		t.Fatal("기존 SQL 변경이 허용됐다")
	}
}

func TestMigrationTargetsAndHNSWLimits(t *testing.T) {
	for _, test := range []struct {
		target, vectorType string
		dimension          int
		valid              bool
	}{
		{"", "vector", 768, true}, {"all_layers", "vector", 1024, true},
		{"without_source", "halfvec", 3072, true},
		{"invalid'", "vector", 768, false}, {"all_layers", "vector", 3072, false},
		{"all_layers", "halfvec", 4001, false},
	} {
		cfg := migrate.Config{GraphName: "agent_context", VectorType: test.vectorType, VectorDim: test.dimension, IndexTargetLayers: test.target}
		if got := cfg.Validate() == nil; got != test.valid {
			t.Errorf("색인 대상·HNSW 구성 수용 = %t, 기대 %t", got, test.valid)
		}
	}
}

// isolatedMigrationDatabase는 이 시험이 만든 DB만 폐기한다. TEST_DATABASE_URL의
// 기존 DB는 연결·생성 권한의 경계일 뿐 스키마와 데이터는 변경하지 않는다.
func isolatedMigrationDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	admin := newTestPool(t)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "gemini_migration_" + strings.ReplaceAll(id.String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatalf("격리된 시험 DB 생성: %v", err)
	}
	address, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("시험 DB 주소 해석 실패")
	}
	address.Path = "/" + name
	pool, err := migrate.NewPool(t.Context(), address.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Errorf("이 시험이 만든 DB 정리 실패: %v", err)
		}
	})
	return pool, address.String()
}

func TestEmbeddingDimensionMigrationIntegration(t *testing.T) {
	pool, databaseURL := isolatedMigrationDatabase(t)
	original := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 1024}
	loaded, err := migrate.Load(migrations.FS, ".", original)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(t.Context(), pool, loaded[0]); err != nil {
		t.Fatal(err)
	}
	// 구형 이력의 복원 경로를 실제로 거친다.
	if _, err := pool.Exec(t.Context(), "UPDATE public.schema_migration SET render_config=NULL"); err != nil {
		t.Fatal(err)
	}
	database, err := store.New(t.Context(), databaseURL, original.GraphName, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	newID := func() model.ID {
		id, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	now := time.Now().UTC()
	actor, graphID := newID(), newID()
	if _, err := database.CreateGraph(t.Context(), model.Graph{ID: graphID, Name: "합성 시험 그래프", CreatedBy: actor, CreatedAt: now, LastActivityAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	value := model.Context{ID: newID(), GraphID: graphID, Layer: model.LayerSource, Body: "보존할 합성 본문", RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1,
		Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelWeb, Locator: "https://example.test/synthetic"}, OccurredAt: now, OriginKind: model.OriginKindExternalContent}}
	if _, err := database.CreateContext(t.Context(), graphID, value, nil); err != nil {
		t.Fatal(err)
	}
	// 앱과 DB의 미세한 시계 차이가 작업 확보를 불안정하게 하지 않도록 시험에서만
	// 현재 그래프의 대기 시각을 앞으로 당긴다.
	readyTasks := func() {
		t.Helper()
		if _, err := pool.Exec(t.Context(), "UPDATE public.index_task SET next_attempt_at=now()-interval '1 minute' WHERE graph_id=$1", graphID.String()); err != nil {
			t.Fatal(err)
		}
	}
	readyTasks()
	vector := make([]float64, 1024)
	vector[0] = 1
	result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, func(context.Context, store.IndexTask) store.IndexTaskResult {
		return store.IndexTaskResult{Embedding: vector, ModelID: "bge-m3:vector:1024"}
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("이전 벡터 저장: %+v, %v", result, err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE public.context_embedding SET storage_tier='cold'"); err != nil {
		t.Fatal(err)
	}
	assertState := func(dimension, embeddings, tasks int) {
		t.Helper()
		if err := database.CheckEmbeddingSchema(t.Context(), "vector", dimension); err != nil {
			t.Fatal(err)
		}
		for table, want := range map[string]int{"context_embedding": embeddings, "index_task": tasks} {
			var count int
			if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM public."+table).Scan(&count); err != nil || count != want {
				t.Fatalf("%s 행 %d, 기대 %d, 오류 %v", table, count, want, err)
			}
		}
		stored, err := database.Context(t.Context(), graphID, value.ID)
		if err != nil || stored.Body != value.Body {
			t.Fatalf("원본 본문 보존: %v", err)
		}
	}
	assertState(1024, 1, 0)
	target := original
	target.VectorDim = 768
	err = migrate.WithLock(t.Context(), pool, func(ctx context.Context) error {
		prepared, applied, err := migrate.Prepare(ctx, pool, migrations.FS, ".", target)
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(prepared, applied)
		if err != nil || len(pending) != 1 || pending[0].Version != 7 || applied[1].Checksum != loaded[0].Checksum {
			return fmt.Errorf("전환 목록·원래 checksum이 다르다: %v", err)
		}
		failed := pending[0]
		failed.Rendered += "; SELECT 1/0;"
		if err := migrate.Apply(ctx, pool, failed); err == nil {
			return fmt.Errorf("실패 주입이 성공했다")
		}
		assertState(1024, 1, 0)
		if err := migrate.Apply(ctx, pool, pending[0]); err != nil {
			return err
		}
		prepared, applied, err = migrate.Prepare(ctx, pool, migrations.FS, ".", target)
		if err != nil {
			return err
		}
		pending, err = migrate.Pending(prepared, applied)
		if err != nil || len(pending) != 0 {
			return fmt.Errorf("반복 실행이 새 전환을 만들었다: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertState(768, 0, 1)
	if err := database.CheckEmbeddingSchema(t.Context(), "vector", 1024); err == nil {
		t.Fatal("새 DB와 이전 구성이 맞는 것으로 판정됐다")
	}
	vector = make([]float64, 768)
	vector[0] = 1
	var providerStatus atomic.Int64
	providerStatus.Store(200)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("x-goog-api-key") != "synthetic-key" {
			t.Error("Gemini 키 연결이 다르다")
		}
		if providerStatus.Load() != 200 {
			writer.WriteHeader(int(providerStatus.Load()))
			_, _ = writer.Write([]byte("synthetic-key private-body"))
			return
		}
		body, _ := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
		_, _ = writer.Write(body)
	}))
	defer provider.Close()
	baseURL, _ := url.Parse(provider.URL)
	worker, err := index.New(database, index.Config{Provider: "gemini", APIKey: "synthetic-key", BaseURL: baseURL, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 768}, provider.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.RunOnceInGraph(t.Context(), graphID)
	if err != nil || !processed {
		t.Fatalf("768차원 재색인: %v", err)
	}
	assertState(768, 1, 0)
	var indexes int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname='context_embedding_embedding_hnsw_idx'").Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("HNSW 재생성: %v", err)
	}
	if count, err := database.OutdatedEmbeddingCount(t.Context(), "gemini:gemini-embedding-2:retrieval-v1:vector:768"); err != nil || count != 0 {
		t.Fatalf("재색인 완료 판정: %v", err)
	}
	searcher, err := search.New(database, worker, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: search.ConsistencySnapshot}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := search.Input{GraphID: graphID, WorkContext: "합성", Budget: 1000, MaxHops: 1, MaxHopNodes: 10}
	flow, err := searcher.Flow(t.Context(), input)
	if err != nil || flow.Channels["semantic"].Failure != "" || len(flow.Contexts) != 1 {
		t.Fatalf("768차원 검색: %v", err)
	}
	providerStatus.Store(429)
	flow, err = searcher.Flow(t.Context(), input)
	if err != nil || flow.Channels["semantic"].Failure != "embedding_unavailable" || len(flow.Contexts) != 1 {
		t.Fatalf("Gemini 장애의 부분 검색: %v", err)
	}
	for _, status := range []int{429, 400} {
		providerStatus.Store(int64(status))
		if err := database.ReindexGraph(t.Context(), graphID); err != nil {
			t.Fatal(err)
		}
		readyTasks()
		if processed, err := worker.RunOnceInGraph(t.Context(), graphID); err != nil || !processed {
			t.Fatalf("장애의 작업 처리: %v", err)
		}
		var attempts int
		var state, lastError string
		if err := pool.QueryRow(t.Context(), "SELECT attempts,state,last_error FROM public.index_task WHERE context_id=$1", value.ID.String()).Scan(&attempts, &state, &lastError); err != nil {
			t.Fatal(err)
		}
		want := "pending"
		if status == 400 {
			want = "failed"
		}
		if attempts != 1 || state != want || strings.Contains(lastError, "synthetic-key") || strings.Contains(lastError, "private-body") {
			t.Fatalf("장애의 재시도·비밀 경계: 시도 %d 상태 %s", attempts, state)
		}
	}
	// 이전 모델 복귀도 이미 적용한 007을 고치지 않는다. 같은 전환 계약을 다음 번호의
	// 시험용 파일로 적용해 과거 치환 구성과 checksum이 계속 보존되는지 확인한다.
	rollbackFiles := make(fstest.MapFS)
	for _, name := range []string{"001_init.sql", "007_embedding_dimensions.sql"} {
		data, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		rollbackFiles[name] = &fstest.MapFile{Data: data}
	}
	rollbackFiles["008_restore_embedding_dimensions.sql"] = rollbackFiles["007_embedding_dimensions.sql"]
	err = migrate.WithLock(t.Context(), pool, func(ctx context.Context) error {
		loaded, applied, err := migrate.Prepare(ctx, pool, rollbackFiles, ".", original)
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(loaded, applied)
		if err != nil || len(pending) != 1 || pending[0].Version != 8 {
			return fmt.Errorf("다음 번호 마이그레이션 복귀 목록이 다르다: %v", err)
		}
		return migrate.Apply(ctx, pool, pending[0])
	})
	if err != nil {
		t.Fatal(err)
	}
	assertState(1024, 0, 1)
	oldProvider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/embed" || request.Header.Get("x-goog-api-key") != "" {
			t.Error("Ollama 복귀의 endpoint·인증 분리가 다르다")
		}
		values := make([]float64, 1024)
		values[0] = 1
		body, _ := json.Marshal(map[string]any{"embeddings": [][]float64{values}})
		_, _ = writer.Write(body)
	}))
	defer oldProvider.Close()
	oldURL, _ := url.Parse(oldProvider.URL)
	oldWorker, err := index.New(database, index.Config{Provider: "ollama", BaseURL: oldURL, Model: "bge-m3", VectorType: "vector", Dimension: 1024}, oldProvider.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	readyTasks()
	if processed, err := oldWorker.RunOnceInGraph(t.Context(), graphID); err != nil || !processed {
		t.Fatalf("Ollama 복귀 재색인: %v", err)
	}
	assertState(1024, 1, 0)
	if count, err := database.OutdatedEmbeddingCount(t.Context(), oldWorker.ModelID()); err != nil || count != 0 {
		t.Fatalf("Ollama 복귀 식별자: %v", err)
	}
	// 차원 전환이 원천 제외 평가의 등록 계약을 우회하지 않아야 한다.
	if err := database.ReindexGraph(t.Context(), graphID); err != nil {
		t.Fatal(err)
	}
	rollbackFiles["009_without_source_dimensions.sql"] = rollbackFiles["007_embedding_dimensions.sql"]
	target.IndexTargetLayers = "without_source"
	err = migrate.WithLock(t.Context(), pool, func(ctx context.Context) error {
		loaded, applied, err := migrate.Prepare(ctx, pool, rollbackFiles, ".", target)
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(loaded, applied)
		if err != nil || len(pending) != 1 || pending[0].Version != 9 {
			return fmt.Errorf("원천 제외 전환 목록이 다르다: %v", err)
		}
		return migrate.Apply(ctx, pool, pending[0])
	})
	if err != nil {
		t.Fatal(err)
	}
	assertState(768, 0, 0)
}
