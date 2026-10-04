package authz

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// revocationTTL은 폐기 목록을 다시 읽기까지의 간격이다.
//
// 「토큰 검증」이 JWKS와 폐기 목록을 요청마다 가져오지 않기로 확정했으나 폐기 목록의
// 방식은 남겼다. `kid`처럼 다시 읽을 방아쇠가 없으므로 짧은 간격으로 새로 읽는다. 겹침
// 구간에 폐기된 토큰이 통과할 수 있는데, 「토큰 갱신」이 이미 갱신으로 겹쳐 발급된 토큰을
// 최대 10초 함께 유효로 두므로 같은 크기의 느슨함이다.
const revocationTTL = 10 * time.Second

// verificationCache는 토큰 검증이 요청마다 데이터베이스를 읽지 않도록 서명 키와 폐기
// 목록을 메모리에 둔다.
type verificationCache struct {
	mu sync.RWMutex
	// keys에는 `kid`로 찾는 공개 키를 둔다. 모르는 `kid`를 만나면 그때 다시 읽는다.
	keys map[string]jose.JSONWebKey
	// revoked에는 폐기된 토큰 식별자를 둔다. loadedAt이 지나면 다시 읽는다.
	revoked  map[string]struct{}
	loadedAt time.Time
	// generation은 로컬 폐기와 적재마다 증가하여 이전 DB 스냅숏의 덮어쓰기를 막는다.
	generation uint64
}

func newVerificationCache() *verificationCache {
	return &verificationCache{keys: make(map[string]jose.JSONWebKey), revoked: make(map[string]struct{})}
}

// publicKey는 `kid`에 해당하는 공개 키를 돌려준다. 캐시에 없으면 한 번만 다시 읽는다.
//
// 키 회전을 `kid` 기반 재조회로 처리하는 이유는 주기 갱신보다 정확하기 때문이다. 새 키로
// 서명된 토큰이 처음 도착한 시점이 곧 갱신이 필요한 시점이다.
func (cache *verificationCache) publicKey(ctx context.Context, source authStore, keyID string) (jose.JSONWebKey, error) {
	cache.mu.RLock()
	key, found := cache.keys[keyID]
	cache.mu.RUnlock()
	if found {
		return key, nil
	}
	// 오류를 감싸지 않고 그대로 올린다. 모르는 `kid`와 저장소 장애를 가르는 것은
	// 호출자이며, 여기에서 합치면 그 구분이 사라진다.
	stored, err := source.SigningKey(ctx, keyID)
	if err != nil {
		return jose.JSONWebKey{}, err
	}
	var public jose.JSONWebKey
	if err := json.Unmarshal([]byte(stored.PublicKey), &public); err != nil {
		return jose.JSONWebKey{}, fmt.Errorf("공개 키 해석: %w", err)
	}
	cache.mu.Lock()
	cache.keys[keyID] = public
	cache.mu.Unlock()
	return public, nil
}

// isRevoked는 토큰 식별자가 폐기 목록에 있는지 본다. 목록이 오래되면 먼저 다시 읽는다.
func (cache *verificationCache) isRevoked(ctx context.Context, source authStore, tokenID string, now time.Time) (bool, error) {
	cache.mu.RLock()
	fresh := now.Sub(cache.loadedAt) < revocationTTL
	_, revoked := cache.revoked[tokenID]
	generation := cache.generation
	cache.mu.RUnlock()
	if fresh {
		return revoked, nil
	}
	ids, err := source.RevokedTokenIDs(ctx, now)
	if err != nil {
		return false, err
	}
	loaded := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		loaded[id] = struct{}{}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	_, revoked = loaded[tokenID]
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

// invalidateRevocations는 이 인스턴스가 방금 폐기한 토큰이 곧바로 막히게 한다.
// 다른 인스턴스의 폐기는 다음 적재에서 보인다.
func (cache *verificationCache) invalidateRevocations(tokenID string) {
	cache.mu.Lock()
	cache.revoked[tokenID] = struct{}{}
	cache.generation++
	cache.mu.Unlock()
}
