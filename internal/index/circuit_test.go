package index

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderCircuitRetryAfterAndBackoff(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		raw  string
		want time.Duration
	}{
		{"120", 2 * time.Minute}, {now.Add(3 * time.Minute).Format(http.TimeFormat), 3 * time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0}, {"-1", 0}, {"+1", 0}, {"bad", 0},
		{"", 0}, {"0", 0}, {"999999999999999999999999999999999", maxProviderRetryAfter},
		{now.Add(48 * time.Hour).Format(http.TimeFormat), maxProviderRetryAfter},
	} {
		if got := parseRetryAfter(sample.raw, now); got != sample.want {
			t.Errorf("Retry-After %q: %v want %v", sample.raw, got, sample.want)
		}
	}
	worker := testWorker(t, nil)
	worker.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	worker.providerNow = func() time.Time { return now }
	for i := range 20 {
		worker.blockProvider(t.Context(), 503, ProviderError{Retryable: true})
		ceiling := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute}[min(i, 4)]
		if delay := worker.providerCircuit.retryAt.Sub(now); delay < ceiling/2 || delay > ceiling {
			t.Fatal("지터·백오프 상한", delay)
		}
	}
	worker.blockProvider(t.Context(), 429, ProviderError{RetryAfter: 2 * time.Hour})
	if delay := worker.providerCircuit.retryAt.Sub(now); delay != 2*time.Hour {
		t.Fatal("Retry-After 하한 무시", delay)
	}
}

func TestProviderCircuitFailureClassification(t *testing.T) {
	for _, status := range []uint16{401, 403, 408, 429, 500, 503, 599} {
		if !providerWideFailure(t.Context(), status, ProviderError{}) {
			t.Fatal("제공자 장애 미분류", status)
		}
	}
	for _, err := range []error{nil, DimensionError{}, ProviderError{}, context.Canceled, errors.New("local failure")} {
		if providerWideFailure(t.Context(), 200, err) {
			t.Fatal("작업별·취소 오류를 제공자 장애로 분류", err)
		}
	}
	if !providerWideFailure(t.Context(), 0, ProviderError{Retryable: true}) || !providerWideFailure(t.Context(), 0, context.DeadlineExceeded) {
		t.Fatal("전송·timeout 미분류")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if providerWideFailure(ctx, 503, ProviderError{}) {
		t.Fatal("요청 취소를 차단 원인으로 사용")
	}
}

func TestProviderProbeCancellationAndQueryIsolation(t *testing.T) {
	t.Run("probe_cancel", func(t *testing.T) {
		var calls atomic.Int64
		release := make(chan struct{})
		entered := make(chan struct{})
		defer close(release)
		worker, _ := geminiWorker(t, func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			calls.Add(1)
			close(entered)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		worker.providerCircuit = providerCircuit{failures: 1, retryAt: time.Now().Add(-time.Minute)}
		before := worker.providerCircuit
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			found, err := worker.RunOnce(ctx)
			if found {
				err = errors.New("탐침 취소 중 작업 확보")
			}
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("탐침 미진입")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("탐침 취소 경계", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("탐침 취소 미반영")
		}
		if calls.Load() != 1 || worker.providerCircuit != before {
			t.Fatal("취소가 차단을 갱신하거나 큐 확보로 진행했다")
		}
	})
	t.Run("query_and_single_cycle", func(t *testing.T) {
		var calls atomic.Int64
		worker, _ := geminiWorker(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); readinessResponse(t, w, 128) })
		worker.providerCircuit = providerCircuit{failures: 1, retryAt: time.Now().Add(time.Hour)}
		if _, err := worker.Embed(t.Context(), "synthetic query"); err != nil {
			t.Fatal("큐 차단이 질의 임베딩에 적용됐다", err)
		}
		if found, err := worker.RunOnce(t.Context()); found || err != nil {
			t.Fatal("차단 중 확보", found, err)
		}
		worker.providerCircuit = providerCircuit{}
		worker.cycleMu.Lock()
		found, err := worker.RunOnce(t.Context())
		worker.cycleMu.Unlock()
		if found || err != nil || calls.Load() != 1 {
			t.Fatal("동시 회차가 추가 제공자 호출", found, err, calls.Load())
		}
	})
}
