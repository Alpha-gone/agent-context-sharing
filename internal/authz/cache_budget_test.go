package authz

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestCacheLookupBudgetIncludesRepeatedLoadCancellation(t *testing.T) {
	for _, kind := range []string{"keys", "revocations"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cache := newVerificationCache()
				remaining := make(chan time.Duration, 1)
				backend := &cacheCancellationStore{memoryStore: newMemoryStore(), kind: kind, first: func(ctx context.Context) error {
					deadline, _ := ctx.Deadline()
					remaining <- time.Until(deadline)
					<-ctx.Done()
					return ctx.Err()
				}}
				setLoad := func(load *cacheLoad) {
					if kind == "keys" {
						cache.keyLoad = load
					} else {
						cache.revocationLoad = load
					}
				}
				load := &cacheLoad{done: make(chan struct{}), err: context.Canceled, retry: true}
				setLoad(load)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				start := time.Now()
				go func() {
					var err error
					if kind == "keys" {
						_, err = cache.publicKey(ctx, backend, "missing")
					} else {
						_, err = cache.isRevoked(ctx, backend, "token", time.Now())
					}
					result <- err
				}()
				synctest.Wait()
				// 취소된 두 적재의 완료를 2초 간격으로 공개한 뒤 이번 요청이 적재를 맡는다.
				for cycle := range 2 {
					time.Sleep(2 * time.Second)
					var next *cacheLoad
					if cycle == 0 {
						next = &cacheLoad{done: make(chan struct{}), err: context.DeadlineExceeded, retry: true}
					}
					cache.mu.Lock()
					setLoad(next)
					close(load.done)
					cache.mu.Unlock()
					load = next
					synctest.Wait()
				}
				if got := <-remaining; got != time.Second {
					t.Errorf("remaining read budget=%v want=1s", got)
				}
				if err := awaitCacheResult(t, result); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lookup result=%v", err)
				}
				if elapsed := time.Since(start); elapsed != 5*time.Second {
					t.Errorf("total wait=%v want=5s", elapsed)
				}
				if ctx.Err() != nil || backend.reads.Load() != 1 {
					t.Fatalf("caller canceled or extra read: %v reads=%d", ctx.Err(), backend.reads.Load())
				}
			})
		})
	}
}

func TestCacheWaitPrioritizesCancellationWhenBothReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, cause := range []error{nil, errStoreUnavailable} {
			for _, retry := range []bool{false, true} {
				ctx, cancel := context.WithCancel(t.Context())
				load := &cacheLoad{done: make(chan struct{}), err: cause, retry: retry}
				close(load.done)
				cancel()
				for range 100 {
					if err := load.wait(ctx); !errors.Is(err, context.Canceled) {
						t.Fatalf("completed/canceled wait=%v", err)
					}
				}
			}
		}
	})
}

func TestCacheStoreFailureThenLoaderCancellationStillFailsWaiters(t *testing.T) {
	for _, kind := range []string{"keys", "revocations"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				_, service, raw := cancellationService(t, kind, func(context.Context) error { close(started); <-release; return errStoreUnavailable })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				leader := make(chan error, 1)
				go func() { _, _, err := service.Verify(ctx, raw, "web"); leader <- err }()
				awaitCacheSignal(t, started)
				waiting := cacheWaiters(t, service, raw, 3)
				close(release)
				err := awaitCacheResult(t, leader)
				if !errors.Is(err, ErrUnavailable) || !errors.Is(err, errStoreUnavailable) {
					t.Fatalf("store result=%v", err)
				}
				cancel()
				for range 3 {
					if err := awaitCacheResult(t, waiting); !errors.Is(err, ErrUnavailable) || !errors.Is(err, errStoreUnavailable) {
						t.Errorf("later cancellation hid store failure: %v", err)
					}
				}
			})
		})
	}
}

func TestCacheReadFailureClassificationSurvivesLaterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errStoreUnavailable} {
			ctx, cancel := context.WithCancel(t.Context())
			result := captureCacheLoadResult(ctx, cause)
			cancel()
			result = result.beforePublish(ctx)
			if !errors.Is(result.err, cause) || result.retry {
				t.Fatalf("source result reclassified after cancellation: %+v", result)
			}
			load := &cacheLoad{done: make(chan struct{}), err: result.err, retry: result.retry}
			close(load.done)
			if err := load.wait(t.Context()); !errors.Is(err, cause) {
				t.Errorf("healthy waiter lost source error: %v", err)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		result := captureCacheLoadResult(ctx, nil)
		cancel()
		result = result.beforePublish(ctx)
		if !errors.Is(result.err, context.Canceled) || !result.retry {
			t.Fatal("late cancellation published a successful result")
		}
	})
}

// Done 통지가 비동기인 context에서 마감 시각만 먼저 지난 상황이다.
type pendingCacheDeadline struct {
	context.Context
	at time.Time
}

func (ctx pendingCacheDeadline) Deadline() (time.Time, bool) { return ctx.at, true }

func TestCacheDeadlineStopsBeforeCancellationNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := pendingCacheDeadline{Context: t.Context(), at: time.Now()}
		if ctx.Err() != nil {
			t.Fatal("test parent already canceled")
		}
		result := captureCacheLoadResult(ctx, nil)
		if !errors.Is(result.err, context.DeadlineExceeded) || !result.retry {
			t.Fatalf("elapsed deadline ignored: %+v", result)
		}
		load := &cacheLoad{done: make(chan struct{})}
		close(load.done)
		if err := load.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("completed wait ignored elapsed deadline: %v", err)
		}
		backend := &cacheCancellationStore{memoryStore: newMemoryStore(), kind: "keys", first: func(context.Context) error { t.Error("started source after deadline"); return nil }}
		if _, err := newVerificationCache().publicKey(ctx, backend, "missing"); !errors.Is(err, context.DeadlineExceeded) || backend.reads.Load() != 0 {
			t.Fatalf("expired lookup=%v reads=%d", err, backend.reads.Load())
		}
	})
}
