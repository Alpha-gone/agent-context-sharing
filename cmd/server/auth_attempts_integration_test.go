package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

func TestPublicAuthenticationAttemptsIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_URL이 필요합니다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 공개 인증 제한 통합 시험을 건너뜁니다")
	}
	graph := os.Getenv("AGE_GRAPH_NAME")
	if graph == "" {
		graph = "agent_context"
	}
	backend, err := store.New(t.Context(), dsn, graph, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	service, err := authz.New(backend, authz.Config{Issuer: "https://service.test", Resource: "https://service.test/mcp", Scope: "agent-context", BcryptCost: 4, Clients: map[string][]*url.URL{"test-client": {{Scheme: "http", Host: "127.0.0.1", Path: "/callback"}}}})
	if err != nil {
		t.Fatal(err)
	}
	security := transportSecurity{trustedProxies: []netip.Prefix{netip.MustParsePrefix("10.203.0.1/32")}}
	server, err := web.New(webAuthentication{service: service}, backend, web.Config{SecureCookie: security.isTLSRequest, ClientAddress: security.clientAddress})
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(backend, nil, security, nil)
	app.web = server
	handler := app.handler()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	login := "limit_" + strings.ReplaceAll(id.String(), "-", "")[:12]
	post := func(path, login, password, address string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"login_id": {login}, "password": {password}}.Encode()))
		request.RemoteAddr = "10.203.0.1:1234"
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("X-Forwarded-Proto", "https")
		request.Header.Set("CF-Connecting-IP", address)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := post("/register", login, "correct password", "198.51.100.1"); response.Code != 303 {
		t.Fatalf("정상 가입 = %d", response.Code)
	}
	account, err := backend.AccountByLoginID(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	response := post("/login", login, "correct password", "198.51.100.1")
	if response.Code != 303 || len(response.Result().Cookies()) != 1 {
		t.Fatalf("정상 로그인 = %d", response.Code)
	}
	if verified, _, err := service.Verify(t.Context(), response.Result().Cookies()[0].Value, web.SessionAudience); err != nil || verified != account.ID {
		t.Fatal("실제 로그인 세션 검증 실패", err)
	}
	if response := post("/register", login, "correct password", "198.51.100.1"); response.Code != 400 {
		t.Fatal("중복 가입 결과 변경")
	}
	var mismatch string
	for i := range 7 {
		name := login
		if i%2 == 0 {
			name = "absent_" + login
		}
		response := post("/login", name, "wrong password", "198.51.100.1")
		if response.Code != 401 {
			t.Fatalf("불일치 응답 = %d", response.Code)
		}
		// 아이디 반영을 제외한 안내는 존재 여부에 따라 달라지지 않는다.
		message := "로그인 아이디 또는 비밀번호가 올바르지 않습니다."
		if !strings.Contains(response.Body.String(), message) {
			t.Fatal("존재 여부별 안내 차이")
		}
		mismatch = message
	}
	response = post("/login", login, "correct password", "198.51.100.1")
	if response.Code != 429 || response.Header().Get("Retry-After") == "" || len(response.Result().Cookies()) != 0 || strings.Contains(response.Body.String(), mismatch) {
		t.Fatal("상한 초과에서 실제 인증·세션 발급")
	}
	if response := post("/login", login, "correct password", "198.51.100.2"); response.Code != 303 {
		t.Fatal("다른 출처에서 같은 계정이 잠겼습니다")
	}
}
