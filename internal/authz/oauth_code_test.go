package authz

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

const oauthVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

func validAuthorizationRequest() AuthorizationRequest {
	return AuthorizationRequest{ClientID: "first", RedirectURI: "http://127.0.0.1:49152/callback", Resource: "https://resource.test/mcp", Scope: "agent-context", ResponseType: "code", ChallengeMethod: "S256", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}
}

func TestOAuthCodeRequestValidation(t *testing.T) {
	service, err := New(newMemoryStore(), oauthTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*AuthorizationRequest)
		code   string
	}{
		{"valid", func(*AuthorizationRequest) {}, ""},
		{"plain", func(r *AuthorizationRequest) { r.ChallengeMethod = "plain" }, "invalid_request"},
		{"missing method", func(r *AuthorizationRequest) { r.ChallengeMethod = "" }, "invalid_request"},
		{"short challenge", func(r *AuthorizationRequest) { r.Challenge = "short" }, "invalid_request"},
		{"padded challenge", func(r *AuthorizationRequest) { r.Challenge += "=" }, "invalid_request"},
		{"noncanonical challenge", func(r *AuthorizationRequest) { r.Challenge = strings.Repeat("A", 42) + "B" }, "invalid_request"},
		{"plain verifier as challenge", func(r *AuthorizationRequest) { r.Challenge = strings.Repeat("~", 43) }, "invalid_request"},
		{"implicit", func(r *AuthorizationRequest) { r.ResponseType = "token" }, "unsupported_response_type"},
		{"missing response", func(r *AuthorizationRequest) { r.ResponseType = "" }, "unsupported_response_type"},
		{"missing scope", func(r *AuthorizationRequest) { r.Scope = "" }, "invalid_scope"},
		{"scope extension", func(r *AuthorizationRequest) { r.Scope += " admin" }, "invalid_scope"},
		{"scope case", func(r *AuthorizationRequest) { r.Scope = "Agent-context" }, "invalid_scope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := validAuthorizationRequest()
			test.change(&input)
			err := service.ValidateAuthorizationRequest(input)
			if test.code == "" {
				if err != nil {
					t.Fatal("정상 인가 거부")
				}
				return
			}
			failure, ok := errors.AsType[*OAuthError](err)
			if !ok || failure.Code != test.code {
				t.Fatal("인가 표준 오류 불일치")
			}
		})
	}
	request := validAuthorizationRequest()
	request.ClientID = "unknown"
	if !errors.Is(service.ValidateAuthorizationRequest(request), ErrInvalidAuthorizationTarget) {
		t.Fatal("미등록 인가 허용")
	}
}

type oauthCodeFaultStore struct {
	create   func(context.Context, store.OAuthAuthorizationCode) error
	exchange func(context.Context, store.OAuthCodeExchange, func() time.Time, func(store.OAuthAuthorizationCode, time.Time) (store.OAuthInitialGrant, error)) error
}

func (s oauthCodeFaultStore) CreateOAuthAuthorizationCode(ctx context.Context, code store.OAuthAuthorizationCode) error {
	return s.create(ctx, code)
}
func (s oauthCodeFaultStore) ExchangeOAuthAuthorizationCode(ctx context.Context, input store.OAuthCodeExchange, clock func() time.Time, issue func(store.OAuthAuthorizationCode, time.Time) (store.OAuthInitialGrant, error)) error {
	return s.exchange(ctx, input, clock, issue)
}

func TestOAuthCodeIssuanceAndFailures(t *testing.T) {
	account, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	var saved store.OAuthAuthorizationCode
	config := oauthTestConfig()
	config.AuthorizationCodes = oauthCodeFaultStore{create: func(_ context.Context, code store.OAuthAuthorizationCode) error { saved = code; return nil }}
	service, err := New(newMemoryStore(), config)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	raw, err := service.IssueAuthorizationCode(t.Context(), account, validAuthorizationRequest())
	decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || decodeErr != nil || len(decoded) != 32 || saved.Hash != sha256.Sum256([]byte(raw)) || saved.AccountID != account || saved.Scope != "agent-context" || !saved.IssuedAt.Equal(now) || !saved.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatal("코드 난수·해시·수명·계정 결합 불일치")
	}
	for _, test := range []struct {
		name    string
		failure error
		cancel  bool
	}{
		{"storage failure", errors.New("synthetic sensitive store detail"), false},
		{"cancel after save", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service.config.AuthorizationCodes = oauthCodeFaultStore{create: func(context.Context, store.OAuthAuthorizationCode) error {
				if test.cancel {
					cancel()
				}
				return test.failure
			}}
			raw, err := service.IssueAuthorizationCode(ctx, account, validAuthorizationRequest())
			if raw != "" || err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("실패·취소 뒤 자격 또는 내부 원문 반환")
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatal("취소 원인 누락")
			}
			if !test.cancel && (!errors.Is(err, ErrUnavailable) || !errors.Is(err, test.failure)) {
				t.Fatal("저장 장애 분류·원인 누락")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.IssueAuthorizationCode(ctx, account, validAuthorizationRequest()); !errors.Is(err, context.Canceled) {
		t.Fatal("취소 요청 처리")
	}
	if _, err := service.IssueAuthorizationCode(t.Context(), model.ID{}, validAuthorizationRequest()); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("빈 계정으로 발급")
	}
	service.config.AuthorizationCodes = nil
	if _, err := service.IssueAuthorizationCode(t.Context(), account, validAuthorizationRequest()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("저장 경계 부재 허용")
	}
}

func TestOAuthTokenHTTPInputBoundary(t *testing.T) {
	base := url.Values{"grant_type": {"authorization_code"}, "code": {"synthetic-code"}, "client_id": {"first"}, "redirect_uri": {"http://127.0.0.1:49152/callback"}, "resource": {"https://resource.test/mcp"}, "code_verifier": {oauthVerifier}}
	for _, test := range []struct {
		name, method, query, contentType, body, authorization string
		length                                                int64
		status                                                int
		code                                                  string
	}{
		{"GET", "GET", "", "", "", "", 0, 405, "invalid_request"},
		{"json", "POST", "", "application/json", "{}", "", 2, 400, "invalid_request"},
		{"no type", "POST", "", "", base.Encode(), "", -1, 400, "invalid_request"},
		{"duplicate", "POST", "", "application/x-www-form-urlencoded", base.Encode() + "&code=other", "", -1, 400, "invalid_request"},
		{"malformed", "POST", "", "application/x-www-form-urlencoded", "code=%zz", "", -1, 400, "invalid_request"},
		{"query credential", "POST", "?code=other", "application/x-www-form-urlencoded", base.Encode(), "", -1, 400, "invalid_request"},
		{"header authentication", "POST", "", "application/x-www-form-urlencoded", base.Encode(), "Bearer synthetic", -1, 400, "invalid_client"},
		{"client secret", "POST", "", "application/x-www-form-urlencoded", base.Encode() + "&client_secret=synthetic", "", -1, 400, "invalid_client"},
		{"missing grant", "POST", "", "application/x-www-form-urlencoded", "code=synthetic", "", -1, 400, "invalid_request"},
		{"unsupported grant", "POST", "", "application/x-www-form-urlencoded", "grant_type=client_credentials", "", -1, 400, "unsupported_grant_type"},
		{"missing code", "POST", "", "application/x-www-form-urlencoded", "grant_type=authorization_code", "", -1, 400, "invalid_request"},
		{"announced over limit", "POST", "", "application/x-www-form-urlencoded", "x", "", tokenFormLimit + 1, 413, "invalid_request"},
		{"chunked over limit", "POST", "", "application/x-www-form-urlencoded", "x=" + strings.Repeat("x", tokenFormLimit), "", -1, 413, "invalid_request"},
		{"wrong verifier format", "POST", "", "application/x-www-form-urlencoded", strings.Replace(base.Encode(), oauthVerifier, "short", 1), "", -1, 400, "invalid_grant"},
		{"exact limit", "POST", "", "application/x-www-form-urlencoded", base.Encode() + "&padding=" + strings.Repeat("x", tokenFormLimit-len(base.Encode())-len("&padding=")), "", -1, 400, "invalid_grant"},
		{"below limit", "POST", "", "application/x-www-form-urlencoded", base.Encode() + "&padding=" + strings.Repeat("x", tokenFormLimit-len(base.Encode())-len("&padding=")-1), "", -1, 400, "invalid_grant"},
		{"invalid grant", "POST", "", "application/x-www-form-urlencoded", base.Encode(), "", -1, 400, "invalid_grant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := oauthTestConfig()
			config.AuthorizationCodes = oauthCodeFaultStore{exchange: func(context.Context, store.OAuthCodeExchange, func() time.Time, func(store.OAuthAuthorizationCode, time.Time) (store.OAuthInitialGrant, error)) error {
				if test.name != "invalid grant" && test.name != "exact limit" && test.name != "below limit" {
					t.Fatal("형식 거부 전에 DB 소비 호출")
				}
				return store.ErrInvalidOAuthGrant
			}}
			service, err := New(newMemoryStore(), config)
			if err != nil {
				t.Fatal(err)
			}
			handler, _ := NewHandler(service)
			request := httptest.NewRequest(test.method, "https://issuer.test/token"+test.query, strings.NewReader(test.body))
			request.ContentLength = test.length
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			response := httptest.NewRecorder()
			handler.Token(response, request)
			var document map[string]string
			if response.Code != test.status || json.Unmarshal(response.Body.Bytes(), &document) != nil || document["error"] != test.code || len(document) != 1 {
				t.Fatalf("token 오류 경계 불일치: status=%d", response.Code)
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Location") != "" || len(response.Result().Cookies()) != 0 {
				t.Fatal("오류 캐시·세션·리디렉션 경계 불일치")
			}
		})
	}
}
