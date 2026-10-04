//go:build embedding_live

package migrate_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
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

// TestGeminiLiveIndexSearchIntegration은 새 시험 DB에서 합성 자료만 색인·검색한다.
func TestGeminiLiveIndexSearchIntegration(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if key == "" {
		t.Fatal("실연결 시험에는 외부 전송 허용과 GEMINI_API_KEY 주입이 필요하다")
	}
	admin, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || admin == nil || (admin.Scheme != "postgres" && admin.Scheme != "postgresql") || !slices.Contains([]string{"127.0.0.1", "localhost", "::1"}, admin.Hostname()) {
		t.Fatal("격리 실연결 시험에는 루프백 주소의 TEST_DATABASE_URL이 필요하다")
	}
	parameters, err := url.ParseQuery(admin.RawQuery)
	if err != nil {
		t.Fatal("시험 DB 주소의 질의 구성이 올바르지 않다")
	}
	// pgx의 host 질의 옵션으로 루프백 검사 이후 접속 대상을 바꾸지 못하게 한다.
	for name := range parameters {
		if name != "sslmode" {
			t.Fatal("시험 DB 주소는 sslmode 외의 질의 옵션을 허용하지 않는다")
		}
	}
	pool, databaseURL := isolatedMigrationDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 110*time.Second)
	defer cancel()
	cfg := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768, IndexTargetLayers: "all_layers"}
	if err := migrate.WithLock(ctx, pool, func(ctx context.Context) error {
		loaded, applied, err := migrate.Prepare(ctx, pool, migrations.FS, ".", cfg)
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(loaded, applied)
		if err != nil {
			return err
		}
		for _, migration := range pending {
			if err := migrate.Apply(ctx, pool, migration); err != nil {
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
	address, err := url.Parse("https://generativelanguage.googleapis.com")
	if err != nil {
		t.Fatal("공식 임베딩 주소 해석 실패")
	}
	transport := &liveEmbeddingTransport{next: http.DefaultTransport}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker, err := index.New(database, index.Config{Provider: "gemini", APIKey: key, BaseURL: address, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 768}, &http.Client{Transport: transport}, logger)
	if err != nil {
		t.Fatal(err)
	}
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
