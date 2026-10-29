package main

import (
	"bytes"
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
)

func TestUnitComparisonCommandIntegration(t *testing.T) {
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
	var failQueries atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		var request struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
		}
		if failQueries.Load() && len(request.Content.Parts) == 1 && strings.HasPrefix(request.Content.Parts[0].Text, "task: search result | query: ") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	trustEvalEmbeddingServer(t, server)
	for key, value := range map[string]string{
		"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "AGE_GRAPH_NAME": "agent_context", "EMBEDDING_PROVIDER": "gemini", "EMBEDDING_BASE_URL": server.URL,
		"EMBEDDING_MODEL": "gemini-embedding-2", "EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": strconv.Itoa(dimension), "GEMINI_API_KEY": "synthetic-key",
		"SEARCH_CHANNEL_EXECUTION": "parallel", "SEARCH_CHANNEL_CANDIDATE_LIMIT": "50", "SEARCH_SEMANTIC_SIMILARITY_THRESHOLD": "0.5", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "0.95", "EVAL_TEST_COMMAND_PROCESS": "1",
	} {
		t.Setenv(key, value)
	}
	input := unitFixture(t, 1)
	input.Version = lifecycleInput(t).Version
	input.Search.EmbeddingModel = fmt.Sprintf("gemini:gemini-embedding-2:retrieval-v1:vector:%d", dimension)
	refreshUnitHashes(&input)
	inputPath := writeManifestFixture(t, input)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(output string, extra ...string) ([]byte, error) {
		args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-unit-comparison=" + inputPath, "-out=" + output}
		return exec.CommandContext(t.Context(), binary, append(args, extra...)...).CombinedOutput()
	}
	output := filepath.Join(t.TempDir(), "result.json")
	beforeCalls := attempts.Load()
	logs, err := invoke(output)
	if err != nil {
		t.Fatalf("단위 CLI: %v\n%s", err, logs)
	}
	body, err := os.ReadFile(output)
	if err != nil || bytes.Contains(body, []byte("private-")) || bytes.Contains(body, []byte("urn:")) {
		t.Fatalf("보고서/원문 유출: %v", err)
	}
	var result unitReport
	if err := json.Unmarshal(body, &result, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 40 || len(result.Cohorts) != 2 || result.AsOf.IsZero() || result.Repeats != 10 {
		t.Fatalf("조건/회차: %+v", result)
	}
	// 원천 두 개 + 파생 둘/하나 + 질의 둘을 조건·회차마다 실제 실행한다.
	if calls := attempts.Load() - beforeCalls; calls != 220 {
		t.Fatalf("실제 HTTP 호출 %d != 220", calls)
	}
	var allBytes = map[string]int64{}
	for i, run := range result.Runs {
		wantPolicy := input.Policies[(i/2%2+run.Metrics.Repeat-1)%2].Name
		wantRows := int64(4)
		if run.Metrics.Stage == "semantic" {
			wantRows = 3
		}
		prep := run.Metrics.Preparation
		if run.Metrics.Stage != wantPolicy || prep.VectorRows != wantRows || prep.EmbeddingCalls != wantRows || prep.VectorBytes <= 0 {
			t.Fatalf("실측/순서: %+v", run)
		}
		allBytes[run.Metrics.Stage] += prep.VectorBytes
		if run.Task.Success != 1 || run.Metrics.UseCases[useCaseFact].Recall != 1 || run.Metrics.UseCases[useCaseFact].EvidenceCompleteness != 1 || len(run.Metrics.QuerySamples) != 2 {
			t.Fatalf("분리된 지표: %+v", run)
		}
	}
	if allBytes["current"] <= allBytes["semantic"] {
		t.Fatal("파생 수 감소에 따른 벡터 열 바이트 감소가 없다")
	}
	for _, cohort := range result.Cohorts {
		if cohort.Samples != 1 || cohort.Status != "deferred" || cohort.Comparisons[0].Interval != -1 || cohort.Totals["current"].Completeness != 0 || cohort.Totals["semantic"].Completeness != 1 {
			t.Fatal(cohort)
		}
	}
	names := []string{}
	for _, sample := range input.Samples {
		for _, policy := range input.Policies {
			names = append(names, "eval "+input.Version+"-"+sample.Code+"-"+policy.Name)
		}
	}
	var graphs, active, sources int
	if err := connection.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM public.context_graph WHERE name=ANY($1)`, names).Scan(&graphs, &active); err != nil {
		t.Fatal(err)
	}
	if graphs != 40 || active != 0 {
		t.Fatalf("격리·정리: %d %d", graphs, active)
	}
	if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM agent_context."Context" c JOIN public.context_graph g ON c.properties OPERATOR(ag_catalog.->>) 'graph_id'::text=g.graph_id::text WHERE g.name=ANY($1) AND c.properties OPERATOR(ag_catalog.->>) 'layer'::text='source' AND c.properties OPERATOR(ag_catalog.->>) 'body'::text = ANY($2)`, names, []string{input.Samples[0].Sources[0].Body, input.Samples[0].Sources[1].Body, input.Samples[1].Sources[0].Body, input.Samples[1].Sources[1].Body}).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if sources != 80 {
		t.Fatalf("원천 원문 보존: %d != 80", sources)
	}
	for _, extra := range [][]string{{"-adaptive"}, {"-contexts=missing"}, {"-budget=1"}, {"-repeats=9"}} {
		t.Run("preflight/"+extra[0], func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://127.0.0.1:1/no-db")
			destination := filepath.Join(t.TempDir(), "failed.json")
			before := attempts.Load()
			if logs, err := invoke(destination, extra...); err == nil || strings.Contains(string(logs), "dial") {
				t.Fatalf("입력 사전 검사: %v\n%s", err, logs)
			}
			if attempts.Load() != before {
				t.Fatal("사전 검사에서 제공자 호출")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("실패 뒤 보고서 생성")
			}
		})
	}
	t.Run("partial_channel_failure", func(t *testing.T) {
		failQueries.Store(true)
		defer failQueries.Store(false)
		destination := filepath.Join(t.TempDir(), "failed.json")
		logs, err := invoke(destination)
		if err == nil {
			t.Fatalf("검색 장애가 정상 측정이 됐다: %s", logs)
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatal("검색 실패 보고서 생성")
		}
		if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM public.context_graph WHERE name=ANY($1) AND deleted_at IS NULL`, names).Scan(&active); err != nil || active != 0 {
			t.Fatalf("검색 장애 뒤 정리: %d %v", active, err)
		}
	})
	rejectEvalCleanup(t, connection)
	t.Run("cleanup_failure", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "failed.json")
		logs, err := invoke(destination)
		if err == nil || !strings.Contains(string(logs), "issue85 cleanup rejected") {
			t.Fatalf("정리 실패: %v\n%s", err, logs)
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatal("정리 실패 보고서 생성")
		}
	})
}
