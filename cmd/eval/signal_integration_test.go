package main

import (
	"bytes"
	"context"
	"errors"
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
)

func TestEvaluationCommandSignalCleansGraphIntegration(t *testing.T) {
	_, _, connection := evalDatabase(t, nil)
	var dimension int
	if err := connection.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"basic", "index", "adaptive", "continual", "consistency", "adversarial", "units"} {
		t.Run(mode, func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(unblock)
			trustEvalEmbeddingServer(t, server)
			for key, value := range map[string]string{
				"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "AGE_GRAPH_NAME": "agent_context",
				"EMBEDDING_PROVIDER": "gemini", "EMBEDDING_BASE_URL": server.URL, "EMBEDDING_MODEL": "gemini-embedding-2",
				"EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": strconv.Itoa(dimension), "GEMINI_API_KEY": "synthetic-key",
				"SEARCH_CHANNEL_CANDIDATE_LIMIT": "500", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "0.95", "EVAL_TEST_COMMAND_PROCESS": "1",
			} {
				t.Setenv(key, value)
			}
			dir := t.TempDir()
			contexts, queries, attacks := filepath.Join(dir, "contexts.json"), filepath.Join(dir, "queries.json"), filepath.Join(dir, "attacks.json")
			if err := runConvert(convertOwnAdversarial, "", "signal-"+mode, 12, "", contexts, queries, attacks); err != nil {
				t.Fatal(err)
			}
			var before int
			if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM public.context_graph WHERE deleted_at IS NULL`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(dir, "report.json")
			args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-contexts=" + contexts, "-queries=" + queries, "-stages=baseline", "-out=" + outPath}
			if mode == "index" {
				args = append(args, "-compare-index-targets")
			} else if mode != "basic" {
				args = append(args, "-"+mode)
			}
			if mode == "adversarial" {
				args = append(args, "-attacks="+attacks)
			}
			if mode == "units" {
				input := unitFixture(t, 1)
				input.Search.EmbeddingModel = "gemini:gemini-embedding-2:retrieval-v1:vector:" + strconv.Itoa(dimension)
				input.Search.CandidateLimit = 500
				refreshUnitHashes(&input)
				args = []string{"-test.run=^TestEvalCommandProcess$", "--", "-unit-comparison=" + writeManifestFixture(t, input), "-out=" + outPath}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, args...)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("취소 전에 실행이 끝났다: %v\n%s", err, output.String())
			case <-ctx.Done():
				<-done
				t.Fatal(ctx.Err())
			}
			if err := command.Process.Signal(os.Interrupt); err != nil {
				cancel()
				<-done
				t.Fatal(err)
			}
			err = <-done
			status, ok := errors.AsType[*exec.ExitError](err)
			if !ok || status.ExitCode() != 1 || !strings.Contains(output.String(), "context canceled") {
				t.Fatalf("신호 취소가 정리 경계를 거치지 않았다: %v\n%s", err, output.String())
			}
			var after int
			if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM public.context_graph WHERE deleted_at IS NULL`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatalf("취소 뒤 활성 그래프 잔류: before=%d after=%d", before, after)
			}
			if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("취소된 평가가 정상 결과를 기록했다: %v", err)
			}
		})
	}
}
