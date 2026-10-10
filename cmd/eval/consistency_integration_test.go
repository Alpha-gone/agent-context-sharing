package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

// evalDatabase는 현재 시험 DB의 차원에 맞는 로컬 제공자 대역만 사용한다.
// 각 테스트는 새 평가 그래프를 만들고 다른 그래프의 색인 작업을 소비하지 않는다.
func evalDatabase(t *testing.T, onEmbed func()) (*store.Store, *index.Worker, *pgx.Conn) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 평가 도구 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	database, err := store.New(t.Context(), databaseURL, graphName, nil, nil, store.IndexTargetsAllLayers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.WithoutCancel(t.Context())) })
	var dimension int
	if err := connection.QueryRow(t.Context(), `
		SELECT substring(format_type(atttypid, atttypmod) FROM '[0-9]+')::int
		FROM pg_attribute WHERE attrelid = 'public.context_embedding'::regclass AND attname = 'embedding'`).Scan(&dimension); err != nil || dimension <= 0 {
		t.Fatalf("시험 DB 임베딩 차원 = %d, %v", dimension, err)
	}
	vector := make([]float64, dimension)
	vector[0] = 1
	raw, err := json.Marshal(map[string]any{"embeddings": [][]float64{vector}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if onEmbed != nil {
			onEmbed()
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(raw)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := index.New(database, index.Config{Provider: "ollama", BaseURL: baseURL, Model: "eval-regression", VectorType: "vector", Dimension: dimension}, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return database, worker, connection
}

func evalScenario(t *testing.T) *consistencyScenario {
	t.Helper()
	database, worker, _ := evalDatabase(t, nil)
	scenario, err := createConsistencyScenario(t.Context(), database, worker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := deleteConsistencyGraph(t.Context(), database, scenario.graph); err != nil {
			t.Error(err)
		}
	})
	return scenario
}

func TestConsistencyWriterTracksOnlySuccessfulWritesIntegration(t *testing.T) {
	scenario := evalScenario(t)
	state := &writerState{discarded: make([]bool, len(scenario.toggled))}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	write := func(ctx context.Context, step int, wantError bool) {
		t.Helper()
		state.next = step
		err := scenario.write(ctx, state)
		if (err != nil) != wantError {
			t.Fatalf("쓰기 단계 %d 오류 = %v", step, err)
		}
	}
	write(canceled, 0, true)
	if state.discarded[0] {
		t.Fatal("실패한 폐기를 추적 상태에 반영했다")
	}
	write(t.Context(), 0, false)
	write(canceled, 0, true)
	if !state.discarded[0] {
		t.Fatal("실패한 복구를 추적 상태에 반영했다")
	}
	write(canceled, 1, true)
	if state.relation != nil {
		t.Fatal("실패한 관계 확정을 추적 상태에 반영했다")
	}
	write(t.Context(), 1, false)
	confirmed := *state.relation
	write(canceled, 1, true)
	if *state.relation != confirmed {
		t.Fatal("실패한 관계 폐기를 추적 상태에 반영했다")
	}
	write(t.Context(), 1, false)
	discarded := *state.relation
	write(canceled, 1, true)
	if *state.relation != discarded {
		t.Fatal("실패한 재확정을 추적 상태에 반영했다")
	}
	write(canceled, 2, true)
	if state.temporary != nil {
		t.Fatal("실패한 임시 파생 생성을 추적 상태에 반영했다")
	}
	write(t.Context(), 2, false)
	temporary := *state.temporary
	write(canceled, 2, true)
	if state.temporary == nil || *state.temporary != temporary {
		t.Fatal("실패한 임시 파생 폐기를 추적 상태에 반영했다")
	}
	if err := scenario.reset(canceled, state); err == nil || state.temporary == nil {
		t.Fatalf("정리 실패가 사라졌다: %v", err)
	}
	if err := scenario.reset(t.Context(), state); err != nil {
		t.Fatal(err)
	}
}

func TestConsistencyResetRestoresAllStateBetweenVariantsIntegration(t *testing.T) {
	scenario := evalScenario(t)
	for range 2 {
		state := &writerState{discarded: make([]bool, len(scenario.toggled))}
		for range 3 {
			if err := scenario.write(t.Context(), state); err != nil {
				t.Fatal(err)
			}
		}
		temporary := *state.temporary
		if err := scenario.reset(t.Context(), state); err != nil {
			t.Fatal(err)
		}
		if state.discarded[0] || state.temporary != nil || state.relation.State != model.RelationStateDiscarded {
			t.Fatalf("초기 상태를 복구하지 않았다: %+v", state)
		}
		for _, id := range scenario.toggled {
			value, err := scenario.database.Context(t.Context(), scenario.graph.GraphID, id)
			if err != nil || value.DeletedAt != nil {
				t.Fatalf("파생 복구 = %+v, %v", value, err)
			}
		}
		value, err := scenario.database.Context(t.Context(), scenario.graph.GraphID, temporary)
		if err != nil || value.DeletedAt == nil {
			t.Fatalf("임시 파생 정리 = %+v, %v", value, err)
		}
		assertNoConfirmedRelations(t, scenario)
	}
}

func assertNoConfirmedRelations(t *testing.T, scenario *consistencyScenario) {
	t.Helper()
	relations, _, err := scenario.database.ListRelations(t.Context(), scenario.graph.GraphID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, relation := range relations {
		if relation.State == model.RelationStateConfirmed {
			t.Fatalf("다음 구성에 확정 관계가 남았다: %+v", relation)
		}
	}
}

func TestConsistencyMeasureResetsVariantsIntegration(t *testing.T) {
	scenario := evalScenario(t)
	loaded := settings{candidateLimit: 50, semanticThreshold: 0.7, foldThreshold: 0.95}
	conditions := consistencyConditions{Requests: 8, Budget: 20000, MaxHops: 2, MaxHopNodes: 100}
	for _, variant := range consistencyVariants() {
		service, err := consistencyService(scenario.database, scenario.worker, loaded, search.GraphStageRelations, variant)
		if err != nil {
			t.Fatal(err)
		}
		result, err := scenario.measure(t.Context(), service, variant, conditions, time.Millisecond)
		if err != nil || result.Errors != 0 || result.WriteErrors != 0 {
			t.Fatalf("구성 %s 측정 = %+v, %v", variant.Name, result, err)
		}
		assertNoConfirmedRelations(t, scenario)
		for _, id := range scenario.toggled {
			value, err := scenario.database.Context(t.Context(), scenario.graph.GraphID, id)
			if err != nil || value.DeletedAt != nil {
				t.Fatalf("구성 뒤 파생 복구 = %+v, %v", value, err)
			}
		}
	}
}

func TestConsistencyScenarioCleansGraphAfterIndexFailureIntegration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	onEmbed := sync.OnceFunc(cancel)
	database, worker, connection := evalDatabase(t, onEmbed)
	counts := func() (int, int) {
		t.Helper()
		var total, active int
		if err := connection.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL)
			FROM public.context_graph WHERE name = 'eval consistency-scenario'`).Scan(&total, &active); err != nil {
			t.Fatal(err)
		}
		return total, active
	}
	totalBefore, activeBefore := counts()
	if scenario, err := createConsistencyScenario(ctx, database, worker); scenario != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("색인 실패 반환 = %v, %v", scenario, err)
	}
	totalAfter, activeAfter := counts()
	if totalAfter != totalBefore+1 || activeAfter != activeBefore {
		t.Fatalf("실패한 그래프 정리: 전체 %d→%d, 활성 %d→%d", totalBefore, totalAfter, activeBefore, activeAfter)
	}
}

func TestContinualRecordsUnmeasuredMetricsAsUndefinedIntegration(t *testing.T) {
	database, worker, _ := evalDatabase(t, nil)
	scenario := continualScenarios()[0]
	graph, err := loadGraph(t.Context(), database, scenario.contexts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := deleteConsistencyGraph(t.Context(), database, graph); err != nil {
			t.Error(err)
		}
	})
	if err := drainIndexQueue(t.Context(), database, worker, graph.GraphID, len(scenario.contexts.Contexts)*4); err != nil {
		t.Fatal(err)
	}
	service, err := consistencyService(database, worker, settings{candidateLimit: 50, semanticThreshold: 0.7, foldThreshold: 0.95}, search.GraphStageRelations, consistencyVariants()[0])
	if err != nil {
		t.Fatal(err)
	}
	sample, _, err := measureContinualQuery(t.Context(), service, graph, scenario.query, 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range metricNames[3:] {
		if got := metric.sample(sample); got != undefinedMetric {
			t.Errorf("측정하지 않은 %s = %v", metric.name, got)
		}
	}
}
