package main

import (
	"context"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/web"
)

// webAuthentication은 웹 전송 계층이 authz 패키지 구현에 직접 의존하지 않도록 경계를 맞춘다.
type webAuthentication struct{ service *authz.Service }

func (adapter webAuthentication) Authenticate(ctx context.Context, loginID, password string) (model.ID, error) {
	return adapter.service.Authenticate(ctx, loginID, password)
}

func (adapter webAuthentication) Register(ctx context.Context, loginID, password string) (model.ID, error) {
	return adapter.service.Register(ctx, loginID, password)
}

func (adapter webAuthentication) WebSession(ctx context.Context, accountID model.ID, audience string) (web.Session, error) {
	token, err := adapter.service.WebSession(ctx, accountID, audience)
	return web.Session{Raw: token.Raw, ID: token.ID, ExpiresAt: token.ExpiresAt}, err
}

func (adapter webAuthentication) VerifyWebSession(ctx context.Context, raw, audience string) (model.ID, web.Session, error) {
	accountID, token, err := adapter.service.Verify(ctx, raw, audience)
	return accountID, web.Session{Raw: token.Raw, ID: token.ID, ExpiresAt: token.ExpiresAt}, err
}

func (adapter webAuthentication) Revoke(ctx context.Context, session web.Session) error {
	return adapter.service.Revoke(ctx, authz.Token{Raw: session.Raw, ID: session.ID, ExpiresAt: session.ExpiresAt})
}
