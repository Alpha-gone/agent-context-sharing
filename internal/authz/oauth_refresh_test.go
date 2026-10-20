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
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type refreshStoreFunc func(context.Context, store.OAuthRefreshExchange, func() time.Time, func(store.OAuthGrantBinding, time.Time) ([32]byte, error)) error

func (f refreshStoreFunc) RotateOAuthRefreshToken(ctx context.Context, input store.OAuthRefreshExchange, clock func() time.Time, issue func(store.OAuthGrantBinding, time.Time) ([32]byte, error)) error {
	return f(ctx, input, clock, issue)
}

func refreshBinding(t *testing.T) store.OAuthGrantBinding {
	t.Helper()
	account, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	grant, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return store.OAuthGrantBinding{ID: grant, AccountID: account, ClientID: "first", Resource: "https://resource.test/mcp", Scope: "agent-context"}
}

func TestOAuthRefreshValidationAndFailure(t *testing.T) {
	for _, mode := range []string{"empty", "short", "padded", "newline", "noncanonical", "unknown client", "other resource", "scope expansion", "missing oauth", "missing store", "key failure", "invalid grant", "storage failure", "cancel before", "cancel after issue", "commit failure", "success"} {
		t.Run(mode, func(t *testing.T) {
			config := oauthTestConfig()
			raw, client, resource, scope := strings.Repeat("A", 43), "first", config.OAuth.Resource, ""
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			binding := refreshBinding(t)
			calls := 0
			failure := errors.New("synthetic-refresh-secret")
			config.RefreshTokens = refreshStoreFunc(func(ctx context.Context, input store.OAuthRefreshExchange, clock func() time.Time, issue func(store.OAuthGrantBinding, time.Time) ([32]byte, error)) error {
				calls++
				if input.Hash != sha256.Sum256([]byte(raw)) || input.ClientID != client || input.Resource != resource || input.Scope != scope {
					t.Fatal("갱신 해시·결합 전달 오류")
				}
				switch mode {
				case "invalid grant":
					return store.ErrInvalidOAuthGrant
				case "storage failure":
					return failure
				}
				if _, err := issue(binding, clock()); err != nil {
					return err
				}
				if mode == "cancel after issue" {
					cancel()
				}
				if mode == "commit failure" {
					return failure
				}
				return nil
			})
			service, err := New(newMemoryStore(), config)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "empty":
				raw = ""
			case "short":
				raw = "short"
			case "padded":
				raw += "="
			case "newline":
				raw += "\n"
			case "noncanonical":
				raw = strings.Repeat("A", 42) + "B"
			case "unknown client":
				client = "unknown"
			case "other resource":
				resource = "https://other.test/mcp"
			case "scope expansion":
				scope = "agent-context admin"
			case "missing oauth":
				service.oauth = nil
			case "missing store":
				service.config.RefreshTokens = nil
			case "key failure":
				if _, err := service.activeKey(t.Context()); err != nil {
					t.Fatal(err)
				}
				service.store = &oauthBrokenKeyStore{authStore: service.store}
			case "cancel before":
				cancel()
			}
			tokens, err := service.Refresh(ctx, raw, client, resource, scope)
			switch mode {
			case "success":
				if err != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" {
					t.Fatal("정상 회전 실패")
				}
			case "cancel before", "cancel after issue":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("취소 원인 누락")
				}
			case "missing store", "key failure", "storage failure", "commit failure":
				if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "synthetic-refresh-secret") {
					t.Fatal("장애 분류·원문 비노출 실패")
				}
				if (mode == "storage failure" || mode == "commit failure") && !errors.Is(err, failure) {
					t.Fatal("내부 원인 손실")
				}
			default:
				want := "invalid_grant"
				if mode == "scope expansion" {
					want = "invalid_scope"
				}
				if failure, ok := errors.AsType[*OAuthError](err); !ok || failure.Code != want {
					t.Fatal("갱신 오류 분류 실패")
				}
			}
			if mode != "success" && tokens != (OAuthTokens{}) {
				t.Fatal("실패 회전이 자격 반환")
			}
			wantCalls := 0
			if mode == "invalid grant" || mode == "storage failure" || mode == "cancel after issue" || mode == "commit failure" || mode == "success" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatal("거부 전 저장소 호출")
			}
		})
	}
}

func TestOAuthRefreshIssuedCredentials(t *testing.T) {
	config := oauthTestConfig()
	binding := refreshBinding(t)
	now := time.Now().UTC().Truncate(time.Second)
	var savedHash [32]byte
	config.RefreshTokens = refreshStoreFunc(func(ctx context.Context, input store.OAuthRefreshExchange, clock func() time.Time, issue func(store.OAuthGrantBinding, time.Time) ([32]byte, error)) error {
		var err error
		savedHash, err = issue(binding, now)
		return err
	})
	service, err := New(newMemoryStore(), config)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	tokens, err := service.Refresh(t.Context(), strings.Repeat("A", 43), "first", config.OAuth.Resource, "agent-context")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(tokens.RefreshToken)
	if err != nil || len(decoded) != 32 || savedHash != sha256.Sum256([]byte(tokens.RefreshToken)) || tokens.RefreshToken == strings.Repeat("A", 43) || tokens.TokenType != "Bearer" || tokens.ExpiresIn != 3600 || tokens.Scope != "agent-context" {
		t.Fatal("발급 난수·해시·외피 불일치")
	}
	parsed, err := jwt.ParseSigned(tokens.AccessToken, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := service.JWKS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var claims jwt.Claims
	var extra struct {
		Scope  string `json:"scope"`
		Client string `json:"client_id"`
		Grant  string `json:"grant_id"`
	}
	if err := parsed.Claims(keys.Keys[0].Key, &claims, &extra); err != nil {
		t.Fatal("접근 서명 실패")
	}
	if claims.Issuer != config.Issuer || claims.Subject != binding.AccountID.String() || len(claims.Audience) != 1 || claims.Audience[0] != config.OAuth.Resource || !claims.IssuedAt.Time().Equal(now) || !claims.Expiry.Time().Equal(now.Add(time.Hour)) || claims.ID == "" || extra.Scope != "agent-context" || extra.Client != "first" || extra.Grant != binding.ID.String() {
		t.Fatal("접근 토큰 결합·수명 불일치")
	}
	if _, _, err := service.Verify(t.Context(), tokens.AccessToken, "web"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("갱신 접근 토큰을 웹 세션으로 수락")
	}
}

func TestOAuthRefreshHTTPBoundary(t *testing.T) {
	for _, mode := range []string{"success", "same scope", "empty scope", "scope expansion", "missing refresh", "missing client", "missing resource", "duplicate refresh", "duplicate scope", "header credential", "client secret", "query", "wrong content type", "announced over limit", "chunked over limit", "exact limit", "below limit", "invalid grant", "storage failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {strings.Repeat("A", 43)}, "client_id": {"first"}, "resource": {"https://resource.test/mcp"}}
			status, code, calls := 200, "", 0
			query, contentType, header := "", "application/x-www-form-urlencoded", ""
			switch mode {
			case "same scope":
				form.Set("scope", "agent-context")
			case "empty scope":
				form.Set("scope", "")
			case "scope expansion":
				form.Set("scope", "agent-context admin")
				status, code = 400, "invalid_scope"
			case "missing refresh":
				form.Del("refresh_token")
				status, code = 400, "invalid_request"
			case "missing client":
				form.Del("client_id")
				status, code = 400, "invalid_request"
			case "missing resource":
				form.Del("resource")
				status, code = 400, "invalid_request"
			case "duplicate refresh":
				form.Add("refresh_token", "other")
				status, code = 400, "invalid_request"
			case "duplicate scope":
				form["scope"] = []string{"agent-context", "agent-context"}
				status, code = 400, "invalid_request"
			case "header credential":
				header = "Basic synthetic"
				status, code = 400, "invalid_client"
			case "client secret":
				form.Set("client_secret", "synthetic")
				status, code = 400, "invalid_client"
			case "query":
				query = "?refresh_token=other"
				status, code = 400, "invalid_request"
			case "wrong content type":
				contentType = "application/json"
				status, code = 400, "invalid_request"
			case "announced over limit", "chunked over limit":
				status, code = 413, "invalid_request"
			case "invalid grant", "exact limit", "below limit":
				status, code = 400, "invalid_grant"
			case "storage failure":
				status, code = 500, "server_error"
			case "cancelled":
				status = 200
			}
			body := form.Encode()
			if mode == "chunked over limit" {
				body += "&padding=" + strings.Repeat("x", tokenFormLimit)
			}
			if mode == "exact limit" || mode == "below limit" {
				n := tokenFormLimit - len(body) - len("&padding=")
				if mode == "below limit" {
					n--
				}
				body += "&padding=" + strings.Repeat("x", n)
			}
			config := oauthTestConfig()
			binding := refreshBinding(t)
			config.RefreshTokens = refreshStoreFunc(func(ctx context.Context, input store.OAuthRefreshExchange, clock func() time.Time, issue func(store.OAuthGrantBinding, time.Time) ([32]byte, error)) error {
				calls++
				if code == "invalid_grant" {
					return store.ErrInvalidOAuthGrant
				}
				if code == "server_error" {
					return errors.New("synthetic-refresh-secret")
				}
				_, err := issue(binding, clock())
				return err
			})
			service, err := New(newMemoryStore(), config)
			if err != nil {
				t.Fatal(err)
			}
			handler, _ := NewHandler(service)
			request := httptest.NewRequest("POST", "https://issuer.test/token"+query, strings.NewReader(body))
			request.Header.Set("Content-Type", contentType)
			if header != "" {
				request.Header.Set("Authorization", header)
			}
			if mode == "announced over limit" {
				request.ContentLength = tokenFormLimit + 1
			}
			if mode == "chunked over limit" {
				request.ContentLength = -1
			}
			if mode == "cancelled" {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			writer := httptest.NewRecorder()
			handler.Token(writer, request)
			if writer.Code != status {
				t.Fatalf("갱신 HTTP 상태=%d", writer.Code)
			}
			if mode == "cancelled" {
				if writer.Body.Len() != 0 || calls != 0 {
					t.Fatal("취소 요청 처리")
				}
				return
			}
			var data map[string]any
			if json.Unmarshal(writer.Body.Bytes(), &data) != nil {
				t.Fatal("OAuth JSON 누락")
			}
			if status == 200 {
				if data["token_type"] != "Bearer" || data["access_token"] == nil || data["refresh_token"] == nil || calls != 1 {
					t.Fatal("갱신 응답 실패")
				}
			} else if data["error"] != code || len(data) != 1 {
				t.Fatal("OAuth 오류 외피 불일치")
			}
			if writer.Header().Get("Cache-Control") != "no-store" || writer.Header().Get("Pragma") != "no-cache" || writer.Header().Get("WWW-Authenticate") != "" || writer.Header().Get("Location") != "" || len(writer.Result().Cookies()) != 0 || strings.Contains(writer.Body.String(), "synthetic-refresh-secret") {
				t.Fatal("갱신 응답 캐시·채널·비노출 오류")
			}
			if status == 413 || code == "invalid_request" || code == "invalid_client" || code == "invalid_scope" {
				if calls != 0 {
					t.Fatal("입력 거부가 저장소에 진입")
				}
			}
		})
	}
}
