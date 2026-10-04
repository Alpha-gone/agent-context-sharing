package authz

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestExchangePreservesAuthenticationTime(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	// 한도 직전 세션으로 재인가해도 최초 로그인 시각을 초기화하지 않는다.
	authenticatedAt := time.Now().UTC().Add(-11*time.Hour - 59*time.Minute).Truncate(time.Second)
	verifier := testVerifier("authentication-time")
	for range 2 {
		code, err := service.Authorize(t.Context(), accountID, authenticatedAt, AuthorizeRequest{
			ResponseType: "code", ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback",
			CodeChallenge: digest(verifier), CodeChallengeMethod: "S256", Resource: service.config.Resource,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := backend.codes[digest(code)].AuthenticatedAt; !got.Equal(authenticatedAt) {
			t.Fatalf("코드의 인증 시각 = %s, want %s", got, authenticatedAt)
		}
		token, err := exchangeWithDPoP(t, service, code, "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := service.verifiedClaims(t.Context(), token.Raw, service.config.Resource)
		if err != nil {
			t.Fatal(err)
		}
		if claims.AuthenticatedAt != authenticatedAt.Unix() {
			t.Fatalf("교환한 토큰 auth_time = %d, want %d", claims.AuthenticatedAt, authenticatedAt.Unix())
		}
	}
}

func TestValidAuthenticationTimeBoundary(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name            string
		authenticatedAt time.Time
		valid           bool
	}{
		{"최초 인증 직후", now, true},
		{"12시간 직전", now.Add(-webSessionLifetime + time.Nanosecond), true},
		{"12시간 경계", now.Add(-webSessionLifetime), false},
		{"12시간 초과", now.Add(-webSessionLifetime - time.Nanosecond), false},
		{"누락", time.Time{}, false},
		{"Unix 0", time.Unix(0, 0), false},
		{"미래", now.Add(time.Nanosecond), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validAuthenticationTime(test.authenticatedAt, now); got != test.valid {
				t.Fatalf("인증 시각 유효 여부 = %t, want %t", got, test.valid)
			}
		})
	}
}

func TestAuthorizeRequiresRecentAuthentication(t *testing.T) {
	now := time.Now().UTC()
	for name, authenticatedAt := range map[string]time.Time{
		"누락": {}, "미래": now.Add(time.Hour), "12시간 경계": now.Add(-webSessionLifetime), "12시간 초과": now.Add(-13 * time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			accountID, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			request := AuthorizeRequest{ResponseType: "code", ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digest(testVerifier(name)), CodeChallengeMethod: "S256", Resource: service.config.Resource}
			if code, err := service.Authorize(t.Context(), accountID, authenticatedAt, request); !errors.Is(err, ErrInvalidCredential) || code != "" || len(backend.codes) != 0 {
				t.Fatalf("오래되거나 잘못된 인증으로 코드 발급 = %q, %v", code, err)
			}
			handler, err := NewHandler(service, func(*http.Request) (model.ID, time.Time, bool) { return accountID, authenticatedAt, true }, "/login")
			if err != nil {
				t.Fatal(err)
			}
			// 쿼리의 인증 시각은 신뢰하지 않는다.
			query := authorizeQuery(testVerifier(name))
			query.Set("auth_time", now.Format(time.RFC3339))
			target := "/authorize?" + query.Encode()
			recorder := httptest.NewRecorder()
			handler.Authorize(recorder, httptest.NewRequest(http.MethodGet, target, nil))
			location, err := url.Parse(recorder.Header().Get("Location"))
			if err != nil || recorder.Code != http.StatusSeeOther || location.Path != "/login" || location.Query().Get("next") != target || len(backend.codes) != 0 {
				t.Fatalf("재인증 리다이렉트 = %d, %v, %v", recorder.Code, location, err)
			}
		})
	}
}

func TestExchangeRejectsInvalidAuthenticationTime(t *testing.T) {
	now := time.Now().UTC()
	for name, authenticatedAt := range map[string]time.Time{
		"구형 코드": {}, "미래": now.Add(time.Hour), "코드 발급 뒤": now.Add(-time.Minute), "12시간 경계": now.Add(-webSessionLifetime), "12시간 초과": now.Add(-13 * time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			accountID, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			verifier := testVerifier(name)
			code, err := service.Authorize(t.Context(), accountID, now.Add(-11*time.Hour), AuthorizeRequest{ResponseType: "code", ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digest(verifier), CodeChallengeMethod: "S256", Resource: service.config.Resource})
			if err != nil {
				t.Fatal(err)
			}
			stored := backend.codes[digest(code)]
			stored.AuthenticatedAt = authenticatedAt
			stored.IssuedAt = now.Add(-2 * time.Minute)
			backend.codes[digest(code)] = stored
			if token, err := exchangeWithDPoP(t, service, code, "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource); err == nil || err.Error() != "invalid_grant" || token.Raw != "" {
				t.Fatalf("인증 한도를 넘은 코드가 교환됐다: %v", err)
			}
			if backend.codes[digest(code)].ConsumedAt != nil {
				t.Fatal("거부한 코드가 소비됐다")
			}
		})
	}
}

func TestExchangePreservesAuthenticationTimeIntegration(t *testing.T) {
	backend := newAuthorizationCodeIntegrationStore(t)
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "authtime"+newIntegrationLoginSuffix(t), "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	authenticatedAt := time.Now().UTC().Add(-11*time.Hour - 59*time.Minute).Truncate(time.Second)
	verifier := testVerifier("persisted-authentication-time")
	code, err := service.Authorize(t.Context(), accountID, authenticatedAt, AuthorizeRequest{ResponseType: "code", ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digest(verifier), CodeChallengeMethod: "S256", Resource: service.config.Resource})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := backend.AuthorizationCodeForExchange(t.Context(), digest(code), time.Now().UTC())
	if err != nil || !stored.AuthenticatedAt.Equal(authenticatedAt) {
		t.Fatalf("저장된 최초 인증 시각 = %s, %v", stored.AuthenticatedAt, err)
	}
	token, err := exchangeWithDPoP(t, service, code, "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.verifiedClaims(t.Context(), token.Raw, service.config.Resource)
	if err != nil || claims.AuthenticatedAt != authenticatedAt.Unix() {
		t.Fatalf("DB 왕복 뒤 auth_time = %d, %v; want %d", claims.AuthenticatedAt, err, authenticatedAt.Unix())
	}
}

func TestWebSessionAuthenticationTimePassesThroughAuthorize(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	authenticatedAt := now.Add(-11*time.Hour - 59*time.Minute).Truncate(time.Second)
	session, err := service.issue(t.Context(), accountID, "web", authenticatedAt, authenticatedAt, "")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(service, func(request *http.Request) (model.ID, time.Time, bool) {
		id, token, err := service.Verify(request.Context(), session.Raw, "web")
		return id, token.AuthenticatedAt, err == nil
	}, "/login")
	if err != nil {
		t.Fatal(err)
	}
	verifier := testVerifier("web-session-time")
	recorder := httptest.NewRecorder()
	handler.Authorize(recorder, httptest.NewRequest(http.MethodGet, "/authorize?"+authorizeQuery(verifier).Encode(), nil))
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil || recorder.Code != http.StatusFound || location.Query().Get("code") == "" {
		t.Fatalf("웹 세션 인가 = %d, %v, %v", recorder.Code, location, err)
	}
	token, err := exchangeWithDPoP(t, service, location.Query().Get("code"), "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.verifiedClaims(t.Context(), token.Raw, service.config.Resource)
	if err != nil || claims.AuthenticatedAt != authenticatedAt.Unix() {
		t.Fatalf("웹 세션에서 교환한 auth_time = %d, %v; want %d", claims.AuthenticatedAt, err, authenticatedAt.Unix())
	}
	// 갱신 구간의 토큰도 최초 인증 시각과 동일한 DPoP 키를 승계한다.
	nearExpiry, err := service.issue(t.Context(), accountID, service.config.Resource, now.Add(-time.Hour+5*time.Second), authenticatedAt, claims.Confirmation.JWKThumbprint)
	if err != nil {
		t.Fatal(err)
	}
	renewed, ok, err := service.Renew(t.Context(), accountID, nearExpiry)
	if err != nil || !ok {
		t.Fatalf("한도 전 갱신 = %t, %v", ok, err)
	}
	renewedClaims, err := service.verifiedClaims(t.Context(), renewed.Raw, service.config.Resource)
	if err != nil || renewedClaims.AuthenticatedAt != authenticatedAt.Unix() || renewedClaims.Confirmation != claims.Confirmation {
		t.Fatalf("갱신한 클레임 = %+v, %v", renewedClaims, err)
	}
	// 최초 인증 한도가 지난 기존 토큰은 exp까지 유효하지만 갱신할 수 없다.
	old, err := service.issue(t.Context(), accountID, service.config.Resource, now.Add(-time.Hour+5*time.Second), now.Add(-webSessionLifetime).Truncate(time.Second), claims.Confirmation.JWKThumbprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Verify(t.Context(), old.Raw, service.config.Resource); err != nil {
		t.Fatal(err)
	}
	if renewed, ok, err := service.Renew(t.Context(), accountID, old); err != nil || ok || renewed.Raw != "" {
		t.Fatalf("한도 뒤 갱신 = %t, %v", ok, err)
	}
}
