package main

import (
	"bytes"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestControlledComparisonsIntegration(t *testing.T) {
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
	var attempts atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	trustEvalEmbeddingServer(t, server)
	for key, value := range map[string]string{
		"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "AGE_GRAPH_NAME": "agent_context",
		"EMBEDDING_PROVIDER": "gemini", "EMBEDDING_BASE_URL": server.URL, "EMBEDDING_MODEL": "gemini-embedding-2",
		"EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": strconv.Itoa(dimension), "GEMINI_API_KEY": "synthetic-key",
		"SEARCH_CHANNEL_CANDIDATE_LIMIT": "50", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "0.95", "EVAL_TEST_COMMAND_PROCESS": "1",
	} {
		t.Setenv(key, value)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(t *testing.T, indexMode, reject bool) {
		t.Helper()
		set := lifecycleInput(t)
		queries := querySet{Version: "controlled-v1", Queries: []querySpec{
			{ID: "q1", IndependentUnit: "task1", UseCase: useCaseFact, WorkContext: "독립 원천", Answers: []string{"source"}, Required: []string{"source", "derived"}, Paths: [][]string{{"source", "derived"}}},
			{ID: "q2", IndependentUnit: "task2", UseCase: useCaseFact, WorkContext: "원천 근거 파생", Answers: []string{"derived"}, Required: []string{"source", "derived"}, Paths: [][]string{{"derived", "source"}}},
		}}
		dir := t.TempDir()
		contextsPath, queriesPath, output := filepath.Join(dir, "contexts.json"), filepath.Join(dir, "queries.json"), filepath.Join(dir, "result.json")
		for path, value := range map[string]any{contextsPath: set, queriesPath: queries} {
			body, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
		}
		args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-contexts=" + contextsPath, "-queries=" + queriesPath, "-out=" + output}
		if indexMode {
			args = append(args, "-compare-index-targets", "-stages=references")
		}
		before := attempts.Load()
		logs, err := exec.CommandContext(t.Context(), binary, args...).CombinedOutput()
		if reject {
			if err == nil || !strings.Contains(string(logs), "issue85 cleanup rejected") {
				t.Fatalf("정리 실패: %v\n%s", err, logs)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("정리 실패 후 정상 보고서를 기록했다")
			}
			return
		}
		if err != nil {
			t.Fatalf("통제 CLI: %v\n%s", err, logs)
		}
		body, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte(set.Contexts[0].Body)) || bytes.Contains(body, []byte(queries.Queries[0].WorkContext)) {
			t.Fatal("비공개 본문이 결과에 들어갔다")
		}
		var result report
		if err := json.Unmarshal(body, &result, json.RejectUnknownMembers(true)); err != nil {
			t.Fatal(err)
		}
		wantRuns, wantCalls, wantAxis := 50, int64(200), "graph_stage"
		if indexMode {
			wantRuns, wantCalls, wantAxis = 20, 70, "index_targets"
		}
		if len(result.Runs) != wantRuns || result.Conditions.ComparisonAxis != wantAxis || attempts.Load()-before != wantCalls {
			t.Fatalf("독립 실행: rounds=%d calls=%d conditions=%+v", len(result.Runs), attempts.Load()-before, result.Conditions)
		}
		if result.Conditions.AsOf.IsZero() || result.Conditions.PreparationPolicy == "" {
			t.Fatal("준비·시각 통제가 빠졌다")
		}
		var allBytes, withoutBytes int64
		for _, run := range result.Runs {
			prep := run.Preparation
			if prep == nil {
				t.Fatal("준비 계측 누락")
			}
			wantRows := int64(2)
			if run.Stage == "without_source" {
				wantRows = 1
				withoutBytes += prep.VectorBytes
			} else {
				allBytes += prep.VectorBytes
			}
			if prep == nil || prep.VectorRows != wantRows || prep.EmbeddingCalls != wantRows || prep.VectorBytes <= 0 || prep.DurationMS < 0 {
				t.Fatalf("준비 실측: %+v", prep)
			}
			if run.UseCases[useCaseFact].Recall != 1 || len(run.QuerySamples) != 2 || run.QuerySamples[0].IndependentUnit == run.QuerySamples[1].IndependentUnit {
				t.Fatalf("원천·근거/독립 단위 보존: %+v", run)
			}
			if run.Stage == "baseline" && (run.UseCases[useCaseFact].Channels["graph"] != 0 || run.UseCases[useCaseFact].Channels["global_summary"] != 0) {
				t.Fatal("기준선의 그래프 채널이 켜졌다")
			}
		}
		if indexMode && allBytes <= withoutBytes {
			t.Fatal("원천 제외 저장량이 줄지 않았다")
		}
		for i, run := range result.Runs {
			variants := result.Conditions.Variants
			if run.Stage != variants[(i%len(variants)+run.Repeat-1)%len(variants)].Name {
				t.Fatal("회차 안의 조건 순환 누락")
			}
		}
		for _, assessment := range result.Assessments {
			if assessment.DefaultActivation {
				t.Fatal("모듈 시험으로 운영 기본값을 승격했다")
			}
		}
		var graphs, active, sources int
		if err := connection.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM public.context_graph WHERE name=$1`, "eval "+set.Version).Scan(&graphs, &active); err != nil {
			t.Fatal(err)
		}
		if graphs != wantRuns || active != 0 {
			t.Fatalf("그래프 수명: graphs=%d active=%d", graphs, active)
		}
		if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM agent_context."Context" c JOIN public.context_graph g ON c.properties OPERATOR(ag_catalog.->>) 'graph_id'::text = g.graph_id::text WHERE g.name=$1 AND c.properties OPERATOR(ag_catalog.->>) 'layer'::text = 'source'`, "eval "+set.Version).Scan(&sources); err != nil {
			t.Fatal(err)
		}
		if sources != wantRuns {
			t.Fatalf("원천 저장을 생략했다: sources=%d", sources)
		}
	}
	t.Run("graph", func(t *testing.T) { invoke(t, false, false) })
	t.Run("index", func(t *testing.T) { invoke(t, true, false) })
	t.Run("prepared", func(t *testing.T) {
		data := preparedComparisonFixture(t)
		directory := filepath.Join(t.TempDir(), "prepared")
		if err := writePreparedData(directory, data); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(t.TempDir(), "result.json")
		before := attempts.Load()
		logs, err := exec.CommandContext(t.Context(), binary, "-test.run=^TestEvalCommandProcess$", "--", "-prepared-input="+directory, "-compare-index-targets", "-stages=references", "-out="+output).CombinedOutput()
		if err != nil {
			t.Fatalf("준비 입력 CLI: %v\n%s", err, logs)
		}
		body, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("private-question-marker")) || bytes.Contains(body, []byte("private-answer-marker")) {
			t.Fatal("준비 주석이 보고서에 유출됐다")
		}
		var result report
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if result.InputPreparation == nil || result.InputPreparation.Graphs != 3 || result.Conditions.GraphGroups != 2 || len(result.Runs) != 20 || attempts.Load()-before != 120 {
			t.Fatalf("준비 그룹/요청 수: %+v calls=%d", result.Conditions, attempts.Load()-before)
		}
		for _, run := range result.Runs {
			if len(run.QuerySamples) != 3 || run.QuerySamples[0].IndependentUnit != run.QuerySamples[1].IndependentUnit || run.QuerySamples[2].IndependentUnit == run.QuerySamples[0].IndependentUnit {
				t.Fatal("준비 독립 단위가 바뀌었다")
			}
			unmeasured := run.QuerySamples[2]
			if unmeasured.Recall != -1 || unmeasured.ReciprocalRank != -1 || unmeasured.BudgetPerHit != -1 || unmeasured.LatencyMS < 0 || unmeasured.BudgetUsed <= 0 {
				t.Fatalf("미주석 지표: %+v", unmeasured)
			}
			rows := int64(4)
			if run.Stage == "without_source" {
				rows = 2
			}
			if run.Preparation.VectorRows != rows || run.Preparation.EmbeddingCalls != rows {
				t.Fatalf("그룹별 전체 색인: %+v", run.Preparation)
			}
		}
		for _, judgement := range result.Judgements {
			if judgement.Estimates[useCaseFact]["recall"].Samples != 1 {
				t.Fatal("미주석 문항이나 반복을 독립 정답 표본으로 늘렸다")
			}
		}
		var names []string
		for _, group := range data.Memory.Groups[:2] {
			names = append(names, "eval "+group.Contexts.Version)
		}
		var graphs, active int
		if err := connection.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE deleted_at IS NULL) FROM public.context_graph WHERE name=ANY($1)`, names).Scan(&graphs, &active); err != nil {
			t.Fatal(err)
		}
		if graphs != 40 || active != 0 {
			t.Fatalf("준비 그룹 수명: graphs=%d active=%d", graphs, active)
		}
	})
	rejectEvalCleanup(t, connection)
	t.Run("graph_cleanup_failure", func(t *testing.T) { invoke(t, false, true) })
	t.Run("index_cleanup_failure", func(t *testing.T) { invoke(t, true, true) })
}
