package store

import (
	"testing"
	"time"
)

// TestAuthStoreRejectsInvalidInputs는 저장소가 잘못된 인증·인가 입력을 SQL 실행 전에 거부하는지 확인한다.
func TestAuthStoreRejectsInvalidInputs(t *testing.T) {
	store := Store{}
	now := time.Now().UTC()

	tests := []struct {
		name string
		err  error
	}{
		{name: "빈 로그인 아이디", err: func() error { _, err := store.AccountByLoginID(t.Context(), ""); return err }()},
		{name: "불완전한 인가 코드", err: store.CreateAuthorizationCode(t.Context(), AuthorizationCode{})},
		{name: "빈 인가 코드 해시", err: func() error { _, err := store.ConsumeAuthorizationCode(t.Context(), "", now); return err }()},
		{name: "빈 발급 토큰", err: store.SetAuthorizationCodeToken(t.Context(), "code", "", now)},
		{name: "빈 폐기 토큰", err: store.RevokeToken(t.Context(), "", now)},
		{name: "빈 폐기 조회 토큰", err: func() error { _, err := store.IsTokenRevoked(t.Context(), "", now); return err }()},
		{name: "불완전한 서명 키 생성", err: store.CreateSigningKey(t.Context(), SigningKey{})},
		{name: "불완전한 서명 키 회전", err: store.RotateSigningKey(t.Context(), SigningKey{})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.err == nil {
				t.Fatal("잘못된 입력이 허용됐다")
			}
		})
	}
}
