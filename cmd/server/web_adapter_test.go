package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

func TestWebAuthenticationPreservesAuthenticationTimeIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_URL이 필요하다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 웹 인증 연결 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	backend, err := store.New(t.Context(), databaseURL, graphName, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	service, err := authz.New(backend, authz.Config{Issuer: "https://service.test", Resource: "https://service.test/mcp", Scope: "agent-context", BcryptCost: 4, Clients: map[string][]*url.URL{"test-client": {{Scheme: "http", Host: "127.0.0.1", Path: "/callback"}}}})
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	adapter := webAuthentication{service: service}
	session, err := adapter.WebSession(t.Context(), accountID, web.SessionAudience)
	if err != nil || session.AuthenticatedAt.IsZero() {
		t.Fatalf("발급 세션의 인증 시각 누락: %v", err)
	}
	id, verified, err := adapter.VerifyWebSession(t.Context(), session.Raw, web.SessionAudience)
	if err != nil || id != accountID || !verified.AuthenticatedAt.Equal(session.AuthenticatedAt) {
		t.Fatalf("검증 세션의 인증 시각 불일치: %v", err)
	}
	server, err := web.New(adapter, backend, web.Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/authorize", nil)
	request.AddCookie(&http.Cookie{Name: "agent_context_session", Value: session.Raw})
	id, authenticatedAt, ok := server.SessionAccount(request)
	if !ok || id != accountID || !authenticatedAt.Equal(session.AuthenticatedAt) {
		t.Fatalf("인가 경계의 인증 시각 불일치: %t", ok)
	}
}
