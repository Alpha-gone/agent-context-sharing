package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

func TestPeriodicWorkerRunsOnlyAfterAdvisoryLock(t *testing.T) {
	fake := &fakePeriodicStore{acquired: true}
	worker := &periodicWorker{store: fake, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ran := false
	worker.runTask(t.Context(), periodicTask{name: "test", lockKey: 42, run: func(context.Context) (int, error) {
		ran = true
		return 3, nil
	}})
	if !ran || fake.lockKey != 42 {
		t.Fatalf("잠금 뒤 주기 작업 실행 = ran:%t key:%d", ran, fake.lockKey)
	}

	fake.acquired = false
	ran = false
	worker.runTask(t.Context(), periodicTask{name: "test", lockKey: 42, run: func(context.Context) (int, error) {
		ran = true
		return 0, errors.New("실행되면 안 된다")
	}})
	if ran {
		t.Fatal("자문 잠금을 얻지 못한 회차가 실행됐다")
	}
}

func TestPeriodicWorkerTasksMatchDesign(t *testing.T) {
	if _, err := newPeriodicWorker(nil, plan.AccountPlans{}, nil); err == nil {
		t.Fatal("저장소 없는 주기 작업자를 만들었다")
	}
	worker, err := newPeriodicWorker(&store.Store{}, plan.AccountPlans{}, nil)
	if err != nil {
		t.Fatalf("주기 작업자 준비: %v", err)
	}
	want := map[string]time.Duration{
		"grace_expiry":               time.Hour,
		"proposal_cleanup":           24 * time.Hour,
		"audit_cleanup":              24 * time.Hour,
		"revocation_cleanup":         time.Hour,
		"authorization_code_cleanup": time.Hour,
		"request_rate_cleanup":       time.Hour,
	}
	if len(worker.tasks) != len(want) {
		t.Fatalf("주기 작업 수 = %d, want %d", len(worker.tasks), len(want))
	}
	keys := make(map[int64]string, len(worker.tasks))
	for _, task := range worker.tasks {
		if interval, ok := want[task.name]; !ok || task.interval != interval {
			t.Fatalf("주기 작업 %s 주기 = %s, want %s", task.name, task.interval, interval)
		}
		if other, ok := keys[task.lockKey]; ok {
			t.Fatalf("주기 작업 %s와 %s가 같은 잠금 키를 쓴다", task.name, other)
		}
		keys[task.lockKey] = task.name
	}
}

func TestPeriodicWorkerRunsAtStartAndCloseWaitsForUnlock(t *testing.T) {
	fake := &blockingPeriodicStore{started: make(chan struct{})}
	worker := &periodicWorker{store: fake, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), done: make(chan struct{})}
	worker.tasks = []periodicTask{{name: "test", interval: time.Hour, lockKey: 42, run: func(context.Context) (int, error) { return 0, nil }}}
	worker.Start(t.Context())
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("기동 직후 첫 회차가 실행되지 않았다")
	}
	if err := worker.Close(t.Context()); err != nil {
		t.Fatalf("주기 작업자 종료: %v", err)
	}
	if !fake.released.Load() {
		t.Fatal("Close가 잠금 해제를 기다리지 않았다")
	}
	worker.Start(t.Context())
}

func TestPeriodicWorkerStartAfterCloseDoesNothing(t *testing.T) {
	worker := &periodicWorker{store: &fakePeriodicStore{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), done: make(chan struct{})}
	if err := worker.Close(t.Context()); err != nil {
		t.Fatalf("시작 전 종료: %v", err)
	}
	worker.Start(t.Context())
	if err := worker.Close(t.Context()); err != nil {
		t.Fatalf("두 번째 종료: %v", err)
	}
}

// blockingPeriodicStore는 회차가 취소될 때까지 잠금을 쥔 뒤 해제를 표시한다.
type blockingPeriodicStore struct {
	fakePeriodicStore
	started  chan struct{}
	released atomic.Bool
}

func (fake *blockingPeriodicStore) WithTryAdvisoryLock(ctx context.Context, _ int64, _ func(context.Context) (int, error)) (int, bool, error) {
	close(fake.started)
	<-ctx.Done()
	time.Sleep(10 * time.Millisecond)
	fake.released.Store(true)
	return 0, true, ctx.Err()
}

type fakePeriodicStore struct {
	acquired bool
	lockKey  int64
}

func (fake *fakePeriodicStore) WithTryAdvisoryLock(ctx context.Context, key int64, run func(context.Context) (int, error)) (int, bool, error) {
	fake.lockKey = key
	if !fake.acquired {
		return 0, false, nil
	}
	count, err := run(ctx)
	return count, true, err
}

func (*fakePeriodicStore) ExpireGrace(context.Context, time.Time) (int, error) {
	return 0, nil
}

func (*fakePeriodicStore) CleanupExpiredProposals(context.Context, time.Time, store.RetentionDays) (int, error) {
	return 0, nil
}

func (*fakePeriodicStore) CleanupAuditRecords(context.Context, time.Time, store.RetentionDays, store.RetentionDays) (int, error) {
	return 0, nil
}

func (*fakePeriodicStore) CleanupExpiredRevocations(context.Context, time.Time) (int, error) {
	return 0, nil
}

func (*fakePeriodicStore) CleanupExpiredAuthorizationCodes(context.Context, time.Time) (int, error) {
	return 0, nil
}

func (*fakePeriodicStore) CleanupRequestRateWindows(context.Context, time.Time) (int, error) {
	return 0, nil
}
