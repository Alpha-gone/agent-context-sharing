package authz

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/store"
)

type blockedKeyStore struct {
	*memoryStore
	reads            atomic.Int64
	started, release chan struct{}
}

func (backend *blockedKeyStore) SigningKeys(ctx context.Context) ([]store.SigningKey, error) {
	if backend.reads.Add(1) == 1 {
		close(backend.started)
		select {
		case <-backend.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return backend.memoryStore.SigningKeys(ctx)
}

func TestUnknownKeyIDsShareBoundedNegativeCache(t *testing.T) {
	backend := &blockedKeyStore{memoryStore: newMemoryStore(), started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(backend.release) })
	t.Cleanup(release)
	cache := newVerificationCache()
	var workers sync.WaitGroup
	results := make(chan error, 40)
	for index := range 40 {
		workers.Go(func() {
			_, err := cache.publicKey(t.Context(), backend, fmt.Sprintf("attacker-%d", index))
			results <- err
		})
	}
	select {
	case <-backend.started:
	case <-time.After(5 * time.Second):
		t.Fatal("키 목록 조회가 시작되지 않았다")
	}
	// 대기 요청의 취소가 진행 중인 공유 조회를 취소하지 않는다.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.publicKey(ctx, backend, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("대기 취소 = %v", err)
	}
	release()
	workers.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("모르는 kid = %v", err)
		}
	}
	if backend.reads.Load() != 1 || len(cache.keys) != 0 {
		t.Fatalf("조회 %d회, 키 캐시 %d개", backend.reads.Load(), len(cache.keys))
	}
	if _, err := cache.publicKey(t.Context(), backend, "another-attacker"); !errors.Is(err, store.ErrNotFound) || backend.reads.Load() != 1 {
		t.Fatalf("음성 캐시 = %v, 조회 %d회", err, backend.reads.Load())
	}
	cache.keysLoadedAt = time.Now().Add(-keyMissTTL)
	if _, err := cache.publicKey(t.Context(), backend, "retry"); !errors.Is(err, store.ErrNotFound) || backend.reads.Load() != 2 {
		t.Fatalf("재조회 = %v, 조회 %d회", err, backend.reads.Load())
	}
}

func TestFailedKeyReloadDoesNotCacheAbsence(t *testing.T) {
	backend := &failingStore{memoryStore: newMemoryStore(), signingKeyFails: true}
	cache := newVerificationCache()
	if _, err := cache.publicKey(t.Context(), backend, "unknown"); !errors.Is(err, errStoreUnavailable) {
		t.Fatal(err)
	}
	if !cache.keysLoadedAt.IsZero() {
		t.Fatal("저장소 장애를 정상 적재로 기록했다")
	}
	backend.signingKeyFails = false
	if _, err := cache.publicKey(t.Context(), backend, "unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestExternalKeyRotationAppearsAfterNegativeCacheWindow(t *testing.T) {
	backend := &countingStore{memoryStore: newMemoryStore()}
	cache := newVerificationCache()
	if _, err := cache.publicKey(t.Context(), backend, "unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	key, err := newSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.RotateSigningKey(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.publicKey(t.Context(), backend, key.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("재조회 제한 전에 새 키를 조회했다")
	}
	cache.keysLoadedAt = time.Now().Add(-keyMissTTL)
	if _, err := cache.publicKey(t.Context(), backend, key.ID); err != nil {
		t.Fatal(err)
	}
	if backend.signingKeyReads.Load() != 2 {
		t.Fatalf("목록 조회 = %d", backend.signingKeyReads.Load())
	}
}
