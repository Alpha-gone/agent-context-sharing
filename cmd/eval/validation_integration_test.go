package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

// TestEvalCommandProcess는 시험 바이너리 안에서 실제 CLI 경로와 실패 종료를 실행한다.
func TestEvalCommandProcess(t *testing.T) {
	if os.Getenv("EVAL_TEST_COMMAND_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"eval"}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("eval", flag.ExitOnError)
	main()
	os.Exit(0)
}

func TestAdversarialIndexFailurePreservesCleanupErrorIntegration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	database, worker, connection := evalDatabase(t, sync.OnceFunc(cancel))
	rejectEvalCleanup(t, connection)
	contexts, _, _ := adversarialFixture(t)
	if _, err := loadAdversarialGraph(ctx, database, worker, contexts); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "issue85 cleanup rejected") {
		t.Fatalf("색인 오류와 정리 오류가 함께 보존되지 않았다: %v", err)
	}
}

func rejectEvalCleanup(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	_, err := connection.Exec(t.Context(), `CREATE FUNCTION public.issue85_reject_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.name LIKE 'eval %' AND OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
	RAISE EXCEPTION 'issue85 cleanup rejected'; END IF; RETURN NEW; END $$;
	CREATE TRIGGER issue85_reject_cleanup BEFORE UPDATE OF deleted_at ON public.context_graph
	FOR EACH ROW EXECUTE FUNCTION public.issue85_reject_cleanup();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := connection.Exec(ctx, `DROP TRIGGER issue85_reject_cleanup ON public.context_graph; DROP FUNCTION public.issue85_reject_cleanup();`); err != nil {
			t.Error(err)
		}
	})
}

func TestAdversarialCleanupFailuresPropagateIntegration(t *testing.T) {
	database, worker, connection := evalDatabase(t, nil)
	rejectEvalCleanup(t, connection)
	contexts, _, _ := adversarialFixture(t)
	service, err := consistencyService(database, worker, settings{candidateLimit: 500, semanticThreshold: 0.5, foldThreshold: 0.95}, search.GraphStageRelations, consistencyVariants()[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runBaseline(t.Context(), database, worker, service, contexts, querySet{}, attackSet{}, adversarialConditions{}, newBaseShape(contexts), nil); err == nil || !strings.Contains(err.Error(), "issue85 cleanup rejected") {
		t.Fatalf("기준선 정리 실패 = %v", err)
	}
	var dimension int
	if err := connection.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	if _, err := runAttackRepeat(t.Context(), database, worker, service, contexts, querySet{}, attackSample{}, adversarialConditions{}, store.EmbeddingExpectation{ModelID: worker.ModelID(), Dimension: dimension}, newBaseShape(contexts), nil, nil, &sampleRuns{}); err == nil || !strings.Contains(err.Error(), "issue85 cleanup rejected") {
		t.Fatalf("공격 회차 정리 실패 = %v", err)
	}
}

func TestEvaluationCommandCleanupFailsExitIntegration(t *testing.T) {
	_, _, connection := evalDatabase(t, nil)
	var dimension int
	if err := connection.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	vector := make([]float64, dimension)
	vector[0] = 1
	raw, err := json.Marshal(map[string]any{"embeddings": [][]float64{vector}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	for key, value := range map[string]string{
		"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "AGE_GRAPH_NAME": "agent_context",
		"EMBEDDING_PROVIDER": "ollama", "EMBEDDING_BASE_URL": server.URL, "EMBEDDING_MODEL": "eval-regression",
		"EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": strconv.Itoa(dimension), "GEMINI_API_KEY": "",
		"SEARCH_CHANNEL_CANDIDATE_LIMIT": "500", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "0.95", "EVAL_TEST_COMMAND_PROCESS": "1",
	} {
		t.Setenv(key, value)
	}
	dir := t.TempDir()
	contexts, queries := filepath.Join(dir, "contexts.json"), filepath.Join(dir, "queries.json")
	if err := runConvert(convertOwn, "", "cleanup", 4, "", contexts, queries, ""); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(t *testing.T, mode []string, wantFailure bool) {
		t.Helper()
		args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-contexts=" + contexts, "-queries=" + queries, "-repeat=3", "-budget=20000", "-stages=baseline", "-out=" + filepath.Join(t.TempDir(), "report.json")}
		command := exec.CommandContext(t.Context(), binary, append(args, mode...)...)
		out, err := command.CombinedOutput()
		if !wantFailure {
			if err != nil {
				t.Fatalf("평가 정상 실행 실패: %v\n%s", err, out)
			}
			return
		}
		status, ok := errors.AsType[*exec.ExitError](err)
		if !ok || status.ExitCode() != 1 || !strings.Contains(string(out), "issue85 cleanup rejected") {
			t.Fatalf("정리 실패 종료 = %v\n%s", err, out)
		}
	}
	modes := [][]string{nil, {"-adaptive", "-direct-thresholds=0.5", "-direct-margins=0"}, {"-continual"}, {"-consistency", "-consistency-requests=1", "-consistency-load=1", "-consistency-load-requests=1", "-consistency-write-pauses=1"}}
	for i, mode := range modes {
		t.Run(fmt.Sprintf("normal/%d", i), func(t *testing.T) { invoke(t, mode, false) })
	}
	t.Run("candidate_boundary", func(t *testing.T) {
		t.Setenv("SEARCH_CHANNEL_CANDIDATE_LIMIT", strconv.Itoa(consistencyActiveContexts+1))
		invoke(t, modes[3], false)
	})
	// 후보 상한 검사는 접속·그래프 생성 전에 수행되어야 한다.
	t.Run("candidate_preflight", func(t *testing.T) {
		t.Setenv("SEARCH_CHANNEL_CANDIDATE_LIMIT", strconv.Itoa(consistencyActiveContexts))
		t.Setenv("DATABASE_URL", "postgres://127.0.0.1:1/no-db?sslmode=disable")
		command := exec.CommandContext(t.Context(), binary, "-test.run=^TestEvalCommandProcess$", "--", "-consistency", "-contexts="+contexts, "-queries="+queries)
		out, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "최대 활성 컨텍스트") {
			t.Fatalf("DB 접속 전 전제 검사 = %v\n%s", err, out)
		}
	})
	rejectEvalCleanup(t, connection)
	for i, mode := range modes {
		t.Run(fmt.Sprintf("cleanup_failure/%d", i), func(t *testing.T) { invoke(t, mode, true) })
	}
}

func TestConsistencyUnmeasuredResourcesIntegration(t *testing.T) {
	scenario := evalScenario(t)
	loaded := settings{candidateLimit: 50, semanticThreshold: 0.7, foldThreshold: 0.95}
	conditions := consistencyConditions{Requests: 2, Budget: 20000, MaxHops: 2, MaxHopNodes: 100}
	for _, variant := range consistencyVariants() {
		service, err := consistencyService(scenario.database, scenario.worker, loaded, search.GraphStageRelations, variant)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		failed, err := scenario.measure(ctx, service, variant, conditions, time.Millisecond)
		if err != nil || failed.Errors != conditions.Requests || failed.Connections != undefinedMetric || failed.PeakConnections != -1 || failed.HoldMS.Mean != undefinedMetric {
			t.Fatalf("전 요청 실패의 미측정 자원 = %+v, %v", failed, err)
		}
		measured, err := scenario.measure(t.Context(), service, variant, conditions, time.Millisecond)
		if err != nil || measured.Errors != 0 || measured.Connections < 0 || measured.PeakConnections < 0 {
			t.Fatalf("측정 자원 = %+v, %v", measured, err)
		}
		if variant.Consistency != search.ConsistencySnapshot && measured.HoldMS.Mean != undefinedMetric {
			t.Fatalf("Read Committed의 스냅숏 시간 = %+v", measured.HoldMS)
		}
		if variant.Consistency == search.ConsistencySnapshot && measured.HoldMS.Mean < 0 {
			t.Fatalf("스냅숏 시간이 미측정이다: %+v", measured.HoldMS)
		}
	}
}
