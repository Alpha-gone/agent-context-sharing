package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type workerFunc func(context.Context) error

func (closing workerFunc) Close(ctx context.Context) error { return closing(ctx) }

func TestShutdownPreservesRequestAndWorkerErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := newApplication(&fakeReadiness{}, logger, proxyTransport(), nil)
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	defer server.Close()
	var requests sync.WaitGroup
	defer requests.Wait()
	defer close(release)
	requests.Go(func() {
		response, err := http.Get(server.URL)
		if err == nil {
			response.Body.Close()
		}
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("진행 요청이 시작되지 않았다")
	}
	requestCtx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	workerErr := errors.New("작업자 종료 실패")
	err := shutdownInOrder(requestCtx, t.Context(), app, server.Config, logger,
		workerFunc(func(context.Context) error { return workerErr }))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, workerErr) {
		t.Fatalf("요청 또는 작업자 오류가 누락됐다: %v", err)
	}
}

func TestCloseWorkersSharesBudgetAndContinuesAfterTimeout(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var firstDeadline, secondDeadline time.Time
	secondCalled := false
	secondErr := errors.New("후행 작업자 종료 실패")
	err := closeWorkers(ctx, logger,
		workerFunc(func(ctx context.Context) error {
			firstDeadline, _ = ctx.Deadline()
			<-ctx.Done()
			return ctx.Err()
		}),
		workerFunc(func(ctx context.Context) error {
			secondCalled = true
			secondDeadline, _ = ctx.Deadline()
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Error("후행 작업자에 공유 기한 초과가 전달되지 않았다")
			}
			return secondErr
		}))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, secondErr) || !secondCalled {
		t.Fatalf("기한 초과 뒤 작업자 종료 결과 = %v, 후행 호출 = %t", err, secondCalled)
	}
	if firstDeadline.IsZero() || !firstDeadline.Equal(secondDeadline) {
		t.Fatalf("작업자 종료 예산이 공유되지 않았다: %s, %s", firstDeadline, secondDeadline)
	}
}

func TestServerResourceCleanupPoolSafety(t *testing.T) {
	workerErr := errors.New("작업자 종료 실패")
	for _, test := range []struct {
		name          string
		poolCloseSafe bool
		workerErr     error
		wantClosed    bool
	}{
		{name: "전체 성공", poolCloseSafe: true, wantClosed: true},
		{name: "작업자 실패", poolCloseSafe: true, workerErr: workerErr},
		{name: "요청 종료 실패"},
		{name: "요청과 작업자 실패", workerErr: workerErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			closed, calls := false, 0
			err := closeServerResources(t.Context(), logger, func() { closed = true }, test.poolCloseSafe,
				workerFunc(func(context.Context) error { calls++; return test.workerErr }))
			if closed != test.wantClosed || calls != 1 || !errors.Is(err, test.workerErr) {
				t.Fatalf("자원 정리: 풀 종료 %t, 작업자 호출 %d, 오류 %v", closed, calls, err)
			}
		})
	}
}

func TestStartupCleanupPreservesOriginalAndWorkerErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	startupErr, workerErr := errors.New("기동 실패"), errors.New("작업자 종료 실패")
	closed := false
	run := func() (resultErr error) {
		defer func() {
			resultErr = errors.Join(resultErr, closeServerResources(t.Context(), logger, func() { closed = true }, true,
				workerFunc(func(context.Context) error { return workerErr })))
		}()
		return startupErr
	}
	err := run()
	if !errors.Is(err, startupErr) || !errors.Is(err, workerErr) || closed {
		t.Fatalf("기동 실패 정리: 오류 %v, 풀 종료 %t", err, closed)
	}
}

func TestNormalShutdownDoesNotRetryWorkersDuringCleanup(t *testing.T) {
	workerErr := errors.New("작업자 종료 실패")
	for _, test := range []struct {
		name    string
		failure error
	}{{name: "성공"}, {name: "실패", failure: workerErr}} {
		t.Run(test.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			app := newApplication(&fakeReadiness{}, logger, proxyTransport(), nil)
			calls, closed := 0, false
			workers := []worker{workerFunc(func(context.Context) error { calls++; return test.failure })}
			err := shutdownInOrder(t.Context(), t.Context(), app, &http.Server{}, logger, workers...)
			workers = nil
			cleanupErr := closeServerResources(t.Context(), logger, func() { closed = true }, err == nil, workers...)
			if calls != 1 || closed != (test.failure == nil) || cleanupErr != nil || !errors.Is(err, test.failure) {
				t.Fatalf("정상 종료 정리: 호출 %d, 풀 종료 %t, 오류 %v, 정리 오류 %v", calls, closed, err, cleanupErr)
			}
		})
	}
}
