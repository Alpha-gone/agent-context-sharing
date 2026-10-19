package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/web"
)

func TestOAuthAuthorizationTLSIntegration(t *testing.T) {
	backend := newPublicAuthenticationStore(t)
	config := authz.Config{Issuer: "https://issuer.test", BcryptCost: 4, AuthorizationCodes: backend, AccessTokens: backend, RefreshTokens: backend, OAuth: &authz.OAuthConfig{Resource: "https://resource.test/mcp", Clients: map[string]authz.OAuthClient{"first": {RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "exact"}}}}
	service, err := authz.New(backend, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(t.Context(), "oauth_http", "correct password"); err != nil {
		t.Fatal("시험 계정 준비 실패")
	}
	security := transportSecurity{directTLS: true}
	adapter := webAuthentication{service: service}
	screen, err := web.New(adapter, backend, web.Config{SecureCookie: security.isTLSRequest, Authorization: adapter})
	if err != nil {
		t.Fatal(err)
	}
	public, err := authz.NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app := newApplication(backend, slog.New(slog.NewJSONHandler(&logs, nil)), security)
	app.web = screen
	app.oauthTokens = public
	app.oauthMetadata = public
	server := httptest.NewTLSServer(app.handler())
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(client.CloseIdleConnections)
	do := func(method, path, body string, cookie *http.Cookie) (*http.Response, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal("요청 준비 실패")
		}
		if method == "POST" {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("TLS 교환 실패")
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal("응답 읽기 실패")
		}
		return response, data
	}
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	digest := sha256.Sum256([]byte(verifier))
	values := url.Values{"response_type": {"code"}, "client_id": {"first"}, "redirect_uri": {"http://127.0.0.1:49152/callback"}, "resource": {"https://resource.test/mcp"}, "scope": {"agent-context"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "state": {"synthetic-state"}}
	path := "/authorize?" + values.Encode()
	response, _ := do("GET", path, "", nil)
	login, err := url.Parse(response.Header.Get("Location"))
	if err != nil || response.StatusCode != 303 || login.Path != "/login" || login.Query().Get("next") != path {
		t.Fatal("로그인으로 인가 요청 전달 실패")
	}
	response, _ = do("POST", "/login", url.Values{"login_id": {"oauth_http"}, "password": {"correct password"}, "next": {login.Query().Get("next")}}.Encode(), nil)
	if response.StatusCode != 303 || response.Header.Get("Location") != path || len(response.Cookies()) != 1 {
		t.Fatal("로그인 뒤 인가 흐름 복원 실패")
	}
	cookie := response.Cookies()[0]
	response, body := do("GET", path, "", cookie)
	if response.StatusCode != 200 || !strings.Contains(string(body), "연결 허용") || !strings.Contains(string(body), "연결 거절") || response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("명시적 동의 화면 누락")
	}
	response, _ = do("POST", path, "decision=deny", cookie)
	denied, err := url.Parse(response.Header.Get("Location"))
	if err != nil || response.StatusCode != 303 || denied.Query().Get("error") != "access_denied" || denied.Query().Get("state") != values.Get("state") || denied.Query().Get("iss") != "https://issuer.test" {
		t.Fatal("동의 거절의 issuer·state 오류")
	}
	response, _ = do("POST", path, "decision=allow", cookie)
	callback, err := url.Parse(response.Header.Get("Location"))
	if err != nil || response.StatusCode != 303 || callback.Query().Get("code") == "" || callback.Query().Get("iss") != "https://issuer.test" || callback.Query().Get("state") != values.Get("state") {
		t.Fatal("최초 인가 성공 응답 오류")
	}
	code := callback.Query().Get("code")
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {"first"}, "redirect_uri": values["redirect_uri"], "resource": values["resource"], "code_verifier": {verifier}}
	response, body = do("POST", "/token", form.Encode(), nil)
	var tokens authz.OAuthTokens
	if response.StatusCode != 200 || json.Unmarshal(body, &tokens) != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.TokenType != "Bearer" || tokens.ExpiresIn != 3600 || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || len(response.Cookies()) != 0 {
		t.Fatal("TLS 최초 토큰 교환 실패")
	}
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens.RefreshToken}, "client_id": {"first"}, "resource": values["resource"]}
	response, body = do("POST", "/token", refreshForm.Encode(), nil)
	var rotated authz.OAuthTokens
	if response.StatusCode != 200 || json.Unmarshal(body, &rotated) != nil || rotated.RefreshToken == "" || rotated.RefreshToken == tokens.RefreshToken || rotated.AccessToken == "" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || len(response.Cookies()) != 0 {
		t.Fatal("TLS 갱신 회전 실패")
	}
	if _, err := service.VerifyBearer(t.Context(), rotated.AccessToken); err != nil {
		t.Fatal("회전 접근 토큰 거부")
	}
	response, body = do("POST", "/token", refreshForm.Encode(), nil)
	var reuse map[string]string
	if response.StatusCode != 400 || json.Unmarshal(body, &reuse) != nil || reuse["error"] != "invalid_grant" {
		t.Fatal("TLS 갱신 재사용 오류 누락")
	}
	if _, err := service.VerifyBearer(t.Context(), rotated.AccessToken); err == nil {
		t.Fatal("갱신 재사용 계열 폐기 누락")
	}
	response, body = do("POST", "/token", form.Encode(), nil)
	var failure map[string]string
	if response.StatusCode != 400 || json.Unmarshal(body, &failure) != nil || failure["error"] != "invalid_grant" {
		t.Fatal("코드 재사용 표준 오류 누락")
	}
	if _, _, err := service.Verify(t.Context(), cookie.Value, "web"); err != nil {
		t.Fatal("OAuth 코드 재사용이 독립 웹 세션을 폐기")
	}
	for _, secret := range []string{code, tokens.AccessToken, tokens.RefreshToken, rotated.AccessToken, rotated.RefreshToken, verifier, cookie.Value, "correct password", "synthetic-state"} {
		hash := sha256.Sum256([]byte(secret))
		if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), hex.EncodeToString(hash[:])) {
			t.Fatal("서버 로그에 OAuth·계정 자격·해시 또는 state 노출")
		}
	}
	for _, target := range []string{"/authorize", "/token"} {
		request := httptest.NewRequest("POST", "http://issuer.test"+target, strings.NewReader("synthetic"))
		recorder := httptest.NewRecorder()
		app.handler().ServeHTTP(recorder, request)
		if recorder.Code != 400 || recorder.Header().Get("Location") != "" {
			t.Fatal("인가 경로의 TLS 선행 판정 누락")
		}
	}
}
