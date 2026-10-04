package store

import (
	"testing"
	"time"
)

func TestAuthorizationCodeRequiresAuthenticationTime(t *testing.T) {
	now := time.Now().UTC()
	code := AuthorizationCode{Hash: "hash", ClientID: "client", AccountID: newTestID(t), RedirectURI: "http://127.0.0.1/callback", CodeChallenge: "challenge", Resource: "https://service.test/mcp", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	for name, authenticatedAt := range map[string]time.Time{
		"누락": {}, "Unix 0": time.Unix(0, 0), "발급 뒤": now.Add(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			code.AuthenticatedAt = authenticatedAt
			if validAuthorizationCode(code) {
				t.Fatal("잘못된 최초 인증 시각이 허용됐다")
			}
		})
	}
	code.AuthenticatedAt = now.Add(-time.Hour)
	if !validAuthorizationCode(code) {
		t.Fatal("로그인 시각이 코드 발급보다 앞선 정상 코드가 거부됐다")
	}
}

func TestLegacyAuthorizationCodeAuthenticationTimeIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	now := time.Now().UTC()
	hash := "legacy-" + accountID.String()
	// 마이그레이션 전 형식으로 저장하여 nullable 인증 시각을 실제로 읽는다.
	if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.authorization_code
		(code_hash, client_id, account_id, redirect_uri, code_challenge, resource, issued_at, expires_at)
		VALUES ($1, 'client', $2, 'http://127.0.0.1/callback', 'challenge', 'https://service.test/mcp', $3, $4)`, hash, accountID.String(), now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	code, err := database.AuthorizationCodeForExchange(t.Context(), hash, now)
	if err != nil || !code.AuthenticatedAt.IsZero() {
		t.Fatalf("구형 코드 인증 시각 = %s, %v", code.AuthenticatedAt, err)
	}
}
