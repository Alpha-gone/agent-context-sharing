package store

import (
	"testing"
	"time"
)

func TestAuthorizationCodeCleanupRespectsIssuedTokenExpiryIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, test := range []struct {
		name        string
		issuedAt    time.Time
		consumed    bool
		tokenExpiry *time.Time
		want        bool
	}{
		{"미소비 만료", now.Add(-2 * time.Minute), false, nil, false},
		{"미소비 유효", now, false, nil, true},
		{"소비/토큰 유효", now.Add(-2 * time.Minute), true, new(now.Add(time.Hour)), true},
		{"소비/토큰 만료", now.Add(-2 * time.Minute), true, new(now.Add(-time.Second)), false},
		{"소비/토큰 만료 경계", now.Add(-2 * time.Minute), true, new(now), false},
		{"기존 소비/보존 한도 안", now.Add(-60 * time.Minute), true, nil, true},
		{"기존 소비/보존 한도 밖", now.Add(-62 * time.Minute), true, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			hash := test.name + "-" + accountID.String()
			code := AuthorizationCode{Hash: hash, ClientID: "client", AccountID: accountID, RedirectURI: "http://127.0.0.1/callback", CodeChallenge: "challenge", Resource: "https://service.test/mcp", IssuedAt: test.issuedAt, AuthenticatedAt: test.issuedAt, ExpiresAt: test.issuedAt.Add(time.Minute)}
			if err := database.CreateAuthorizationCode(t.Context(), code); err != nil {
				t.Fatal(err)
			}
			if test.consumed {
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.authorization_code SET consumed_at = $2, issued_token_id = $3, issued_token_expires_at = $4 WHERE code_hash = $1`, hash, test.issuedAt, "token-"+hash, test.tokenExpiry); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.CleanupExpiredAuthorizationCodes(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			var exists bool
			if err := database.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM public.authorization_code WHERE code_hash = $1)`, hash).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != test.want {
				t.Fatalf("정리 뒤 코드 보존 = %t, 기대 %t", exists, test.want)
			}
		})
	}
}
