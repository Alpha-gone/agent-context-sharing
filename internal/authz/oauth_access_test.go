package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type accessStoreFunc func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error)

func (f accessStoreFunc) OAuthAccessAllowed(ctx context.Context, binding store.OAuthAccessBinding, now time.Time) (bool, error) {
	return f(ctx, binding, now)
}

func bearerFixture(t *testing.T) (*Service, map[string]any, func(map[string]any) string) {
	t.Helper()
	config := oauthTestConfig()
	config.AccessTokens = accessStoreFunc(func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error) { return true, nil })
	service, err := New(newMemoryStore(), config)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return now }
	account, _ := model.NewID()
	grant, _ := model.NewID()
	claims := map[string]any{"iss": config.Issuer, "sub": account.String(), "aud": config.OAuth.Resource, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "jti": "synthetic-access", "scope": "agent-context", "client_id": "first", "grant_id": grant.String()}
	key, err := service.activeKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	private, err := privateSigningKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", key.ID))
	if err != nil {
		t.Fatal(err)
	}
	return service, claims, func(claims map[string]any) string {
		t.Helper()
		raw, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			t.Fatal("합성 접근 토큰 준비 실패")
		}
		return raw
	}
}

func TestBearerRequiredClaims(t *testing.T) {
	for _, field := range []string{"iss", "sub", "aud", "exp", "iat", "jti", "scope", "client_id", "grant_id"} {
		for _, mode := range []string{"missing", "null", "wrong type"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				service, claims, sign := bearerFixture(t)
				switch mode {
				case "missing":
					delete(claims, field)
				case "null":
					claims[field] = nil
				case "wrong type":
					claims[field] = map[string]string{"value": "synthetic"}
				}
				if principal, err := service.VerifyBearer(t.Context(), sign(claims)); !errors.Is(err, ErrInvalidCredential) || principal != (BearerPrincipal{}) {
					t.Fatal("필수 클레임 오류가 인증 주체로 전달됨")
				}
			})
		}
	}
}

func TestBearerClaimBoundaries(t *testing.T) {
	for _, mode := range []string{"valid", "last instant", "expired", "future iat", "future nbf", "empty lifetime", "over hour", "issuer", "resource", "multi audience", "web", "subject uuid", "grant uuid", "client", "client resource", "empty jti", "empty scope", "insufficient scope"} {
		t.Run(mode, func(t *testing.T) {
			service, claims, sign := bearerFixture(t)
			now := service.now()
			want := ErrInvalidCredential
			switch mode {
			case "valid":
				want = nil
			case "last instant":
				service.now = func() time.Time { return now.Add(time.Hour - time.Nanosecond) }
				want = nil
			case "expired":
				service.now = func() time.Time { return now.Add(time.Hour) }
			case "future iat":
				claims["iat"] = now.Add(time.Second).Unix()
			case "future nbf":
				claims["nbf"] = now.Add(time.Second).Unix()
			case "empty lifetime":
				claims["exp"] = claims["iat"]
			case "over hour":
				claims["exp"] = now.Add(time.Hour + time.Second).Unix()
			case "issuer":
				claims["iss"] = "https://other.test"
			case "resource":
				claims["aud"] = "https://other.test/mcp"
			case "multi audience":
				claims["aud"] = []string{service.oauth.Resource, "web"}
			case "web":
				claims["aud"] = "web"
			case "subject uuid":
				claims["sub"] = "00000000-0000-4000-8000-000000000000"
			case "grant uuid":
				claims["grant_id"] = "00000000-0000-4000-8000-000000000000"
			case "client":
				claims["client_id"] = "unknown"
			case "client resource":
				client := service.oauth.Clients["first"]
				client.Resources = []string{"https://other.test/mcp"}
				service.oauth.Clients["first"] = client
			case "empty jti":
				claims["jti"] = ""
			case "empty scope":
				claims["scope"] = ""
			case "insufficient scope":
				claims["scope"] = "other"
				want = ErrInsufficientScope
			}
			principal, err := service.VerifyBearer(t.Context(), sign(claims))
			if !errors.Is(err, want) {
				t.Fatalf("결과 분류 불일치: %v", err)
			}
			if want != nil && principal != (BearerPrincipal{}) {
				t.Fatal("거부 자격의 주체 전달")
			}
			if want == nil && (principal.AccountID.String() != claims["sub"] || principal.ClientID != "first" || principal.GrantID.String() != claims["grant_id"]) {
				t.Fatal("인증 결합 불일치")
			}
		})
	}
}

func TestBearerSharedStateEveryRequest(t *testing.T) {
	service, claims, sign := bearerFixture(t)
	raw := sign(claims)
	calls := 0
	allowed := true
	var failure error
	service.config.AccessTokens = accessStoreFunc(func(ctx context.Context, binding store.OAuthAccessBinding, now time.Time) (bool, error) {
		calls++
		if binding.AccountID.String() != claims["sub"] || binding.GrantID.String() != claims["grant_id"] || binding.ClientID != "first" || binding.Resource != service.oauth.Resource || binding.Scope != "agent-context" || binding.TokenID != claims["jti"] || !binding.IssuedAt.Equal(service.now()) || !now.Equal(service.now()) {
			t.Fatal("공유 상태 대조 결합 누락")
		}
		return allowed, failure
	})
	for range 2 {
		if _, err := service.VerifyBearer(t.Context(), raw); err != nil {
			t.Fatal(err)
		}
	}
	allowed = false
	if _, err := service.VerifyBearer(t.Context(), raw); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("즉시 폐기 반영 실패")
	}
	failure = errors.New("synthetic-storage-secret")
	if _, err := service.VerifyBearer(t.Context(), raw); !errors.Is(err, ErrUnavailable) || !errors.Is(err, failure) || strings.Contains(err.Error(), "synthetic-storage-secret") {
		t.Fatal("장애 분류·원인 보존·비노출 실패")
	}
	if calls != 4 {
		t.Fatal("Bearer 폐기가 캐시됨")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.VerifyBearer(ctx, raw); !errors.Is(err, context.Canceled) || calls != 4 {
		t.Fatal("취소 요청이 DB에 진입")
	}
	ctx, cancel = context.WithCancel(t.Context())
	service.config.AccessTokens = accessStoreFunc(func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error) { cancel(); return true, nil })
	if principal, err := service.VerifyBearer(ctx, raw); !errors.Is(err, context.Canceled) || principal != (BearerPrincipal{}) {
		t.Fatal("조회 뒤 취소 성공")
	}
	now := service.now()
	service.config.AccessTokens = accessStoreFunc(func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error) {
		service.now = func() time.Time { return now.Add(time.Hour) }
		return true, nil
	})
	if _, err := service.VerifyBearer(t.Context(), raw); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("공유 조회 중 만료 허용")
	}
}

func TestBearerSignatureAndKeyFailures(t *testing.T) {
	for _, mode := range []string{"unsigned", "symmetric", "wrong signature", "unknown kid", "no kid", "key outage", "damaged key", "missing store", "missing oauth", "web session"} {
		t.Run(mode, func(t *testing.T) {
			service, claims, sign := bearerFixture(t)
			raw := sign(claims)
			want := ErrInvalidCredential
			backend := service.store.(*memoryStore)
			switch mode {
			case "unsigned":
				raw = "eyJhbGciOiJub25lIn0.e30."
			case "symmetric":
				signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(strings.Repeat("x", 32))}, (&jose.SignerOptions{}).WithHeader("kid", backend.keys[0].ID))
				if err != nil {
					t.Fatal(err)
				}
				raw, err = jwt.Signed(signer).Claims(claims).Serialize()
				if err != nil {
					t.Fatal(err)
				}
			case "wrong signature", "unknown kid", "no kid":
				key, err := newSigningKey()
				if err != nil {
					t.Fatal(err)
				}
				private, err := privateSigningKey(key)
				if err != nil {
					t.Fatal(err)
				}
				options := (&jose.SignerOptions{}).WithType("JWT")
				if mode == "wrong signature" {
					options.WithHeader("kid", backend.keys[0].ID)
				} else if mode == "unknown kid" {
					options.WithHeader("kid", "unknown")
				}
				signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, options)
				if err != nil {
					t.Fatal(err)
				}
				raw, err = jwt.Signed(signer).Claims(claims).Serialize()
				if err != nil {
					t.Fatal(err)
				}
			case "key outage":
				service.store = &failingStore{memoryStore: backend, signingKeyFails: true}
				want = ErrUnavailable
			case "damaged key":
				backend.keys[0].PublicKey = "synthetic-key-secret"
				want = ErrUnavailable
			case "missing store":
				service.config.AccessTokens = nil
				want = ErrUnavailable
			case "missing oauth":
				service.oauth = nil
				want = ErrUnavailable
			case "web session":
				account, _ := model.ParseID(claims["sub"].(string))
				token, err := service.WebSession(t.Context(), account, "web")
				if err != nil {
					t.Fatal(err)
				}
				raw = token.Raw
			}
			if principal, err := service.VerifyBearer(t.Context(), raw); !errors.Is(err, want) || principal != (BearerPrincipal{}) {
				t.Fatal("서명·키·채널 거부 분류 실패")
			}
		})
	}
}

func TestBearerHTTPMapping(t *testing.T) {
	for _, mode := range []string{"valid", "scheme case and spaces", "absent", "cookie", "query", "form", "basic", "empty", "tab", "trailing space", "list", "duplicate", "expired", "scope", "binding", "outage", "key outage", "cancelled", "cancel after lookup"} {
		t.Run(mode, func(t *testing.T) {
			service, claims, sign := bearerFixture(t)
			status, code := http.StatusUnauthorized, "invalid_token"
			switch mode {
			case "valid", "scheme case and spaces":
				status, code = http.StatusNoContent, ""
			case "absent", "cookie", "query", "form":
				code = ""
			case "expired":
				claims["exp"] = claims["iat"]
			case "scope":
				claims["scope"] = "other"
				status, code = http.StatusForbidden, "insufficient_scope"
			case "binding":
				service.config.AccessTokens = accessStoreFunc(func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error) { return false, nil })
			case "outage":
				service.config.AccessTokens = accessStoreFunc(func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error) {
					return false, errors.New("synthetic-storage-secret")
				})
				status, code = 500, "server_error"
			case "key outage":
				service.store = &failingStore{memoryStore: service.store.(*memoryStore), signingKeyFails: true}
				status, code = 500, "server_error"
			case "cancelled", "cancel after lookup":
				status, code = 200, ""
			}
			raw := sign(claims)
			request := httptest.NewRequest("POST", "https://attacker.test/mcp", nil)
			request.Header.Set("Authorization", "Bearer "+raw)
			ctx, cancel := context.WithCancel(request.Context())
			defer cancel()
			request = request.WithContext(ctx)
			switch mode {
			case "scheme case and spaces":
				request.Header.Set("Authorization", "bEaReR   "+raw)
			case "absent", "cookie", "query", "form":
				request.Header.Del("Authorization")
				if mode == "cookie" {
					request.AddCookie(&http.Cookie{Name: "session", Value: raw})
				}
				if mode == "query" {
					request.URL.RawQuery = "access_token=" + raw
				}
				if mode == "form" {
					request.Body = http.NoBody
					request.PostForm = map[string][]string{"access_token": {raw}}
				}
			case "basic":
				request.Header.Set("Authorization", "Basic "+raw)
			case "empty":
				request.Header.Set("Authorization", "Bearer ")
			case "tab":
				request.Header.Set("Authorization", "Bearer\t"+raw)
			case "trailing space":
				request.Header.Set("Authorization", "Bearer "+raw+" ")
			case "list":
				request.Header.Set("Authorization", "Bearer "+raw+", Bearer "+raw)
			case "duplicate":
				request.Header.Add("Authorization", "Bearer "+raw)
			case "cancelled":
				cancel()
			case "cancel after lookup":
				service.config.AccessTokens = accessStoreFunc(func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error) { cancel(); return true, nil })
			}
			handler, _ := NewHandler(service)
			called := false
			writer := httptest.NewRecorder()
			handler.ProtectBearer(func(writer http.ResponseWriter, _ *http.Request, principal BearerPrincipal) {
				called = true
				if principal.AccountID.String() != claims["sub"] || principal.ClientID != "first" {
					t.Fatal("다음 경계에 잘못된 계정 전달")
				}
				writer.WriteHeader(http.StatusNoContent)
			}).ServeHTTP(writer, request)
			if writer.Code != status || called != (status == 204) {
				t.Fatalf("HTTP 오류 분류: status=%d called=%t", writer.Code, called)
			}
			challenge := writer.Header().Get("WWW-Authenticate")
			if status == 401 || status == 403 {
				if !strings.Contains(challenge, `resource_metadata="https://resource.test/.well-known/oauth-protected-resource/mcp"`) || !strings.Contains(challenge, `scope="agent-context"`) || strings.Contains(challenge, "attacker.test") || (code != "" && !strings.Contains(challenge, `error="`+code+`"`)) || (code == "" && strings.Contains(challenge, "error=")) {
					t.Fatal("고정 Bearer 도전 불일치")
				}
			} else if challenge != "" {
				t.Fatal("성공·장애·취소가 재인증을 요청")
			}
			if code != "" && !strings.Contains(writer.Body.String(), `"error":"`+code+`"`) {
				t.Fatal("안전한 오류 본문 불일치")
			}
			for _, secret := range []string{raw, "synthetic-storage-secret", "synthetic-key-secret"} {
				if strings.Contains(writer.Body.String()+challenge, secret) {
					t.Fatal("오류 응답에 자격·내부 원문 노출")
				}
			}
			if (status == 401 || status == 403 || status == 500) && writer.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("인증 오류 캐시 허용")
			}
		})
	}
}
