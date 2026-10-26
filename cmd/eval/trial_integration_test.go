package main

import (
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func trialGraphManifest(t *testing.T, contexts contextSet, queries querySet) (executionManifest, trialBindings) {
	t.Helper()
	m := validManifest()
	m.TrialID = "graph-trial"
	m.Provenance = "synthetic"
	m.Inputs = nil
	m.Mapping = manifestMapping{Preprocessing: "direct-v1", Split: "direct-v1", Answers: "direct-v1", Evidence: "direct-v1", AnswerPolicy: "evaluation_only"}
	m.Units = trialUnits("synthetic", queries.Queries, "")
	m.Exclusions = manifestExclusions{Policy: "direct-v1", Total: len(m.Units), Items: []manifestExclusion{}}
	m.Execution.Preparation, m.Execution.Cache = "fresh_graph_no_explicit_warmup_shared_cache", "shared_db_os_cache"
	m.Execution.RequestLimit = 1000
	m.Execution.IndexRoundLimit = 100
	m.Execution.TimeoutMS = 60000
	m.Execution.Seeds = []manifestSeed{{Component: "embedding", Supported: new(false)}}
	m.Search.Stages = []string{"baseline", "references"}
	m.Search.CandidateLimit = 50
	m.Search.FoldThreshold = new(0.95)
	m.Cost.Items = nil
	for _, code := range businessComponents {
		m.Cost.Items = append(m.Cost.Items, manifestCostItem{Code: code, Status: "unmeasured"})
	}
	b := trialBindings{}
	for _, input := range []struct {
		role, version string
		value         any
	}{{"contexts", contexts.Version, contexts}, {"queries", queries.Version, queries}} {
		path := writeManifestFixture(t, input.value)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m.Inputs = append(m.Inputs, manifestInput{Code: input.role, Dataset: "synthetic", Role: input.role, Version: input.version, Commit: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))})
		b[input.role] = path
	}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	return m, b
}

func configureTrialGraph(t *testing.T, server *httptest.Server, dimension int) {
	t.Helper()
	trustEvalEmbeddingServer(t, server)
	trustTrialTransport(t, server)
	for key, value := range map[string]string{"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "AGE_GRAPH_NAME": "agent_context", "EMBEDDING_PROVIDER": "gemini", "EMBEDDING_BASE_URL": server.URL, "EMBEDDING_MODEL": "gemini-embedding-2", "EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": strconv.Itoa(dimension), "GEMINI_API_KEY": "synthetic-key", "SEARCH_CHANNEL_EXECUTION": "parallel", "SEARCH_CHANNEL_CANDIDATE_LIMIT": "50", "SEARCH_SEMANTIC_SIMILARITY_THRESHOLD": "0.5", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "0.95", "EVAL_TEST_COMMAND_PROCESS": "1"} {
		t.Setenv(key, value)
	}
}

// 표준 루트 인증서 풀은 프로세스에서 캐시된다. 환경 변수만 바꾸는 CLI 방식 대신
// 같은 프로세스의 통합 시험에는 해당 대역의 CA를 명시적으로 주입한다.
func trustTrialTransport(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := http.DefaultTransport
	trusted := previous.(*http.Transport).Clone()
	trusted.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	http.DefaultTransport = trusted
	t.Cleanup(func() { trusted.CloseIdleConnections(); http.DefaultTransport = previous })
}

func TestManifestTrialGraphIntegration(t *testing.T) {
	_, _, conn := evalDatabase(t, nil)
	var dimension int
	if err := conn.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	vector := make([]float64, dimension)
	vector[0] = 1
	raw, _ := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	configureTrialGraph(t, server, dimension)
	for _, outcome := range []string{"direct", "index_targets", "prepared", "request_budget", "index_budget", "model_mismatch", "frozen_input", "cleanup_failure"} {
		t.Run(outcome, func(t *testing.T) {
			contexts := lifecycleInput(t)
			queries := querySet{Version: "query-v1", Queries: []querySpec{{ID: "q1", IndependentUnit: "task1", UseCase: useCaseFact, WorkContext: "독립 원천", Answers: []string{"source"}, Required: []string{"source", "derived"}}, {ID: "q2", IndependentUnit: "task2", UseCase: useCaseFact, WorkContext: "원천 근거", Answers: []string{"derived"}, Required: []string{"source", "derived"}}}}
			m, b := trialGraphManifest(t, contexts, queries)
			m.Models.Dimension = dimension
			m.Models.Embedding = fmt.Sprintf("gemini:gemini-embedding-2:retrieval-v1:vector:%d", dimension)
			root, prepared := t.TempDir(), ""
			success := true
			before := calls.Load()
			switch outcome {
			case "index_targets":
				m.ComparisonAxis = "index_targets"
				m.Search.Stages = []string{"references"}
			case "prepared":
				data := preparedComparisonFixture(t)
				prepared = filepath.Join(t.TempDir(), "data")
				if err := writePreparedData(prepared, data); err != nil {
					t.Fatal(err)
				}
				loaded, err := loadPreparedComparison(prepared)
				if err != nil {
					t.Fatal(err)
				}
				m.Units = loaded.Preparation.Units
				m.Exclusions = loaded.Preparation.Exclusions
				m.Mapping = loaded.Preparation.Mapping
				m.Mapping.AnswerPolicy = "evaluation_only"
				m.Search.Stages = []string{"baseline"}
				m.Inputs = nil
				b = trialBindings{}
				for _, f := range []struct{ role, name string }{{"contexts", "memory.json"}, {"queries", "evaluation-private.json"}, {"preparation", "preparation.json"}} {
					bytes, err := os.ReadFile(filepath.Join(prepared, f.name))
					if err != nil {
						t.Fatal(err)
					}
					m.Inputs = append(m.Inputs, manifestInput{Code: f.role, Dataset: loaded.Preparation.Dataset, Role: f.role, Version: preparationVersion, Commit: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(bytes))})
				}
			case "request_budget":
				m.Execution.RequestLimit = 3
				success = false
			case "index_budget":
				m.Execution.IndexRoundLimit = 1
				success = false
			case "model_mismatch":
				m.Models.Dimension++
				success = false
			case "cleanup_failure":
				rejectEvalCleanup(t, conn)
				success = false
			}
			var err error
			if outcome == "frozen_input" {
				trial, e := newManifestTrial(m, root)
				if e != nil {
					t.Fatal(e)
				}
				if e = trial.bindInputs(b, ""); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(b["contexts"], []byte(`{"private":"changed-after-checksum"}`), 0600); e != nil {
					t.Fatal(e)
				}
				args, e := trial.arguments(false)
				if e != nil {
					t.Fatal(e)
				}
				err = trial.finish(runEvaluation(args, trial.Progress))
			} else {
				err = runManifestTrial(writeManifestFixture(t, m), root, b, prepared)
			}
			if (err == nil) != success {
				t.Fatalf("%s: %v", outcome, err)
			}
			assertTrialFiles(t, root, m.TrialID, success)
			if success {
				var summary trialSummary
				bytes := readTrialFile(t, root, m.TrialID, "summary.json", &summary)
				if strings.Contains(string(bytes), contexts.Contexts[0].Body) || strings.Contains(string(bytes), server.URL) || strings.Contains(string(bytes), "private-question-marker") {
					t.Fatal("원문/질의/출처 노출")
				}
				var prep trialPreparation
				readTrialFile(t, root, m.TrialID, "preparation.json", &prep)
				if prep.Readiness != "ready" || prep.Pending != 0 || prep.Failed != 0 || prep.VectorRows <= 0 || prep.VectorBytes <= 0 || prep.DocumentCalls <= 0 || prep.Costs["preparation"].Total != -1 {
					t.Fatalf("실측 준비량/미측정 금액: %+v", prep)
				}
				if prep.Progress.Graphs == 0 || prep.Progress.Graphs != prep.Progress.CleanupAttempts || prep.Progress.Cleanup != "completed" || prep.Progress.ActiveGraphs != 0 || len(prep.Progress.Completed) != 10*len(m.Search.Stages) && outcome != "index_targets" {
					t.Fatalf("준비/회차/정리: %+v", prep.Progress)
				}
				var cost trialCost
				readTrialFile(t, root, m.TrialID, "cost.json", &cost)
				if cost.Total.Total != -1 || cost.Total.Status != "unmeasured" {
					t.Fatal("임베딩 HTTP 호출을 금액으로 추정했다")
				}
			} else {
				var failure struct {
					Trial    string                `json:"trial_id"`
					Mode     string                `json:"mode"`
					Status   string                `json:"status"`
					Code     string                `json:"cause_code"`
					Progress trialProgressSnapshot `json:"progress"`
				}
				readTrialFile(t, root, m.TrialID, "failure.json", &failure)
				if outcome == "request_budget" && (failure.Code != "request_limit_exceeded" || failure.Progress.Requests != 3 || len(failure.Progress.Completed) != 1 || failure.Progress.ActiveGraphs != 0) {
					t.Fatalf("요청 상한 실패: %+v", failure)
				}
				if outcome == "index_budget" && (len(failure.Progress.Completed) != 0 || failure.Progress.Cleanup != "completed") {
					t.Fatalf("준비 실패: %+v", failure)
				}
				if outcome == "model_mismatch" && (failure.Progress.Graphs != 0 || calls.Load() != before) {
					t.Fatal("모델 불일치 전에 실제 호출했다")
				}
				if outcome == "cleanup_failure" && (failure.Progress.Cleanup != "failed" || failure.Progress.CleanupFailures == 0) {
					t.Fatalf("정리 실패: %+v", failure)
				}
			}
			if outcome != "prepared" && outcome != "model_mismatch" && outcome != "cleanup_failure" {
				requireEvaluationDeleted(t, conn, contexts.Version)
			}
		})
	}
}

func TestManifestTrialOriginalSnapshotIntegration(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Source-Revision", "rev1")
		_, _ = w.Write(originalCapturedBody)
	}))
	defer server.Close()
	trustEvalEmbeddingServer(t, server)
	trustTrialTransport(t, server)
	input, path := originalFixture(t, server.URL)
	writeOriginalFixture(t, input, path)
	raw, _ := os.ReadFile(path)
	m := validManifest()
	m.Mode = "originals"
	m.TrialID = "original-trial"
	m.Provenance = "synthetic"
	m.Inputs = []manifestInput{{Code: "original", Role: "original_reproduction", Dataset: "original", Version: input.Version, Commit: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}}
	m.Mapping = manifestMapping{Preprocessing: "direct-v1", Split: "direct-v1", Answers: "direct-v1", Evidence: "direct-v1", AnswerPolicy: "evaluation_only"}
	m.Execution.Seeds = []manifestSeed{{Component: "http", Supported: new(false)}}
	m.Execution.RequestLimit = input.RequestLimit
	m.Units = nil
	for _, s := range input.Samples {
		m.Units = append(m.Units, manifestUnit{Dataset: "original", Query: s.Code, Task: s.Independent, Type: s.Scenario, Independent: s.Independent})
	}
	m.Exclusions = manifestExclusions{Policy: "direct-v1", Total: len(m.Units), Items: []manifestExclusion{}}
	m.Cost.Items = nil
	for _, c := range businessComponents {
		m.Cost.Items = append(m.Cost.Items, manifestCostItem{Code: c, Status: "unmeasured"})
	}
	root := t.TempDir()
	if err := runManifestTrial(writeManifestFixture(t, m), root, trialBindings{"original": path}, ""); err != nil {
		t.Fatal(err)
	}
	assertTrialFiles(t, root, m.TrialID, true)
	var summary trialSummary
	bytes := readTrialFile(t, root, m.TrialID, "summary.json", &summary)
	if calls.Load() != 70 || strings.Contains(string(bytes), server.URL) || strings.Contains(string(bytes), string(originalCapturedBody)) {
		t.Fatal("원본 조회 횟수/비공개 경계")
	}
}

func TestManifestTrialSignalCleanupIntegration(t *testing.T) {
	_, _, conn := evalDatabase(t, nil)
	var dimension int
	if err := conn.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	vector := make([]float64, dimension)
	vector[0] = 1
	response, _ := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
	started := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
			return
		}
		if strings.HasPrefix(body.Content.Parts[0].Text, "task: search result | query:") {
			select {
			case started <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(response)
	}))
	defer server.Close()
	configureTrialGraph(t, server, dimension)
	contexts := lifecycleInput(t)
	queries := querySet{Version: "signal-v1", Queries: []querySpec{{ID: "q1", UseCase: useCaseFact, WorkContext: "private-query", Answers: []string{"source"}}}}
	m, b := trialGraphManifest(t, contexts, queries)
	m.Models.Dimension = dimension
	m.Models.Embedding = fmt.Sprintf("gemini:gemini-embedding-2:retrieval-v1:vector:%d", dimension)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-manifest=" + writeManifestFixture(t, m), "-results=" + root, "-trial-input=contexts=" + b["contexts"], "-trial-input=queries=" + b["queries"]}
	cmd := exec.CommandContext(t.Context(), binary, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("질의 시작 대기 초과")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("취소를 성공 처리했다")
	}
	assertTrialFiles(t, root, m.TrialID, false)
	var failure struct {
		Trial    string                `json:"trial_id"`
		Mode     string                `json:"mode"`
		Status   string                `json:"status"`
		Code     string                `json:"cause_code"`
		Progress trialProgressSnapshot `json:"progress"`
	}
	readTrialFile(t, root, m.TrialID, "failure.json", &failure)
	if failure.Code != "cancelled" || failure.Progress.Cleanup != "completed" || failure.Progress.ActiveGraphs != 0 {
		t.Fatalf("신호 취소/독립 정리: %+v", failure)
	}
	requireEvaluationDeleted(t, conn, contexts.Version)
}
