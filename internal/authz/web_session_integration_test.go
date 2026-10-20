package authz

import (
	"errors"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestWebSessionLifecycleIntegration(t *testing.T) {
	firstStore, secondStore := newAuthenticationContractStores(t)
	first, second := testService(t, firstStore), testService(t, secondStore)
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	first.now, second.now = clock, clock
	id, err := first.Register(t.Context(), "sessionuser", "correct password")
	if err != nil {
		t.Fatal("시험 계정 가입 실패")
	}
	loggedIn, err := second.Authenticate(t.Context(), "sessionuser", "correct password")
	if err != nil || loggedIn != id {
		t.Fatal("다른 인스턴스의 로그인 실패")
	}
	old, err := first.WebSession(t.Context(), id, "web")
	if err != nil {
		t.Fatal("최초 세션 발급 실패")
	}
	other, err := first.WebSession(t.Context(), id, "web")
	if err != nil || old.ID == other.ID {
		t.Fatal("독립 세션 발급 실패")
	}
	verify := func(t *testing.T, service *Service, token Token, expected model.ID) {
		t.Helper()
		got, verified, err := service.Verify(t.Context(), token.Raw, "web")
		if err != nil || got != expected || verified.ID != token.ID || !verified.ExpiresAt.Equal(token.ExpiresAt) {
			t.Fatal("공유 DB 세션 검증 실패")
		}
	}
	t.Run("shared verification and local logout", func(t *testing.T) {
		verify(t, second, old, id)
		if err := first.Revoke(t.Context(), old); err != nil {
			t.Fatal("로그아웃 폐기 실패")
		}
		if _, _, err := first.Verify(t.Context(), old.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("로컬 폐기 즉시 반영 실패")
		}
		verify(t, first, other, id)
		if err := first.Revoke(t.Context(), old); err != nil {
			t.Fatal("중복 로그아웃의 멱등 폐기 실패")
		}
	})
	t.Run("remote revocation interval and restart", func(t *testing.T) {
		now = now.Add(revocationTTL - time.Nanosecond)
		verify(t, second, old, id)
		now = now.Add(time.Nanosecond)
		if _, _, err := second.Verify(t.Context(), old.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("10초 경계의 원격 폐기 반영 실패")
		}
		restarted := testService(t, secondStore)
		restarted.now = clock
		if _, _, err := restarted.Verify(t.Context(), old.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("재시작 후 폐기가 유실됐습니다")
		}
		verify(t, restarted, other, id)
		ids, err := secondStore.RevokedTokenIDs(t.Context(), now)
		if err != nil || !slices.Contains(ids, old.ID) || slices.Contains(ids, other.ID) {
			t.Fatal("공유 폐기 목록의 세션별 격리 실패")
		}
		ids, err = secondStore.RevokedTokenIDs(t.Context(), old.ExpiresAt)
		if err != nil || slices.Contains(ids, old.ID) {
			t.Fatal("만료된 세션의 폐기 상태가 남았습니다")
		}
	})
	t.Run("rotation preserves valid sessions", func(t *testing.T) {
		if err := first.RotateSigningKey(t.Context()); err != nil {
			t.Fatal("키 회전 실패")
		}
		fresh, err := first.WebSession(t.Context(), id, "web")
		if err != nil {
			t.Fatal("회전 후 발급 실패")
		}
		restarted := testService(t, secondStore)
		restarted.now = clock
		verify(t, restarted, other, id)
		verify(t, restarted, fresh, id)
		if _, _, err := restarted.Verify(t.Context(), old.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("키 회전이 폐기를 되돌렸습니다")
		}
	})
	t.Run("twelve hour boundary without renewal", func(t *testing.T) {
		now = other.ExpiresAt.Add(-time.Nanosecond)
		verify(t, second, other, id)
		now = other.ExpiresAt
		if _, _, err := second.Verify(t.Context(), other.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("12시간 만료 경계 실패")
		}
	})
}
