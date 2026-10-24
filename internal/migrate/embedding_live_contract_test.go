package migrate_test

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
)

// liveEmbeddingTransport는 시험에서 실수로 반복 호출해도 세 번을 넘겨 전송하지 않는다.
type liveEmbeddingTransport struct {
	next  http.RoundTripper
	calls atomic.Int64
}

func validateLiveDatabaseAddress(address string) error {
	admin, err := url.Parse(address)
	if err != nil || admin == nil || (admin.Scheme != "postgres" && admin.Scheme != "postgresql") || admin.Fragment != "" || admin.Opaque != "" ||
		!slices.Contains([]string{"127.0.0.1", "localhost", "::1"}, admin.Hostname()) {
		return fmt.Errorf("격리 실연결 시험에는 루프백 주소의 TEST_DATABASE_URL이 필요하다")
	}
	parameters, err := url.ParseQuery(admin.RawQuery)
	if err != nil {
		return fmt.Errorf("시험 DB 주소의 질의 구성이 올바르지 않다")
	}
	for name, values := range parameters {
		if name != "sslmode" || len(values) != 1 {
			return fmt.Errorf("시험 DB 주소는 단일 sslmode 외의 질의 옵션을 허용하지 않는다")
		}
	}
	return nil
}

func TestLiveDatabaseAddressBoundary(t *testing.T) {
	for _, test := range []struct {
		name, address string
		valid         bool
	}{
		{"ipv4", "postgres://synthetic:synthetic@127.0.0.1:5432/test?sslmode=disable", true},
		{"ipv6", "postgresql://[::1]:5432/test?sslmode=require", true},
		{"localhost", "postgres://localhost/test", true},
		{"missing", "", false}, {"remote", "postgres://example.test/test", false},
		{"socket", "postgres:///test?host=/tmp", false},
		{"host_override", "postgres://127.0.0.1/test?host=example.test", false},
		{"service", "postgres://127.0.0.1/test?service=synthetic", false},
		{"malformed_query", "postgres://127.0.0.1/test?sslmode=%xx", false},
		{"fragment", "postgres://127.0.0.1/test#synthetic", false},
		{"duplicate_sslmode", "postgres://127.0.0.1/test?sslmode=require&sslmode=disable", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateLiveDatabaseAddress(test.address); (err == nil) != test.valid {
				t.Fatalf("DB 주소 허용=%t, 기대=%t", err == nil, test.valid)
			}
		})
	}
}

// 태그 안 실연결과 같은 색인·검색·호출 상한·정리 경로를 합성 TLS 제공자로 실행한다.
func TestSyntheticLiveIndexSearchIntegration(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("x-goog-api-key") != "synthetic-key" {
			t.Error("합성 키 전달이 다르다")
		}
		vector := make([]float64, 768)
		vector[0] = 1
		if n == 2 {
			vector[0], vector[1] = 0, 1
		}
		raw, err := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	exerciseLiveIndexSearch(t, "synthetic-key", address, server.Client().Transport)
	if calls.Load() != 3 {
		t.Fatalf("합성 TLS 호출=%d, 기대=3", calls.Load())
	}
}

func (transport *liveEmbeddingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.calls.Add(1) > 3 {
		return nil, fmt.Errorf("실연결 시험의 최대 세 번 호출을 넘었다")
	}
	return transport.next.RoundTrip(request)
}

func TestLiveEmbeddingTransportBudget(t *testing.T) {
	var received atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	transport := &liveEmbeddingTransport{next: http.DefaultTransport}
	for i := range 4 {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := transport.RoundTrip(request)
		if i == 3 {
			if err == nil || response != nil {
				t.Fatal("네 번째 제공자 요청이 차단되지 않았다")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
	}
	if received.Load() != 3 {
		t.Fatalf("대역이 받은 요청 수 = %d, 기대 3", received.Load())
	}
}

// liveQueryRecorder는 실제 검색에 사용한 질의 벡터를 DB 순위 검사에 재사용한다.
type liveQueryRecorder struct {
	*index.Worker
	vector []float64
}

func (recorder *liveQueryRecorder) Embed(ctx context.Context, input string) ([]float64, error) {
	vector, err := recorder.Worker.Embed(ctx, input)
	recorder.vector = vector
	return vector, err
}

func exerciseLiveIndexSearch(t *testing.T, key string, address *url.URL, next http.RoundTripper) {
	t.Helper()
	if closer, ok := next.(interface{ CloseIdleConnections() }); ok {
		t.Cleanup(closer.CloseIdleConnections)
	}
	pool, databaseURL := isolatedMigrationDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 110*time.Second)
	defer cancel()
	cfg := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768, IndexTargetLayers: "all_layers"}
	if err := migrate.WithLock(ctx, pool, func(ctx context.Context, locked *pgxpool.Conn) error {
		loaded, applied, err := migrate.Prepare(ctx, locked, migrations.FS, ".", cfg)
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(loaded, applied)
		if err != nil {
			return err
		}
		for _, migration := range pending {
			if err := migrate.Apply(ctx, locked, migration); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	database, err := store.New(ctx, databaseURL, cfg.GraphName, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	if err := database.CheckEmbeddingSchema(ctx, "vector", 768); err != nil {
		t.Fatal(err)
	}
	newID := func() model.ID {
		id, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	actor, graphID, otherGraphID := newID(), newID(), newID()
	now := time.Now().UTC()
	if err := database.CreateAccount(ctx, store.Account{ID: actor, LoginID: "synthetic_live", PasswordHash: "synthetic-live-hash", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []model.ID{graphID, otherGraphID} {
		if _, err := database.CreateGraph(ctx, model.Graph{ID: id, Name: "합성 실연결 시험", CreatedBy: actor, CreatedAt: now, LastActivityAt: now, Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	var answerID model.ID
	for i, body := range []string{
		"합성 시험 문서입니다. 파란색 상자는 창고의 선반 위에 있습니다.",
		"합성 시험 문서입니다. 빨간색 공은 정원의 나무 아래에 있습니다.",
	} {
		id := newID()
		if i == 0 {
			answerID = id
		}
		value := model.Context{ID: id, GraphID: graphID, Layer: model.LayerSource, Body: body, RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1,
			Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelWeb, Locator: fmt.Sprintf("https://example.test/live/%d", i)}, OccurredAt: now, OriginKind: model.OriginKindExternalContent}}
		if _, err := database.CreateContext(ctx, graphID, value, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE public.index_task SET next_attempt_at=now()-interval '1 minute' WHERE graph_id=$1", graphID.String()); err != nil {
		t.Fatal(err)
	}
	transport := &liveEmbeddingTransport{next: next}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker, err := index.New(database, index.Config{Provider: "gemini", APIKey: key, BaseURL: address, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 768}, &http.Client{Transport: transport}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := worker.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	for i := range 2 {
		started := time.Now()
		if processed, err := worker.RunOnceInGraph(ctx, graphID); err != nil || !processed {
			t.Fatalf("실제 색인 작업 실행 실패: %v", err)
		}
		var stored int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM public.context_embedding WHERE model_id=$1 AND vector_dims(embedding)=768", worker.ModelID()).Scan(&stored); err != nil || stored != i+1 {
			t.Fatalf("실제 768차원 벡터 저장 수 = %d, 기대 %d, 오류 %v", stored, i+1, err)
		}
		t.Logf("document_%d: indexed=1 elapsed_ms=%d", i+1, time.Since(started).Milliseconds())
	}
	if err := database.CheckGraphIndexReady(ctx, graphID, worker.ModelID()); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM public.index_task").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("색인 완료 뒤 잔여 작업 = %d, 오류 %v", pending, err)
	}
	query := &liveQueryRecorder{Worker: worker}
	searcher, err := search.New(database, query, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 2, FoldThreshold: 0.9, Consistency: search.ConsistencySnapshot}, logger)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	flow, err := searcher.Flow(ctx, search.Input{GraphID: graphID, WorkContext: "파란색 상자는 어디에 있습니까?", Budget: 1000, Scope: "local", MaxHops: 1, MaxHopNodes: 10})
	if err != nil || flow.Channels["semantic"].Failure != "" || flow.Channels["semantic"].Candidates != 2 || len(flow.Contexts) != 2 {
		t.Fatalf("실제 의미 채널·흐름 검색 실패: %v", err)
	}
	if !slices.ContainsFunc(flow.Contexts, func(value search.Context) bool {
		return value.Value.ID == answerID && slices.Contains(value.MatchedChannels, "semantic")
	}) {
		t.Fatal("실제 흐름에 정답의 의미 채널 기여가 없다")
	}
	candidates, err := database.SemanticCandidates(ctx, graphID, worker.ModelID(), query.vector, time.Now().UTC(), 2)
	if err != nil || len(candidates) != 2 || candidates[0].Context.ID != answerID {
		t.Fatalf("합성 정답의 의미 순위 1위 확인 실패: %v", err)
	}
	isolated, err := database.SemanticCandidates(ctx, otherGraphID, worker.ModelID(), query.vector, time.Now().UTC(), 2)
	if err != nil || len(isolated) != 0 {
		t.Fatalf("다른 그래프로 벡터 결과가 노출됐다: %v", err)
	}
	if transport.calls.Load() != 3 {
		t.Fatalf("실제 호출 수 = %d, 기대 3", transport.calls.Load())
	}
	t.Logf("search: semantic_candidates=2 expected_top1=true isolated=true calls=3 elapsed_ms=%d", time.Since(started).Milliseconds())
}
