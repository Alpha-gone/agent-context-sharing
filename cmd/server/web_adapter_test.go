package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/web"
)

func TestWebCredentialErrorMapping(t *testing.T) {
	for _, test := range []struct {
		name            string
		input, expected error
	}{
		{"success", nil, nil},
		{"busy", fmt.Errorf("wrapped: %w", authz.ErrPasswordBusy), web.ErrAuthenticationBusy},
		{"unavailable", fmt.Errorf("%w: synthetic-sensitive-marker", authz.ErrUnavailable), web.ErrAuthenticationUnavailable},
		{"mismatch", authz.ErrInvalidCredential, authz.ErrInvalidCredential},
		{"duplicate", authz.ErrLoginIDExists, authz.ErrLoginIDExists},
		{"canceled", context.Canceled, context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !errors.Is(webCredentialError(test.input), test.expected) {
				t.Fatal("웹 자격 오류 사상이 다릅니다")
			}
		})
	}
}

func TestWebSessionRoundTripIntegration(t *testing.T) {
	backend := newPublicAuthenticationStore(t)
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
