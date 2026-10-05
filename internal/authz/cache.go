package authz

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
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

type cacheLoad struct {
	done chan struct{}
	err  error
}

func (load *cacheLoad) wait(ctx context.Context) error {
	select {
	case <-load.done:
		return load.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// verificationCache는 토큰 검증이 요청마다 데이터베이스를 읽지 않도록 서명 키와 폐기
// 목록을 메모리에 둔다.
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
	for {
		cache.mu.Lock()
		if key, found := cache.keys[keyID]; found {
			cache.mu.Unlock()
			return key, nil
		}
		if time.Since(cache.keysLoadedAt) < keyMissTTL {
			cache.mu.Unlock()
			return jose.JSONWebKey{}, store.ErrNotFound
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
		loaded := make(map[string]jose.JSONWebKey, len(items))
		if err == nil {
			for _, item := range items {
				var public jose.JSONWebKey
				if decodeErr := json.Unmarshal([]byte(item.PublicKey), &public); decodeErr != nil {
					err = fmt.Errorf("공개 키 해석: %w", decodeErr)
					break
				}
				loaded[item.ID] = public
			}
		}
		cache.mu.Lock()
		if err == nil {
			maps.Copy(cache.keys, loaded)
			if cache.keysGeneration == generation {
				cache.keysLoadedAt = time.Now()
			}
		}
		load.err = err
		cache.keyLoad = nil
		close(load.done)
		cache.mu.Unlock()
		if err != nil {
			return jose.JSONWebKey{}, err
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
	for {
		cache.mu.Lock()
		if now.Sub(cache.loadedAt) < revocationTTL {
			_, revoked := cache.revoked[tokenID]
			cache.mu.Unlock()
			return revoked, nil
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
		loaded := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			loaded[id] = struct{}{}
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		load.err = err
		cache.revocationLoad = nil
		defer close(load.done)
		if err != nil {
			return false, err
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
