package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

func TestWorkerShutdownTimeoutSkipsHeldConnectionPoolIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_URL이 필요하다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 연결 유지 종료 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	for _, mode := range []string{"정상 종료", "기동 실패"} {
		t.Run(mode, func(t *testing.T) {
			database, err := store.New(t.Context(), databaseURL, graphName, nil, nil, store.IndexTargetsAllLayers)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			periodic, err := newPeriodicWorker(database, plan.AccountPlans{}, logger)
			if err != nil {
				t.Fatal(err)
			}
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			periodic.tasks = []periodicTask{{name: "shutdown_test", interval: time.Hour, lockKey: 4182026106106, run: func(ctx context.Context) (int, error) {
				// 실제 세션 잠금 연결을 유지하며 취소 뒤에도 회차를 끝내지 않는다.
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release
				return 0, ctx.Err()
			}}}
			periodic.Start(t.Context())
			var executions sync.WaitGroup
			defer func() {
				unblock()
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
				defer cancel()
				if err := periodic.Close(ctx); err != nil {
					t.Errorf("시험 작업자 정리: %v", err)
				}
				executions.Wait()
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("실제 DB 잠금을 가진 회차가 시작되지 않았다")
			}
			var poolClosed atomic.Bool
			closePool := func() { poolClosed.Store(true); database.Close() }
			startupErr := errors.New("시험 기동 실패")
			results := make(chan error, 1)
			executions.Go(func() {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				if mode == "기동 실패" {
					run := func() (resultErr error) {
						defer func() {
							resultErr = errors.Join(resultErr, closeServerResources(ctx, logger, closePool, true, periodic))
						}()
						return startupErr
					}
					results <- run()
					return
				}
				app := newApplication(database, logger, proxyTransport(), nil)
				shutdownErr := shutdownInOrder(t.Context(), ctx, app, &http.Server{}, logger, periodic)
				// 실제 조립 경로처럼 이미 종료를 시도한 작업자는 다시 닫지 않는다.
				results <- errors.Join(shutdownErr, closeServerResources(ctx, logger, closePool, shutdownErr == nil))
			})
			select {
			case err := <-results:
				if !errors.Is(err, context.DeadlineExceeded) || (mode == "기동 실패" && !errors.Is(err, startupErr)) {
					t.Fatalf("종료 실패 원인이 누락됐다: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("반환되지 않은 연결 때문에 종료 실패 보고가 막혔다")
			}
			if poolClosed.Load() {
				t.Fatal("끝나지 않은 작업자가 있어도 풀 종료를 호출했다")
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("작업자의 실행 취소가 전파되지 않았다")
			}
			select {
			case <-periodic.done:
				t.Fatal("연결을 유지한 작업자가 시험 해제 전에 종료됐다")
			default:
			}
		})
	}
}
