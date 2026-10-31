package authz

import (
	"context"
	"errors"
	"sync"
	"time"

	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
)

// revocationTTL은 폐기 목록을 다시 읽기까지의 간격이다.
//
// 다른 인스턴스의 폐기를 이 간격 안에 반영한다. 로컬 폐기는 간격을 기다리지 않는다.
const revocationTTL = 10 * time.Second

// keyMissTTL은 정상 적재한 목록에 없는 kid의 재조회를 제한한다.
const keyMissTTL = time.Second

// publicKeyTTL은 저장소에서 삭제된 키의 신뢰가 남는 캐시 간격이다.
const publicKeyTTL = 10 * time.Second

// cacheLookupTimeout은 캐시 미스의 대기·재시도·적재를 합산한 예산이다.
const cacheLookupTimeout = 5 * time.Second

type cacheLoad struct {
	done chan struct{}
	err  error
	// retry는 저장소 장애와 구분되는 적재 호출의 취소·마감·예산 소진이다.
	retry bool
}

// cacheContextError는 타이머 콜백이 아직 실행되지 않아도 지난 마감으로 재시도하지 않게 한다.
func cacheContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// cacheLoadResult는 저장소 반환 시점의 오류 분류를 공개 시점까지 보존한다.
type cacheLoadResult struct {
	err   error
	retry bool
}

func captureCacheLoadResult(ctx context.Context, err error) cacheLoadResult {
	cause := cacheContextError(ctx)
	if err == nil {
		err = cause
	}
	return cacheLoadResult{err: err, retry: cause != nil && errors.Is(err, cause)}
}

func (result cacheLoadResult) beforePublish(ctx context.Context) cacheLoadResult {
	// 새 취소는 성공 결과만 무효화하며 기존 저장소 오류의 분류를 바꾸지 않는다.
	if result.err == nil {
		result.err = cacheContextError(ctx)
		result.retry = result.err != nil
	}
	return result
}

func (load *cacheLoad) wait(ctx context.Context) error {
	select {
	case <-load.done:
		if err := cacheContextError(ctx); err != nil {
			return err
		}
		if load.retry {
			return nil
		}
		return load.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// verificationCache는 공개 키와 웹 세션 폐기 목록을 메모리에 둔다.
// Bearer의 개별·계열 폐기는 이 캐시를 사용하지 않는다.
type verificationCache struct {
	mu sync.Mutex
	// keys에는 실제 저장된 공개 키만 둔다. 모르는 kid 자체는 적재하지 않는다.
	keys           map[string]jose.JSONWebKey
	keysLoadedAt   time.Time
	keysGeneration uint64
	keyLoad        *cacheLoad
	// revoked에는 폐기된 토큰 식별자를 둔다. loadedAt이 지나면 다시 읽는다.
	revoked  map[string]struct{}
	loadedAt time.Time
	// generation은 로컬 폐기와 적재마다 증가하여 이전 DB 스냅숏의 덮어쓰기를 막는다.
	generation     uint64
	revocationLoad *cacheLoad
}

func newVerificationCache() *verificationCache {
	return &verificationCache{keys: make(map[string]jose.JSONWebKey), revoked: make(map[string]struct{})}
}

// publicKey는 알려진 공개 키를 돌려주거나, 짧은 재조회 제한 안에서 키 목록을 적재한다.
func (cache *verificationCache) publicKey(ctx context.Context, source authStore, keyID string) (jose.JSONWebKey, error) {
	var cancel context.CancelFunc
	for {
		if err := cacheContextError(ctx); err != nil {
			return jose.JSONWebKey{}, err
		}
		cache.mu.Lock()
		if err := cacheContextError(ctx); err != nil {
			cache.mu.Unlock()
			return jose.JSONWebKey{}, err
		}
		if key, found := cache.keys[keyID]; found && time.Since(cache.keysLoadedAt) < publicKeyTTL {
			cache.mu.Unlock()
			return key, nil
		}
		if time.Since(cache.keysLoadedAt) < keyMissTTL {
			cache.mu.Unlock()
			return jose.JSONWebKey{}, store.ErrNotFound
		}
		if cancel == nil {
			ctx, cancel = context.WithTimeout(ctx, cacheLookupTimeout)
			defer cancel()
		}
		if load := cache.keyLoad; load != nil {
			cache.mu.Unlock()
			if err := load.wait(ctx); err != nil {
				return jose.JSONWebKey{}, err
			}
			continue
		}
		load := &cacheLoad{done: make(chan struct{})}
		cache.keyLoad = load
		generation := cache.keysGeneration
		cache.mu.Unlock()

		items, err := source.SigningKeys(ctx)
		result := captureCacheLoadResult(ctx, err)
		loaded := make(map[string]jose.JSONWebKey, len(items))
		if result.err == nil {
			for _, item := range items {
				public, decodeErr := publicSigningKey(item)
				if decodeErr != nil {
					result.err = decodeErr
					break
				}
				loaded[item.ID] = public
			}
		}
		cache.mu.Lock()
		result = result.beforePublish(ctx)
		if result.err == nil && cache.keysGeneration == generation {
			cache.keys = loaded
			cache.keysLoadedAt = time.Now()
		}
		load.err, load.retry = result.err, result.retry
		cache.keyLoad = nil
		close(load.done)
		cache.mu.Unlock()
		if result.err != nil {
			return jose.JSONWebKey{}, result.err
		}
	}
}

func (cache *verificationCache) invalidateKeys() {
	cache.mu.Lock()
	cache.keysLoadedAt = time.Time{}
	cache.keysGeneration++
	cache.mu.Unlock()
}

// isRevoked는 토큰 식별자가 폐기 목록에 있는지 본다. 목록이 오래되면 먼저 다시 읽는다.
func (cache *verificationCache) isRevoked(ctx context.Context, source authStore, tokenID string, now time.Time) (bool, error) {
	var cancel context.CancelFunc
	for {
		if err := cacheContextError(ctx); err != nil {
			return false, err
		}
		cache.mu.Lock()
		if err := cacheContextError(ctx); err != nil {
			cache.mu.Unlock()
			return false, err
		}
		if now.Sub(cache.loadedAt) < revocationTTL {
			_, revoked := cache.revoked[tokenID]
			cache.mu.Unlock()
			return revoked, nil
		}
		if cancel == nil {
			ctx, cancel = context.WithTimeout(ctx, cacheLookupTimeout)
			defer cancel()
		}
		if load := cache.revocationLoad; load != nil {
			cache.mu.Unlock()
			if err := load.wait(ctx); err != nil {
				return false, err
			}
			continue
		}
		load := &cacheLoad{done: make(chan struct{})}
		cache.revocationLoad = load
		generation := cache.generation
		cache.mu.Unlock()
		ids, err := source.RevokedTokenIDs(ctx, now)
		result := captureCacheLoadResult(ctx, err)
		loaded := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			loaded[id] = struct{}{}
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		result = result.beforePublish(ctx)
		load.err, load.retry = result.err, result.retry
		cache.revocationLoad = nil
		defer close(load.done)
		if result.err != nil {
			return false, result.err
		}
		_, revoked := loaded[tokenID]
		if cache.generation == generation {
			cache.revoked, cache.loadedAt = loaded, now
			cache.generation++
		} else {
			// 조회 중 갱신된 캐시는 보존한다. 로컬 폐기는 적재 시각을 연장하지 않으므로
			// 다음 요청이 다시 읽으며, 이번 요청도 새 캐시와 DB 스냅숏의 폐기를 모두 본다.
			_, latestRevoked := cache.revoked[tokenID]
			revoked = revoked || latestRevoked
		}
		return revoked, nil
	}
}

// invalidateRevocations는 이 인스턴스가 방금 폐기한 토큰이 곧바로 막히게 한다.
// 다른 인스턴스의 폐기는 다음 적재에서 보인다.
func (cache *verificationCache) invalidateRevocations(tokenID string) {
	cache.mu.Lock()
	cache.revoked[tokenID] = struct{}{}
	cache.generation++
	cache.mu.Unlock()
}
