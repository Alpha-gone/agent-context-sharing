package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

func indexReadinessGraph(t *testing.T, database *store.Store, connection *pgx.Conn) loadedGraph {
	t.Helper()
	graph, err := loadGraph(t.Context(), database, contextSet{Version: "index-readiness", Contexts: []contextSpec{{
		Key: "source", Layer: "source", Body: "평가 색인이 준비되어야 측정을 시작한다.",
		Source: &sourceSpec{Channel: "conversation", Locator: "urn:eval:index-readiness", OccurredAt: time.Now().UTC().Add(-time.Minute), OriginKind: "user_utterance"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := connection.Exec(ctx, `DELETE FROM public.index_task WHERE graph_id = $1`, graph.GraphID.String()); err != nil {
			t.Error(err)
		}
		if err := dropGraph(ctx, database, graph); err != nil {
			t.Error(err)
		}
	})
	if _, err := connection.Exec(t.Context(), `UPDATE public.index_task SET next_attempt_at = now() - interval '1 second' WHERE graph_id = $1`, graph.GraphID.String()); err != nil {
		t.Fatal(err)
	}
	return graph
}

func failingEvalWorker(t *testing.T, database *store.Store, connection *pgx.Conn, status, dimensionDelta int) *index.Worker {
	t.Helper()
	var dimension int
	if err := connection.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	vector := make([]float64, dimension+dimensionDelta)
	vector[0] = 1
	raw, err := json.Marshal(map[string]any{"embeddings": [][]float64{vector}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(raw)
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
	return worker
}

func TestDrainIndexQueueRejectsProviderFailureIntegration(t *testing.T) {
	for _, test := range []struct {
		name           string
		status         int
		attempts       int
		dimensionDelta int
		state          string
		reason         string
	}{
		{name: "429", status: 429, state: "pending", reason: "임베딩 제공자 상태: 429"},
		{name: "503", status: 503, state: "pending", reason: "임베딩 제공자 상태: 503"},
		{name: "permanent", status: 400, state: "failed", reason: "임베딩 제공자 상태: 400"},
		{name: "exhausted", status: 503, attempts: 4, state: "failed", reason: "임베딩 제공자 상태: 503"},
		{name: "dimension", status: 200, dimensionDelta: -1, state: "failed", reason: "임베딩 차원이 다르다"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, _, connection := evalDatabase(t, nil)
			graph := indexReadinessGraph(t, database, connection)
			if _, err := connection.Exec(t.Context(), `UPDATE public.index_task SET attempts = $2 WHERE graph_id = $1`, graph.GraphID.String(), test.attempts); err != nil {
				t.Fatal(err)
			}
			worker := failingEvalWorker(t, database, connection, test.status, test.dimensionDelta)
			err := drainIndexQueue(t.Context(), database, worker, graph.GraphID, 4)
			if err == nil || !strings.Contains(err.Error(), "색인 미완료") || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("미완료 색인 처리 결과: %v", err)
			}
			var state string
			var attempts int
			if err := connection.QueryRow(t.Context(), `SELECT state, attempts FROM public.index_task WHERE graph_id = $1`, graph.GraphID.String()).Scan(&state, &attempts); err != nil {
				t.Fatal(err)
			}
			if state != test.state || attempts != test.attempts+1 {
				t.Fatalf("실패 저장 결과: state=%s, attempts=%d", state, attempts)
			}
		})
	}
}

func TestDrainIndexQueueRejectsOtherWorkerClaimIntegration(t *testing.T) {
	claimed, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	database, worker, connection := evalDatabase(t, func() { close(claimed); <-release })
	graph := indexReadinessGraph(t, database, connection)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var group sync.WaitGroup
	group.Go(func() {
		_, err := worker.RunOnceInGraph(ctx, graph.GraphID)
		done <- err
	})
	t.Cleanup(func() { unblock(); group.Wait() })
	select {
	case <-claimed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := drainIndexQueue(ctx, database, worker, graph.GraphID, 4); err == nil || !strings.Contains(err.Error(), "pending=1") {
		t.Fatalf("다른 작업자의 확보를 완료로 오인했다: %v", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := drainIndexQueue(ctx, database, worker, graph.GraphID, 1); err != nil {
		t.Fatalf("다른 작업자의 완료 뒤 준비 검사: %v", err)
	}
}

func TestDrainIndexQueueRejectsLockedTaskIntegration(t *testing.T) {
	database, worker, connection := evalDatabase(t, nil)
	graph := indexReadinessGraph(t, database, connection)
	tx, err := connection.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	if _, err := tx.Exec(t.Context(), `SELECT task_id FROM public.index_task WHERE graph_id = $1 FOR UPDATE`, graph.GraphID.String()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := drainIndexQueue(ctx, database, worker, graph.GraphID, 4); err == nil || !strings.Contains(err.Error(), "pending=1") {
		t.Fatalf("행 잠금으로 숨겨진 작업의 준비 검사: %v", err)
	}
}

func TestDrainIndexQueueChecksCurrentEmbeddingIntegration(t *testing.T) {
	for _, change := range []string{"missing", "old_model", "pending_with_embedding"} {
		t.Run(change, func(t *testing.T) {
			database, worker, connection := evalDatabase(t, nil)
			graph := indexReadinessGraph(t, database, connection)
			// 마지막 허용 회차에 완료된 작업도 준비 완료여야 한다.
			if err := drainIndexQueue(t.Context(), database, worker, graph.GraphID, 1); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "missing":
				_, err := connection.Exec(t.Context(), `DELETE FROM public.context_embedding WHERE graph_id = $1`, graph.GraphID.String())
				if err != nil {
					t.Fatal(err)
				}
			case "old_model":
				_, err := connection.Exec(t.Context(), `UPDATE public.context_embedding SET model_id = 'old-model' WHERE graph_id = $1`, graph.GraphID.String())
				if err != nil {
					t.Fatal(err)
				}
			case "pending_with_embedding":
				if err := database.ReindexGraph(t.Context(), graph.GraphID); err != nil {
					t.Fatal(err)
				}
				if _, err := connection.Exec(t.Context(), `UPDATE public.index_task SET next_attempt_at = now() + interval '1 hour' WHERE graph_id = $1`, graph.GraphID.String()); err != nil {
					t.Fatal(err)
				}
			}
			err := drainIndexQueue(t.Context(), database, worker, graph.GraphID, 4)
			if err == nil || !strings.Contains(err.Error(), "색인 미완료") {
				t.Fatalf("준비되지 않은 현재 모델의 평가 허용: %v", err)
			}
		})
	}
}

func TestEvaluationCommandRejectsUnreadyIndexIntegration(t *testing.T) {
	_, _, connection := evalDatabase(t, nil)
	var dimension int
	if err := connection.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }))
	t.Cleanup(server.Close)
	for key, value := range map[string]string{
		"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "AGE_GRAPH_NAME": "agent_context",
		"EMBEDDING_PROVIDER": "ollama", "EMBEDDING_BASE_URL": server.URL, "EMBEDDING_MODEL": "eval-regression",
		"EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": strconv.Itoa(dimension), "GEMINI_API_KEY": "",
		"SEARCH_CHANNEL_CANDIDATE_LIMIT": "500", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "0.95", "EVAL_TEST_COMMAND_PROCESS": "1",
	} {
		t.Setenv(key, value)
	}
	dir := t.TempDir()
	contexts, queries, attacks := filepath.Join(dir, "contexts.json"), filepath.Join(dir, "queries.json"), filepath.Join(dir, "attacks.json")
	if err := runConvert(convertOwnAdversarial, "", "index-readiness", 12, "", contexts, queries, attacks); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i, mode := range [][]string{nil, {"-continual"}, {"-adversarial", "-attacks=" + attacks}, {"-consistency"}, {"-adaptive"}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "report.json")
			args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-contexts=" + contexts, "-queries=" + queries, "-repeat=3", "-out=" + outPath}
			out, err := exec.CommandContext(t.Context(), binary, append(args, mode...)...).CombinedOutput()
			status, ok := errors.AsType[*exec.ExitError](err)
			if !ok || status.ExitCode() != 1 || !strings.Contains(string(out), "색인 미완료") {
				t.Fatalf("미완료 색인의 평가 종료: %v\n%s", err, out)
			}
			if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("미완료 색인에서 정상 보고서가 생성됐다: %v", err)
			}
		})
	}
}
