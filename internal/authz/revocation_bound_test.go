package authz

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRevocationCacheHasFixedCapacity(t *testing.T) {
	cache := newVerificationCache()
	backend := &countingStore{memoryStore: newMemoryStore()}
	now := time.Now().UTC()
	for i := range 2*maxRevocationEntries + 1 {
		if revoked, err := cache.isRevoked(t.Context(), backend, fmt.Sprintf("token-%d", i), now); err != nil || revoked {
			t.Fatal(revoked, err)
		}
		if len(cache.revoked) > maxRevocationEntries || len(cache.revocationOrder) > maxRevocationEntries {
			t.Fatal("폐기 판정/FIFO 상한 초과")
		}
	}
	if len(cache.revoked) != maxRevocationEntries || len(cache.revocationLoads) != 0 {
		t.Fatal("완료 항목/진행 중 적재 잔류")
	}
	before := backend.revocationReads.Load()
	if _, err := cache.isRevoked(t.Context(), backend, fmt.Sprintf("token-%d", 2*maxRevocationEntries), now); err != nil || backend.revocationReads.Load() != before {
		t.Fatal("최신 항목 캐시 적중 실패", err)
	}
	if _, err := cache.isRevoked(t.Context(), backend, "token-0", now); err != nil || backend.revocationReads.Load() != before+1 {
		t.Fatal("축출 항목 단건 재조회 실패", err)
	}
	if len(cache.revoked) != maxRevocationEntries {
		t.Fatal("축출 뒤 상한 초과")
	}
}

func TestLocalRevocationSurvivesEvictionDuringRead(t *testing.T) {
	backend := &blockedRevocationStore{memoryStore: newMemoryStore(), started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(backend.release) })
	t.Cleanup(release)
	cache := newVerificationCache()
	now := time.Now().UTC()
	result := make(chan error, 1)
	go func() {
		revoked, err := cache.isRevoked(t.Context(), backend, "target", now)
		if err == nil && !revoked {
			err = errors.New("진행 중 적재가 로컬 폐기를 되돌렸다")
		}
		result <- err
	}()
	awaitCacheSignal(t, backend.started)
	if err := backend.RevokeToken(t.Context(), "target", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cache.invalidateRevocations("target", now)
	for i := range maxRevocationEntries {
		if _, err := cache.isRevoked(t.Context(), backend, fmt.Sprintf("other-%d", i), now); err != nil {
			t.Fatal(err)
		}
	}
	cache.mu.Lock()
	_, retained := cache.revoked["target"]
	cache.mu.Unlock()
	if retained {
		t.Fatal("축출 경합 표본 준비 실패")
	}
	release()
	if err := awaitCacheResult(t, result); err != nil {
		t.Fatal(err)
	}
	if revoked, err := cache.isRevoked(t.Context(), backend, "target", now); err != nil || !revoked {
		t.Fatal("로컬 폐기 재공개 실패", err)
	}
	if len(cache.revoked) != maxRevocationEntries || len(cache.revocationLoads) != 0 {
		t.Fatal("축출/적재 정리 상한 위반")
	}
}

type isolatedRevocationFailureStore struct {
	*memoryStore
	started, release chan struct{}
	once             sync.Once
}

func (s *isolatedRevocationFailureStore) IsTokenRevoked(ctx context.Context, id string, now time.Time) (bool, error) {
	if id == "slow" {
		s.once.Do(func() { close(s.started) })
		select {
		case <-s.release:
			return false, errStoreUnavailable
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return s.memoryStore.IsTokenRevoked(ctx, id, now)
}
func TestRevocationLoadsAndFailuresAreTokenScoped(t *testing.T) {
	backend := &isolatedRevocationFailureStore{memoryStore: newMemoryStore(), started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(backend.release) })
	t.Cleanup(release)
	cache := newVerificationCache()
	now := time.Now().UTC()
	result := make(chan error, 1)
	go func() { _, err := cache.isRevoked(t.Context(), backend, "slow", now); result <- err }()
	awaitCacheSignal(t, backend.started)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if revoked, err := cache.isRevoked(ctx, backend, "other", now); err != nil || revoked {
		t.Fatal("다른 토큰이 느린 적재를 기다렸다", err)
	}
	release()
	if err := awaitCacheResult(t, result); !errors.Is(err, errStoreUnavailable) {
		t.Fatal("조회 실패 분류", err)
	}
	if _, found := cache.revoked["slow"]; found {
		t.Fatal("실패를 정상 부재로 캐시했다")
	}
	if revoked, err := cache.isRevoked(t.Context(), backend, "other", now); err != nil || revoked {
		t.Fatal("다른 토큰 캐시에 실패 전파", err)
	}
	if len(cache.revocationLoads) != 0 {
		t.Fatal("실패한 적재 잔류")
	}
}
