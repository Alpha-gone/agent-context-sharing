package main

import (
	"context"
	"errors"
	"net/url"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/web"
)

// webAuthentication은 웹 전송 계층이 authz 패키지 구현에 직접 의존하지 않도록 경계를 맞춘다.
type webAuthentication struct{ service *authz.Service }

func (adapter webAuthentication) ValidateAuthorizationTarget(client, callback, resource string) error {
	return adapter.service.ValidateAuthorizationTarget(client, callback, resource)
}

func authorizationRequest(values url.Values) authz.AuthorizationRequest {
	return authz.AuthorizationRequest{ClientID: values.Get("client_id"), RedirectURI: values.Get("redirect_uri"), Resource: values.Get("resource"), ResponseType: values.Get("response_type"), Scope: values.Get("scope"), Challenge: values.Get("code_challenge"), ChallengeMethod: values.Get("code_challenge_method")}
}

func (adapter webAuthentication) ValidateAuthorizationRequest(values url.Values) string {
	err := adapter.service.ValidateAuthorizationRequest(authorizationRequest(values))
	if err == nil {
		return ""
	}
	if failure, ok := errors.AsType[*authz.OAuthError](err); ok {
		return failure.Code
	}
	return "invalid_request"
}

func (adapter webAuthentication) IssueAuthorizationCode(ctx context.Context, account model.ID, values url.Values) (string, error) {
	return adapter.service.IssueAuthorizationCode(ctx, account, authorizationRequest(values))
}

func (adapter webAuthentication) AuthorizationIssuer() string {
	return adapter.service.AuthorizationIssuer()
}

func (adapter webAuthentication) Authenticate(ctx context.Context, loginID, password string) (model.ID, error) {
	id, err := adapter.service.Authenticate(ctx, loginID, password)
	return id, webCredentialError(err)
}

func (adapter webAuthentication) Register(ctx context.Context, loginID, password string) (model.ID, error) {
	id, err := adapter.service.Register(ctx, loginID, password)
	return id, webCredentialError(err)
}

func webCredentialError(err error) error {
	if errors.Is(err, authz.ErrPasswordBusy) {
		return web.ErrAuthenticationBusy
	}
	if errors.Is(err, authz.ErrUnavailable) {
		return web.ErrAuthenticationUnavailable
	}
	return err
}

func (adapter webAuthentication) WebSession(ctx context.Context, accountID model.ID, audience string) (web.Session, error) {
	token, err := adapter.service.WebSession(ctx, accountID, audience)
	return web.Session{Raw: token.Raw, ID: token.ID, ExpiresAt: token.ExpiresAt}, webCredentialError(err)
}

func (adapter webAuthentication) VerifyWebSession(ctx context.Context, raw, audience string) (model.ID, web.Session, error) {
	accountID, token, err := adapter.service.Verify(ctx, raw, audience)
	return accountID, web.Session{Raw: token.Raw, ID: token.ID, ExpiresAt: token.ExpiresAt}, webCredentialError(err)
}

func (adapter webAuthentication) Revoke(ctx context.Context, session web.Session) error {
	return webCredentialError(adapter.service.Revoke(ctx, authz.Token{Raw: session.Raw, ID: session.ID, ExpiresAt: session.ExpiresAt}))
}
