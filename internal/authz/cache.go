package authz

import (
	"context"
	"errors"
	"sync"
	"time"

	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
)

// revocationTTL은 토큰별 폐기 상태를 다시 읽기까지의 간격이다.
//
// 다른 인스턴스의 폐기를 이 간격 안에 반영한다. 로컬 폐기는 간격을 기다리지 않는다.
const revocationTTL = 10 * time.Second

const maxRevocationEntries = 4096

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

type revocationEntry struct {
	revoked   bool
	checkedAt time.Time
}

type revocationLoad struct {
	*cacheLoad
	localRevoked bool
}

// verificationCache는 공개 키와 크기가 제한된 웹 토큰별 폐기 판정을 메모리에 둔다.
// Bearer의 개별·계열 폐기는 이 캐시를 사용하지 않는다.
type verificationCache struct {
	mu sync.Mutex
	// keys에는 실제 저장된 공개 키만 둔다. 모르는 kid 자체는 적재하지 않는다.
	keys            map[string]jose.JSONWebKey
	keysLoadedAt    time.Time
	keysGeneration  uint64
	keyLoad         *cacheLoad
	revoked         map[string]revocationEntry
	revocationOrder []string
	nextRevocation  int
	// 적재 중인 항목은 같은 토큰의 대기자만 공유하며 완료/실패 때 제거한다.
	revocationLoads map[string]*revocationLoad
}

func newVerificationCache() *verificationCache {
	return &verificationCache{keys: make(map[string]jose.JSONWebKey), revoked: make(map[string]revocationEntry), revocationLoads: make(map[string]*revocationLoad)}
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

// isRevoked는 토큰 하나만 조회하며 정상 결과를 짧게 보관한다.
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
		if entry, found := cache.revoked[tokenID]; found && now.Sub(entry.checkedAt) < revocationTTL {
			cache.mu.Unlock()
			return entry.revoked, nil
		}
		if cancel == nil {
			ctx, cancel = context.WithTimeout(ctx, cacheLookupTimeout)
			defer cancel()
		}
		if load := cache.revocationLoads[tokenID]; load != nil {
			cache.mu.Unlock()
			if err := load.wait(ctx); err != nil {
				return false, err
			}
			continue
		}
		load := &revocationLoad{cacheLoad: &cacheLoad{done: make(chan struct{})}}
		cache.revocationLoads[tokenID] = load
		cache.mu.Unlock()
		revoked, err := source.IsTokenRevoked(ctx, tokenID, now)
		result := captureCacheLoadResult(ctx, err)
		cache.mu.Lock()
		defer cache.mu.Unlock()
		result = result.beforePublish(ctx)
		load.err, load.retry = result.err, result.retry
		delete(cache.revocationLoads, tokenID)
		defer close(load.done)
		if result.err != nil {
			return false, result.err
		}
		// 로컬 판정이 FIFO에서 축출됐어도 진행 중 조회의 폐기 표시는 남는다.
		revoked = revoked || load.localRevoked
		cache.rememberRevocation(tokenID, revocationEntry{revoked: revoked, checkedAt: now})
		return revoked, nil
	}
}

// invalidateRevocations는 이 인스턴스가 방금 폐기한 토큰이 곧바로 막히게 한다.
// 다른 인스턴스의 폐기는 다음 적재에서 보인다.
func (cache *verificationCache) invalidateRevocations(tokenID string, now time.Time) {
	cache.mu.Lock()
	cache.rememberRevocation(tokenID, revocationEntry{revoked: true, checkedAt: now})
	if load := cache.revocationLoads[tokenID]; load != nil {
		load.localRevoked = true
	}
	cache.mu.Unlock()
}

// 호출자가 mu를 소유한다. 갱신은 순서를 유지하고 새 ID만 가장 오래 삽입된 항목과 교체한다.
func (cache *verificationCache) rememberRevocation(tokenID string, entry revocationEntry) {
	if _, found := cache.revoked[tokenID]; !found {
		if len(cache.revocationOrder) < maxRevocationEntries {
			cache.revocationOrder = append(cache.revocationOrder, tokenID)
		} else {
			delete(cache.revoked, cache.revocationOrder[cache.nextRevocation])
			cache.revocationOrder[cache.nextRevocation] = tokenID
			cache.nextRevocation = (cache.nextRevocation + 1) % maxRevocationEntries
		}
	}
	cache.revoked[tokenID] = entry
}
