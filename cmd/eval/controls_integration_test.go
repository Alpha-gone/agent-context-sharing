package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

type commitAfterTimeStore struct {
	*store.Store
	after func()
	once  sync.Once
}

func (database *commitAfterTimeStore) TimeCandidates(ctx context.Context, graph model.ID, at time.Time, limit int) ([]model.SearchCandidate, error) {
	values, err := database.Store.TimeCandidates(ctx, graph, at, limit)
	if err == nil {
		database.once.Do(database.after)
	}
	return values, err
}

// 실제 커밋을 진입 읽기와 확장 사이에 넣는다. 원래 없는 파생의 복구가 RC에서는
// 그래프 전용 후보를 만들고 snapshot은 이전 상태, revision은 새 상태 하나를 설명한다.
func TestConsistencyCandidatesExplainSingleCommitIntegration(t *testing.T) {
	var embeddings atomic.Int64
	database, worker, _ := evalDatabase(t, func() { embeddings.Add(1) })
	set := lifecycleInput(t)
	err := withEvaluationGraph(t.Context(), database, worker, set, 32, func(graph loadedGraph) error {
		for _, mode := range []string{"read_committed", "snapshot", "revision"} {
			t.Run(mode, func(t *testing.T) {
				if _, err := database.SetContextDeleted(t.Context(), graph.GraphID, graph.Keys["derived"], graph.AccountID, true, 0); err != nil {
					t.Fatal(err)
				}
				barrier := &commitAfterTimeStore{Store: database, after: func() {
					if _, err := database.SetContextDeleted(t.Context(), graph.GraphID, graph.Keys["derived"], graph.AccountID, false, 0); err != nil {
						t.Fatal(err)
					}
				}}
				var backend search.Store = barrier
				consistency := search.Consistency("")
				if mode == "snapshot" {
					consistency = search.ConsistencySnapshot
				}
				if mode == "revision" {
					backend = revisionEvaluationStore{Store: barrier, revision: database.EvaluationContentRevision}
					consistency = search.ConsistencySnapshot
				}
				service, err := search.New(backend, worker, search.Config{Execution: search.ExecutionSequential, CandidateLimit: 50, SemanticThreshold: 0.7, FoldThreshold: 0.95, GraphStage: search.GraphStageReferences, Consistency: consistency}, nil)
				if err != nil {
					t.Fatal(err)
				}
				reader := consistencyReader{service: service}
				before := embeddings.Load()
				flow, attempt, err := reader.measuredFlow(t.Context(), search.Input{GraphID: graph.GraphID, WorkContext: "독립 원천", Scope: "local", MaxHops: 1})
				if err != nil {
					t.Fatal(err)
				}
				if embeddings.Load()-before != 1 {
					t.Fatal("재시도로 외부 임베딩을 반복했다")
				}
				if mixedStates(flow) != (mode == "read_committed") {
					t.Fatalf("혼합 상태=%v mode=%s flow=%+v", mixedStates(flow), mode, flow)
				}
				found := false
				for _, item := range flow.Contexts {
					if item.Value.ID == graph.Keys["derived"] {
						found = true
					}
				}
				if found != (mode != "snapshot") {
					t.Fatalf("단일 커밋 결과: found=%t mode=%s", found, mode)
				}
				if (attempt.Retries == 1) != (mode == "revision") {
					t.Fatalf("재시도: %+v", attempt)
				}
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRevisionFailureDiscardsFlowIntegration(t *testing.T) {
	database, worker, _ := evalDatabase(t, nil)
	err := withEvaluationGraph(t.Context(), database, worker, lifecycleInput(t), 32, func(graph loadedGraph) error {
		var revision int64
		backend := revisionEvaluationStore{Store: database, revision: func(context.Context, model.ID) (int64, error) { revision++; return revision, nil }}
		service, err := search.New(backend, worker, search.Config{Execution: search.ExecutionSequential, CandidateLimit: 50, FoldThreshold: 0.95, GraphStage: search.GraphStageReferences, Consistency: search.ConsistencySnapshot}, nil)
		if err != nil {
			return err
		}
		reader := consistencyReader{service: service}
		flow, attempt, err := reader.measuredFlow(t.Context(), search.Input{GraphID: graph.GraphID, WorkContext: "원천", Scope: "local", MaxHops: 1})
		if !errors.Is(err, errRevisionExhausted) || !attempt.Exhausted || attempt.Retries != 3 || len(flow.Contexts) != 0 {
			t.Fatalf("실패한 내용 판을 공개했다: flow=%+v attempt=%+v err=%v", flow, attempt, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConsistencyAndAdaptiveControlsIntegration(t *testing.T) {
	database, worker, connection := evalDatabase(t, nil)
	set := lifecycleInput(t)
	queries := querySet{Version: "eval06-controls", Queries: []querySpec{
		{ID: "q1", IndependentUnit: "t1", UseCase: useCaseFact, WorkContext: "독립 원천", Answers: []string{"source"}},
		{ID: "q2", IndependentUnit: "t2", UseCase: useCaseFact, WorkContext: "독립 파생", Answers: []string{"derived"}},
	}}
	loaded := settings{execution: search.ExecutionSequential, candidateLimit: 50, semanticThreshold: 0.7, foldThreshold: 0.95}
	result, err := runAdaptive(t.Context(), database, worker, set, queries, loaded, adaptiveConditions{Repeats: 10, Budget: 4000, MaxHops: 2, MaxHopNodes: 100, GraphStage: "relations"}, []float64{0.7}, []float64{0.1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Conditions.AsOf.IsZero() || result.Conditions.SignalPolicy == "" || len(result.Oracle) == 0 || len(result.Grid) != 1 || len(result.Runs) < 40 {
		t.Fatalf("라우팅 통제 누락: %+v", result)
	}
	var consistent consistencyReport
	err = withEvaluationGraph(t.Context(), database, worker, set, 32, func(graph loadedGraph) error {
		var err error
		consistent, err = runConsistency(t.Context(), database, worker, graph, set, queries, loaded, consistencyConditions{Requests: 4, LoadRequests: 4, LoadConcurrency: []int{1, 2}, WritePausesMS: []int{50}, GraphStage: "references", Budget: 4000, MaxHops: 2, MaxHopNodes: 100})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(consistent.Equivalence) != 6 || len(consistent.Load) != 6 || len(consistent.Concurrent) != 6 {
		t.Fatalf("일관성 비교 누락: %+v", consistent)
	}
	for _, entry := range consistent.Concurrent {
		if entry.WriteErrors != 0 || (entry.ChannelFailures != 0 && entry.Variant != "read_committed/parallel" && entry.Variant != "read_committed/sequential") {
			t.Fatalf("일관성 시나리오 실패: %+v", entry)
		}
		if entry.Variant != "snapshot/parallel" && entry.Variant != "snapshot/sequential" {
			if entry.Connections != -1 || entry.PeakConnections != -1 || entry.HoldMS.Mean != -1 {
				t.Fatalf("미측정 연결을 0으로 보고했다: %+v", entry)
			}
		} else if entry.Inconsistent != 0 || entry.Errors != 0 || entry.Connections <= 0 || entry.HoldMS.Mean <= 0 {
			t.Fatalf("스냅숏 측정: %+v", entry)
		}
	}
	if queries.Queries[0].AsOf != nil {
		t.Fatal("원본 질의의 조회 시각을 변경했다")
	}
	requireEvaluationDeleted(t, connection, set.Version)
}

func TestEVAL06CommandsIntegration(t *testing.T) {
	_, _, connection := evalDatabase(t, nil)
	var dimension int
	if err := connection.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	vector := make([]float64, dimension)
	vector[0] = 1
	raw, err := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	trustEvalEmbeddingServer(t, server)
	for key, value := range map[string]string{
		"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "AGE_GRAPH_NAME": "agent_context", "EMBEDDING_PROVIDER": "gemini", "EMBEDDING_BASE_URL": server.URL,
		"EMBEDDING_MODEL": "gemini-embedding-2", "EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": strconv.Itoa(dimension), "GEMINI_API_KEY": "synthetic-key",
		"SEARCH_CHANNEL_CANDIDATE_LIMIT": "50", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "0.95", "EVAL_TEST_COMMAND_PROCESS": "1",
	} {
		t.Setenv(key, value)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"adaptive", "consistency"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			set := lifecycleInput(t)
			queries := querySet{Version: "eval06-cli", Queries: []querySpec{{ID: "q1", IndependentUnit: "t1", UseCase: useCaseFact, WorkContext: "private-eval06-query", Answers: []string{"source"}}}}
			contextsPath, queriesPath, output := filepath.Join(directory, "contexts.json"), filepath.Join(directory, "queries.json"), filepath.Join(directory, "result.json")
			for path, value := range map[string]any{contextsPath: set, queriesPath: queries} {
				body, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-" + mode, "-contexts=" + contextsPath, "-queries=" + queriesPath, "-out=" + output}
			if mode == "adaptive" {
				args = append(args, "-direct-thresholds=0.7", "-direct-margins=0.1")
			} else {
				args = append(args, "-consistency-requests=2", "-consistency-load=1", "-consistency-load-requests=2", "-consistency-write-pauses=50")
			}
			logs, err := exec.CommandContext(t.Context(), binary, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("%s CLI: %v\n%s", mode, err, logs)
			}
			body, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte(set.Contexts[0].Body)) || bytes.Contains(body, []byte(queries.Queries[0].WorkContext)) {
				t.Fatal("본문·질의가 결과에 유출됐다")
			}
			if mode == "consistency" {
				var result consistencyReport
				if err := json.Unmarshal(body, &result, json.RejectUnknownMembers(true)); err != nil {
					t.Fatal(err)
				}
				if len(result.Equivalence) != 6 || len(result.Concurrent) != 6 || result.Conditions.RetryLimit != 3 {
					t.Fatal("일관성 CLI의 후보·재시도 누락")
				}
			} else {
				var result adaptiveReport
				if err := json.Unmarshal(body, &result, json.RejectUnknownMembers(true)); err != nil {
					t.Fatal(err)
				}
				if len(result.Grid) != 1 || len(result.Oracle) == 0 || result.Conditions.AsOf.IsZero() {
					t.Fatal("라우팅 CLI의 통제 결과 누락")
				}
			}
			requireEvaluationDeleted(t, connection, set.Version)
		})
	}
}

// TestAdaptiveNoRelevantLocalIntegration은 관련 국소 후보가 없어 강제 local 경로의 그래프
// 채널이 진입점 없이 생략된 질의가 채널 실패로 세어져 적응형 실행을 중단하지 않는지 확인한다.
func TestAdaptiveNoRelevantLocalIntegration(t *testing.T) {
	database, worker, connection := newEvalDatabase(t, nil, true)
	set := lifecycleInput(t)
	queries := querySet{Version: "eval195-no-local", Queries: []querySpec{
		{ID: "q1", IndependentUnit: "t1", UseCase: useCaseFact, WorkContext: "무관한합성질의어", Answers: []string{"source"}},
		{ID: "q2", IndependentUnit: "t2", UseCase: useCaseFact, WorkContext: "독립 파생", Answers: []string{"derived"}},
	}}
	loaded := settings{execution: search.ExecutionSequential, candidateLimit: 50, semanticThreshold: 0.7, foldThreshold: 0.95}
	result, err := runAdaptive(t.Context(), database, worker, set, queries, loaded, adaptiveConditions{Repeats: 10, Budget: 4000, MaxHops: 2, MaxHopNodes: 100, GraphStage: "relations"}, []float64{0.7}, []float64{0.1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Grid) != 1 || len(result.Judgements) == 0 || len(result.Runs) < 40 {
		t.Fatalf("격자·판정 누락: grid=%d judgements=%d runs=%d", len(result.Grid), len(result.Judgements), len(result.Runs))
	}
	for _, run := range result.Runs {
		for _, metrics := range run.UseCases {
			if metrics.Failures["graph"] != 0 {
				t.Fatalf("%s 회차 %d가 진입점 없는 그래프 생략을 실패로 셌다", run.Stage, run.Repeat)
			}
		}
	}
	requireEvaluationDeleted(t, connection, set.Version)
}
