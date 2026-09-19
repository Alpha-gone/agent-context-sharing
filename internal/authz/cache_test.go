package authz

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/store"
)

// countingStore는 검증이 데이터베이스를 몇 번 읽는지 센다.
type countingStore struct {
	*memoryStore
	signingKeyReads atomic.Int64
	revocationReads atomic.Int64
}

func (s *countingStore) SigningKey(ctx context.Context, keyID string) (store.SigningKey, error) {
	s.signingKeyReads.Add(1)
	return s.memoryStore.SigningKey(ctx, keyID)
}

func (s *countingStore) RevokedTokenIDs(ctx context.Context, now time.Time) ([]string, error) {
	s.revocationReads.Add(1)
	return s.memoryStore.RevokedTokenIDs(ctx, now)
}

// TestVerificationReadsKeysAndRevocationsFromCache는 토큰 검증이 요청마다 서명 키와 폐기
// 목록을 읽지 않는지 확인한다. 「토큰 검증」이 둘 다 요청마다 가져오지 않기로 확정했다.
func TestVerificationReadsKeysAndRevocationsFromCache(t *testing.T) {
	backend := &countingStore{memoryStore: newMemoryStore()}
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "cached", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	token, err := service.WebSession(t.Context(), accountID, "web")
	if err != nil {
		t.Fatalf("세션 발급: %v", err)
	}

	const requests = 20
	for range requests {
		if _, _, err := service.Verify(t.Context(), token.Raw, "web"); err != nil {
			t.Fatalf("토큰 검증: %v", err)
		}
	}
	// 서명 키는 kid마다 한 번만 읽는다.
	if reads := backend.signingKeyReads.Load(); reads != 1 {
		t.Fatalf("서명 키 조회 = %d회, want 1회", reads)
	}
	// 폐기 목록도 요청마다 읽지 않는다.
	if reads := backend.revocationReads.Load(); reads >= requests {
		t.Fatalf("폐기 목록 조회 = %d회; %d회 요청보다 적어야 한다", reads, requests)
	}
}

// TestRevokedTokenIsRejectedImmediately는 이 인스턴스가 폐기한 토큰이 캐시 간격을
// 기다리지 않고 곧바로 막히는지 확인한다.
func TestRevokedTokenIsRejectedImmediately(t *testing.T) {
	backend := &countingStore{memoryStore: newMemoryStore()}
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "revoked", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	token, err := service.WebSession(t.Context(), accountID, "web")
	if err != nil {
		t.Fatalf("세션 발급: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); err != nil {
		t.Fatalf("폐기 전 검증: %v", err)
	}
	if err := service.Revoke(t.Context(), token); err != nil {
		t.Fatalf("토큰 폐기: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); err == nil {
		t.Fatal("폐기한 토큰이 곧바로 막히지 않았다")
	}
}

// TestRotatedKeyIsLoadedOnFirstUse는 회전한 키로 서명된 토큰이 처음 도착할 때 키를 다시
// 읽는지 확인한다. 「토큰 검증」이 키 회전을 kid 기반 재조회로 처리하기로 했다.
func TestRotatedKeyIsLoadedOnFirstUse(t *testing.T) {
	backend := &countingStore{memoryStore: newMemoryStore()}
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "rotated", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	first, err := service.WebSession(t.Context(), accountID, "web")
	if err != nil {
		t.Fatalf("첫 세션 발급: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), first.Raw, "web"); err != nil {
		t.Fatalf("첫 토큰 검증: %v", err)
	}
	if err := service.RotateSigningKey(t.Context()); err != nil {
		t.Fatalf("서명 키 회전: %v", err)
	}
	second, err := service.WebSession(t.Context(), accountID, "web")
	if err != nil {
		t.Fatalf("회전 뒤 세션 발급: %v", err)
	}
	// 새 kid를 만나면 다시 읽어 검증에 성공해야 한다.
	if _, _, err := service.Verify(t.Context(), second.Raw, "web"); err != nil {
		t.Fatalf("회전한 키로 서명된 토큰 검증: %v", err)
	}
	// 은퇴한 키로 서명된 토큰도 계속 검증된다.
	if _, _, err := service.Verify(t.Context(), first.Raw, "web"); err != nil {
		t.Fatalf("은퇴한 키로 서명된 토큰 검증: %v", err)
	}
	if reads := backend.signingKeyReads.Load(); reads != 2 {
		t.Fatalf("서명 키 조회 = %d회, want 2회(kid 두 개)", reads)
	}
}
