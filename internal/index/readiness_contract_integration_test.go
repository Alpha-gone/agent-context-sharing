package index

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func readinessGraph(t *testing.T, database *store.Store, pool *pgxpool.Pool) (model.ID, model.ID) {
	t.Helper()
	actor, graph := integrationID(t), integrationID(t)
	now := time.Now().UTC()
	if err := database.CreateAccount(t.Context(), store.Account{ID: actor, LoginID: strings.ReplaceAll(actor.String(), "-", ""), PasswordHash: "synthetic-password-hash", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateGraph(t.Context(), model.Graph{ID: graph, Name: "색인 준비 합성", CreatedBy: actor, CreatedAt: now, LastActivityAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	id := integrationID(t)
	value := model.Context{ID: id, GraphID: graph, Layer: model.LayerSource, Body: "합성 준비 대상", RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1,
		Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelAPI, Locator: "api://readiness/" + id.String()}, OccurredAt: now, OriginKind: model.OriginKindExternalContent}}
	if _, err := database.CreateContext(t.Context(), graph, value, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second' WHERE graph_id=$1`, graph.String()); err != nil {
		t.Fatal(err)
	}
	return graph, id
}

func readinessWorker(t *testing.T, database *store.Store, handler http.HandlerFunc) *Worker {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	worker, err := New(database, Config{Provider: "gemini", APIKey: testGeminiKey, BaseURL: address, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 128}, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := worker.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return worker
}

func readinessResponse(t *testing.T, w http.ResponseWriter, dimension int) {
	t.Helper()
	vector := make([]float64, dimension)
	vector[0] = 1
	raw, err := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
	if err != nil {
		t.Error(err)
		return
	}
	_, _ = w.Write(raw)
}

func TestIndexReadinessProviderAcceptanceIntegration(t *testing.T) {
	for _, test := range []struct {
		name                        string
		status, dimension, attempts int
		state                       string
	}{
		{"ready", 200, 128, 0, ""}, {"429", 429, 128, 0, "pending"}, {"503", 503, 128, 0, "pending"},
		{"dimension", 200, 127, 0, "failed"}, {"permanent", 403, 128, 0, "failed"}, {"exhausted", 503, 128, 4, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, pool := newIndexContractDatabase(t)
			graph, _ := readinessGraph(t, database, pool)
			other, _ := readinessGraph(t, database, pool)
			if _, err := pool.Exec(t.Context(), `UPDATE public.index_task SET state='failed',attempts=5,last_error='other graph' WHERE graph_id=$1`, other.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE public.index_task SET attempts=$2 WHERE graph_id=$1`, graph.String(), test.attempts); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			worker := readinessWorker(t, database, func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				r.Body.Close()
				calls.Add(1)
				w.WriteHeader(test.status)
				if test.status == 200 {
					readinessResponse(t, w, test.dimension)
				} else {
					_, _ = w.Write([]byte("synthetic private marker"))
				}
			})
			found, err := worker.RunOnceInGraph(t.Context(), graph)
			if err != nil || !found {
				t.Fatalf("색인 회차=%t: %v", found, err)
			}
			ready := database.CheckGraphIndexReady(t.Context(), graph, worker.ModelID())
			if test.state == "" {
				if ready != nil {
					t.Fatal(ready)
				}
			} else {
				if ready == nil {
					t.Fatal("미완료 색인을 준비 완료로 판정했다")
				}
				for _, marker := range []string{testGeminiKey, "synthetic private marker", "https://"} {
					if strings.Contains(ready.Error(), marker) {
						t.Fatal("준비 오류가 원문을 노출했다")
					}
				}
				var state string
				var attempts int
				if err := pool.QueryRow(t.Context(), `SELECT state,attempts FROM public.index_task WHERE graph_id=$1`, graph.String()).Scan(&state, &attempts); err != nil || state != test.state || attempts != test.attempts+1 {
					t.Fatalf("실패 상태=%s attempts=%d: %v", state, attempts, err)
				}
				if found, err := worker.RunOnceInGraph(t.Context(), graph); err != nil || found {
					t.Fatalf("재시도 대기·실패 확보=%t: %v", found, err)
				}
				if database.CheckGraphIndexReady(t.Context(), graph, worker.ModelID()) == nil {
					t.Fatal("빈 확보 회차를 완료로 오인했다")
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("제공자 호출=%d, 기대=1", calls.Load())
			}
			var state string
			var attempts int
			if err := pool.QueryRow(t.Context(), `SELECT state,attempts FROM public.index_task WHERE graph_id=$1`, other.String()).Scan(&state, &attempts); err != nil || state != "failed" || attempts != 5 {
				t.Fatal("다른 그래프 상태를 변경했다")
			}
		})
	}
}

func TestIndexReadinessDelayAndCancellationIntegration(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "publish"
		if stop {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			database, pool := newIndexContractDatabase(t)
			graph, _ := readinessGraph(t, database, pool)
			entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			var calls atomic.Int64
			worker := readinessWorker(t, database, func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				r.Body.Close()
				calls.Add(1)
				close(entered)
				select {
				case <-release:
					readinessResponse(t, w, 128)
				case <-r.Context().Done():
					close(canceled)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			done := make(chan error, 1)
			go func() { _, err := worker.RunOnceInGraph(ctx, graph); done <- err }()
			t.Cleanup(func() {
				cancel()
				unblock()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("색인 회차 종료 실패")
				}
			})
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if database.CheckGraphIndexReady(t.Context(), graph, worker.ModelID()) == nil {
				t.Fatal("유효 리스를 준비 완료로 판정했다")
			}
			if found, err := worker.RunOnceInGraph(ctx, graph); err != nil || found {
				t.Fatalf("다른 작업자의 유효 리스 확보=%t: %v", found, err)
			}
			if stop {
				cancel()
			} else {
				unblock()
			}
			var err error
			select {
			case err = <-done:
				done <- err
			case <-time.After(5 * time.Second):
				t.Fatal("색인 종료 대기 초과")
			}
			if stop {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("취소 원인=%v", err)
				}
				select {
				case <-canceled:
				case <-time.After(2 * time.Second):
					t.Fatal("TLS 요청 취소가 전달되지 않았다")
				}
				if database.CheckGraphIndexReady(t.Context(), graph, worker.ModelID()) == nil {
					t.Fatal("취소 뒤 남은 리스를 완료로 판정했다")
				}
			} else if err != nil {
				t.Fatal(err)
			} else if err := database.CheckGraphIndexReady(t.Context(), graph, worker.ModelID()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("지연 회차에서 중복 제공자 호출이 발생했다")
			}
		})
	}
}
