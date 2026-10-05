package store

import (
	"errors"
	"testing"
	"time"
)

// TestExpiredCodeReuseReportsUseIntegration은 소비한 인가 코드를 만료 뒤에 다시 제시해도
// 재사용으로 판정하는지 확인한다.
//
// 코드 수명은 60초이고 접근 토큰은 1시간이다. 만료를 소비 여부보다 먼저 보면 탈취한
// 코드를 60초 뒤에 제시할 때 없는 코드로 판정되어, 그 코드로 발급한 토큰이 폐기되지
// 않는다. 「인가 코드 흐름」이 소비 행을 남겨 두는 이유가 바로 이 판정이다.
func TestExpiredCodeReuseReportsUseIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)

	now := time.Now().UTC()
	hash := "reuse-" + accountID.String()
	tokenID := "token-" + accountID.String()
	code := AuthorizationCode{
		Hash: hash, ClientID: "test-client", AccountID: accountID,
		RedirectURI: "http://127.0.0.1/callback", CodeChallenge: "challenge",
		Resource: "https://service.test/mcp", IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
		AuthenticatedAt: now.Add(-2 * time.Hour),
	}
	if err := database.CreateAuthorizationCode(t.Context(), code); err != nil {
		t.Fatalf("인가 코드 저장: %v", err)
	}
	consumedAt := now.Add(-2 * time.Minute)
	tokenExpiresAt := now.Add(time.Hour)
	if _, err := database.pool.Exec(t.Context(), `
		UPDATE public.authorization_code
		SET consumed_at = $2, issued_token_id = $3, issued_token_expires_at = $4, authenticated_at = NULL
		WHERE code_hash = $1`, hash, consumedAt, tokenID, tokenExpiresAt); err != nil {
		t.Fatalf("소비 표시: %v", err)
	}

	if _, err := database.CleanupExpiredAuthorizationCodes(t.Context(), now); err != nil {
		t.Fatalf("인가 코드 정리: %v", err)
	}
	for name, call := range map[string]func() error{
		"교환 조회": func() error {
			_, err := database.AuthorizationCodeForExchange(t.Context(), hash, now)
			return err
		},
		"조건부 소비": func() error {
			_, err := database.ConsumeAuthorizationCode(t.Context(), hash, "new-token", now, tokenExpiresAt)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			used, ok := errors.AsType[CodeUsedError](err)
			if !ok {
				t.Fatalf("만료된 재사용 코드 = %v, want CodeUsedError", err)
			}
			if used.TokenID != tokenID {
				t.Fatalf("폐기 대상 토큰 = %q, want %q", used.TokenID, tokenID)
			}
		})
	}
}
