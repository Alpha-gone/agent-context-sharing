package authz

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestBearerSharedDatabaseIntegration(t *testing.T) {
	firstStore, secondStore, pool := newAuthenticationContractDatabase(t)
	config := oauthTestConfig()
	config.AuthorizationCodes, config.AccessTokens = firstStore, firstStore
	first, err := New(firstStore, config)
	if err != nil {
		t.Fatal(err)
	}
	config.AuthorizationCodes, config.AccessTokens = secondStore, secondStore
	second, err := New(secondStore, config)
	if err != nil {
		t.Fatal(err)
	}
	account, err := first.Register(t.Context(), "bearer_account", "correct password")
	if err != nil {
		t.Fatal("시험 계정 준비 실패")
	}
	now := time.Now().UTC().Truncate(time.Second)
	first.now = func() time.Time { return now }
	second.now = first.now
	issue := func(t *testing.T) (OAuthTokens, jwt.Claims, BearerPrincipal) {
		t.Helper()
		code, err := first.IssueAuthorizationCode(t.Context(), account, validAuthorizationRequest())
		if err != nil {
			t.Fatal("시험 코드 준비 실패")
		}
		tokens, err := first.ExchangeAuthorizationCode(t.Context(), code, "first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", oauthVerifier)
		if err != nil {
			t.Fatal("시험 토큰 준비 실패")
		}
		principal, err := second.VerifyBearer(t.Context(), tokens.AccessToken)
		if err != nil || principal.AccountID != account || principal.ClientID != "first" {
			t.Fatal("다른 인스턴스의 최초 접근 거부")
		}
		parsed, err := jwt.ParseSigned(tokens.AccessToken, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatal(err)
		}
		var claims jwt.Claims
		// 서명은 앞의 검증기가 확인했고 이 읽기는 시험의 jti·수명 대조에만 쓴다.
		if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
			t.Fatal(err)
		}
		return tokens, claims, principal
	}
	for _, field := range []string{"account_id", "client_id", "resource", "scope", "authorized_at"} {
		t.Run("grant binding/"+field, func(t *testing.T) {
			tokens, _, principal := issue(t)
			var value any = "different"
			switch field {
			case "account_id":
				other, _ := model.NewID()
				value = other.String()
			case "authorized_at":
				value = now.Add(time.Second)
			}
			if _, err := pool.Exec(t.Context(), "UPDATE public.oauth_grant SET "+field+"=$2 WHERE grant_id=$1", principal.GrantID.String(), value); err != nil {
				t.Fatal("격리 계열 준비 실패")
			}
			if principal, err := second.VerifyBearer(t.Context(), tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) || principal != (BearerPrincipal{}) {
				t.Fatal("다른 계열 결합 허용")
			}
		})
	}
	t.Run("missing grant", func(t *testing.T) {
		_, claims, principal := issue(t)
		missing, _ := model.NewID()
		allowed, err := secondStore.OAuthAccessAllowed(t.Context(), store.OAuthAccessBinding{AccountID: account, GrantID: missing, ClientID: principal.ClientID, Resource: config.OAuth.Resource, Scope: "agent-context", TokenID: claims.ID, IssuedAt: now}, now)
		if err != nil || allowed {
			t.Fatal("없는 계열 허용")
		}
	})
	t.Run("individual revocation immediately across instances", func(t *testing.T) {
		tokens, claims, _ := issue(t)
		if err := first.Revoke(t.Context(), Token{ID: claims.ID, ExpiresAt: claims.Expiry.Time()}); err != nil {
			t.Fatal("폐기 실패")
		}
		for range 3 {
			if _, err := second.VerifyBearer(t.Context(), tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
				t.Fatal("웹 캐시 주기까지 개별 폐기 지연")
			}
		}
	})
	t.Run("family revocation and restart", func(t *testing.T) {
		tokens, _, principal := issue(t)
		if _, err := pool.Exec(t.Context(), `UPDATE public.oauth_grant SET revoked_at=$2,revocation_reason='synthetic_revoke' WHERE grant_id=$1`, principal.GrantID.String(), now); err != nil {
			t.Fatal("계열 폐기 실패")
		}
		restarted, err := New(secondStore, config)
		if err != nil {
			t.Fatal(err)
		}
		restarted.now = first.now
		var workers sync.WaitGroup
		for i := range 8 {
			workers.Go(func() {
				service := []*Service{first, second, restarted}[i%3]
				if _, err := service.VerifyBearer(t.Context(), tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
					t.Error("인스턴스·재시작 뒤 계열 폐기 누락")
				}
			})
		}
		workers.Wait()
	})
	t.Run("code reuse immediately revokes bearer", func(t *testing.T) {
		code, err := first.IssueAuthorizationCode(t.Context(), account, validAuthorizationRequest())
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := first.ExchangeAuthorizationCode(t.Context(), code, "first", "http://127.0.0.1:49152/callback", config.OAuth.Resource, oauthVerifier)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := second.VerifyBearer(t.Context(), tokens.AccessToken); err != nil {
			t.Fatal(err)
		}
		if _, err := first.ExchangeAuthorizationCode(t.Context(), code, "first", "http://127.0.0.1:49152/callback", config.OAuth.Resource, oauthVerifier); err == nil {
			t.Fatal("코드 재사용 허용")
		}
		if _, err := second.VerifyBearer(t.Context(), tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("코드 재사용 폐기 미반영")
		}
	})
	t.Run("refresh expiry preserves access and does not rotate", func(t *testing.T) {
		tokens, _, principal := issue(t)
		if _, err := pool.Exec(t.Context(), `UPDATE public.oauth_grant SET absolute_expires_at=$2,idle_expires_at=$2 WHERE grant_id=$1`, principal.GrantID.String(), now); err != nil {
			t.Fatal(err)
		}
		if _, err := second.VerifyBearer(t.Context(), tokens.AccessToken); err != nil {
			t.Fatal("갱신 만료가 접근 수명 축소")
		}
		var rotated time.Time
		if err := pool.QueryRow(t.Context(), `SELECT last_rotated_at FROM public.oauth_grant WHERE grant_id=$1`, principal.GrantID.String()).Scan(&rotated); err != nil || !rotated.Equal(now) {
			t.Fatal("도구 인증이 갱신 활동 변경")
		}
		second.now = func() time.Time { return now.Add(time.Hour) }
		defer func() { second.now = first.now }()
		if _, err := second.VerifyBearer(t.Context(), tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("접근 만료 정각 허용")
		}
	})
	t.Run("key rotation", func(t *testing.T) {
		tokens, _, _ := issue(t)
		if err := first.RotateSigningKey(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := second.VerifyBearer(t.Context(), tokens.AccessToken); err != nil {
			t.Fatal("회전 전 접근 거부")
		}
		second.cache.invalidateKeys()
		issue(t)
	})
	t.Run("protected TLS and closed database mapping", func(t *testing.T) {
		tokens, _, _ := issue(t)
		handler, _ := NewHandler(second)
		server := httptest.NewTLSServer(handler.ProtectBearer(func(writer http.ResponseWriter, request *http.Request, principal BearerPrincipal) {
			if principal.AccountID != account {
				t.Error("TLS 주체 결합 실패")
			}
			writer.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()
		invoke := func(raw string, want int) {
			t.Helper()
			request, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", nil)
			if err != nil {
				t.Fatal(err)
			}
			if raw != "" {
				request.Header.Set("Authorization", "Bearer "+raw)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal("TLS 요청 실패")
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != want || strings.Contains(string(body), tokens.AccessToken) {
				t.Fatal("TLS 상태·비노출 실패")
			}
			if want == 500 && response.Header.Get("WWW-Authenticate") != "" {
				t.Fatal("저장 장애가 재인증 요구")
			}
		}
		invoke(tokens.AccessToken, 204)
		invoke("", 401)
		secondStore.Close()
		invoke(tokens.AccessToken, 500) // 알려진 키 캐시가 있어도 공유 폐기 조회 장애는 500이다.
	})
}
