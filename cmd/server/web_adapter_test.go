package main

import (
	"os"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

func TestWebSessionRoundTripIntegration(t *testing.T) {
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
	service, err := authz.New(backend, authz.Config{Issuer: "https://service.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	adapter := webAuthentication{service: service}
	session, err := adapter.WebSession(t.Context(), accountID, web.SessionAudience)
	if err != nil || session.Raw == "" || session.ID == "" || session.ExpiresAt.IsZero() {
		t.Fatalf("발급 세션 정보 누락: %v", err)
	}
	id, verified, err := adapter.VerifyWebSession(t.Context(), session.Raw, web.SessionAudience)
	if err != nil || id != accountID || verified.Raw != session.Raw || verified.ID != session.ID || verified.ExpiresAt.Unix() != session.ExpiresAt.Unix() {
		t.Fatalf("검증 세션 정보 불일치: %v", err)
	}
}
