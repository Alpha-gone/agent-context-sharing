package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

type indexerRoundTrip func(*http.Request) (*http.Response, error)

func (f indexerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRINF05ExternalIndexerConsumesQueueIntegration(t *testing.T) {
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("시험 DB가 필요하다")
		}
		t.Skip("시험 DB 미설정")
	}
	pool, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	id, _ := model.NewID()
	graph, _ := model.NewID()
	node, _ := model.NewID()
	account, _ := model.NewID()
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.relation_inference_task(task_id,graph_id,context_id,target_version,trigger_account_id,attempts,output_attempts,state,enqueued_at,next_attempt_at,correlation_id) VALUES($1,$2,$3,1,$4,0,0,'pending',statement_timestamp(),statement_timestamp(),'')`, id.String(), graph.String(), node.String(), account.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, `DELETE FROM public.relation_inference_task WHERE task_id=$1`, id.String()); err != nil {
			t.Error(err)
		}
	})
	data, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			values[key] = strings.Trim(value, "'")
		}
	}
	local, err := os.MkdirTemp(".", ".rinf05-indexer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(local); err != nil {
			t.Error(err)
		}
	})
	logDir, err := filepath.Abs(filepath.Join(local, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"DATABASE_URL": address, "GEMINI_API_KEY": "synthetic-key", "INDEX_WORKER_PLACEMENT": "external", "DIAGNOSTIC_LOG_DIR": logDir, "RELATION_INFERENCE_MODE": "shadow", "RELATION_INFERENCE_PROVIDER": "openai", "RELATION_INFERENCE_MODEL": "gpt-6-luna", "RELATION_INFERENCE_BASE_URL": "https://provider.test/v1", "RELATION_INFERENCE_API_KEY": "synthetic-inference-key"} {
		values[key] = value
	}
	cfg, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	client := &http.Client{Transport: indexerRoundTrip(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected provider call") })}
	go func() { done <- runConfiguredWithClient(ctx, cfg, client) }()
	deadline := time.Now().Add(15 * time.Second)
	var attempts int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(t.Context(), `SELECT attempts FROM public.relation_inference_task WHERE task_id=$1`, id.String()).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("작업자 조기 종료: %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("분리 실행기 종료 시간 초과")
	}
	if attempts == 0 {
		t.Fatal("분리 실행기가 추론 작업을 확보하지 않았다")
	}
}
