package authz

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type cacheCancellationStore struct {
	*memoryStore
	kind  string
	reads atomic.Int64
	first func(context.Context) error
}

func (s *cacheCancellationStore) SigningKeys(ctx context.Context) ([]store.SigningKey, error) {
	if s.kind == "keys" && s.reads.Add(1) == 1 {
		if err := s.first(ctx); err != nil {
			return nil, err
		}
	}
	return s.memoryStore.SigningKeys(ctx)
}
func (s *cacheCancellationStore) RevokedTokenIDs(ctx context.Context, at time.Time) ([]string, error) {
	if s.kind == "revocations" && s.reads.Add(1) == 1 {
		if err := s.first(ctx); err != nil {
			return nil, err
		}
	}
	return s.memoryStore.RevokedTokenIDs(ctx, at)
}

func cancellationService(t *testing.T, kind string, first func(context.Context) error) (*cacheCancellationStore, *Service, string) {
	t.Helper()
	backend := &cacheCancellationStore{memoryStore: newMemoryStore(), kind: kind, first: first}
	service := testService(t, backend)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.issue(t.Context(), id, "web", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return backend, service, token.Raw
}

// Done의 평가로 실제 단일 적재 대기에 진입했음을 확인한다. sleep으로 경쟁을 추측하지 않는다.
type cacheWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *cacheWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}
func awaitCacheSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("cache operation did not reach barrier")
	}
}
func awaitCacheResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("cache operation did not finish")
		return nil
	}
}
func cacheWaiters(t *testing.T, service *Service, raw string, n int) <-chan error {
	t.Helper()
	result := make(chan error, n)
	for range n {
		ctx := &cacheWaitContext{Context: t.Context(), entered: make(chan struct{})}
		go func() { _, _, err := service.Verify(ctx, raw, "web"); result <- err }()
		awaitCacheSignal(t, ctx.entered)
	}
	return result
}

func TestCacheLoaderCancellationDoesNotFailWaiters(t *testing.T) {
	for _, kind := range []string{"keys", "revocations"} {
		for _, mode := range []string{"cancel", "deadline"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					started := make(chan struct{})
					backend, service, raw := cancellationService(t, kind, func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() })
					ctx, cancel := context.WithCancel(t.Context())
					want := context.Canceled
					if mode == "deadline" {
						cancel()
						ctx, cancel = context.WithTimeout(t.Context(), time.Second)
						want = context.DeadlineExceeded
					}
					defer cancel()
					leader := make(chan error, 1)
					go func() { _, _, err := service.Verify(ctx, raw, "web"); leader <- err }()
					awaitCacheSignal(t, started)
					waiting := cacheWaiters(t, service, raw, 3)
					if mode == "cancel" {
						cancel()
					}
					if err := awaitCacheResult(t, leader); !errors.Is(err, want) || errors.Is(err, ErrUnavailable) {
						t.Fatalf("loader cancellation=%v want=%v", err, want)
					}
					for range 3 {
						if err := awaitCacheResult(t, waiting); err != nil {
							t.Errorf("healthy waiter inherited loader error: %v", err)
						}
					}
					if got := backend.reads.Load(); got != 2 {
						t.Fatalf("source reads=%d want=2", got)
					}

				})
			})
		}
	}
}

func TestCacheStoreFailuresRemainShared(t *testing.T) {
	for _, kind := range []string{"keys", "revocations"} {
		for _, cause := range []error{errStoreUnavailable, context.Canceled, context.DeadlineExceeded} {
			t.Run(kind+"/"+cause.Error(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					started, release := make(chan struct{}), make(chan struct{})
					stop := sync.OnceFunc(func() { close(release) })
					defer stop()
					backend, service, raw := cancellationService(t, kind, func(ctx context.Context) error {
						close(started)
						select {
						case <-release:
							return cause
						case <-ctx.Done():
							return ctx.Err()
						}
					})
					leader := make(chan error, 1)
					go func() { _, _, err := service.Verify(t.Context(), raw, "web"); leader <- err }()
					awaitCacheSignal(t, started)
					waiting := cacheWaiters(t, service, raw, 3)
					stop()
					results := []error{awaitCacheResult(t, leader)}
					for range 3 {
						results = append(results, awaitCacheResult(t, waiting))
					}
					for _, err := range results {
						if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
							t.Errorf("store failure=%v want unavailable + %v", err, cause)
						}
					}
					if got := backend.reads.Load(); got != 1 {
						t.Fatalf("store failure retried unexpectedly: %d", got)
					}

				})
			})
		}
	}
}

func TestCacheLoadHasIndependentTimeoutAndRecovers(t *testing.T) {
	for _, kind := range []string{"keys", "revocations"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				deadlines := make(chan time.Duration, 1)
				backend, service, raw := cancellationService(t, kind, func(ctx context.Context) error {
					deadline, ok := ctx.Deadline()
					duration := time.Duration(0)
					if ok {
						duration = time.Until(deadline)
					}
					deadlines <- duration
					<-ctx.Done()
					return ctx.Err()
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, _, err := service.Verify(ctx, raw, "web"); result <- err }()
				var duration time.Duration
				select {
				case duration = <-deadlines:
				case <-time.After(10 * time.Second):
					t.Fatal("source did not start")
				}
				if duration <= 0 || duration > 5*time.Second {
					cancel()
					awaitCacheResult(t, result)
					t.Fatalf("load timeout=%v want <=5s", duration)
				}
				waiting := cacheWaiters(t, service, raw, 3)
				if err := awaitCacheResult(t, result); !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("load timeout failure=%v", err)
				}
				for range 3 {
					if err := awaitCacheResult(t, waiting); !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("waiter timeout failure=%v", err)
					}
				}
				if ctx.Err() != nil {
					t.Fatal("load timeout canceled caller")
				}
				if _, _, err := service.Verify(t.Context(), raw, "web"); err != nil || backend.reads.Load() != 2 {
					t.Fatalf("recovery reads=%d: %v", backend.reads.Load(), err)
				}

			})
		})
	}
}
