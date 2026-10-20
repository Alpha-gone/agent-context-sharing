package main

import (
	"errors"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
)

func TestAgentBearerIdentityIntegration(t *testing.T) {
	backend := newPublicAuthenticationStore(t)
	config := authz.Config{Issuer: "https://issuer.test", BcryptCost: 4, AuthorizationCodes: backend, AccessTokens: backend, RefreshTokens: backend, OAuth: &authz.OAuthConfig{Resource: "https://resource.test/mcp", Clients: map[string]authz.OAuthClient{
		"first":  {RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "exact"},
		"second": {RedirectURIs: []string{"http://127.0.0.1:49153/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "exact"},
	}}}
	service, err := authz.New(backend, config)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := service.Register(t.Context(), "agent_account", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	issue := func(client string) (authz.OAuthTokens, string, string) {
		t.Helper()
		callback := config.OAuth.Clients[client].RedirectURIs[0]
		code, err := service.IssueAuthorizationCode(t.Context(), owner, authz.AuthorizationRequest{ClientID: client, RedirectURI: callback, Resource: config.OAuth.Resource, ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := service.ExchangeAuthorizationCode(t.Context(), code, client, callback, config.OAuth.Resource, verifier)
		if err != nil {
			t.Fatal(err)
		}
		return tokens, code, callback
	}
	principal := func(tokens authz.OAuthTokens) mcp.Principal {
		t.Helper()
		verified, err := service.VerifyBearer(t.Context(), tokens.AccessToken)
		if err != nil {
			t.Fatal("Bearer 인증 실패")
		}
		return mcp.Principal{AccountID: verified.AccountID, ClientID: verified.ClientID}
	}
	first, code, callback := issue("first")
	second, _, _ := issue("second")
	a, b := principal(first), principal(second)
	id, err := mcp.RegisterAgent(t.Context(), backend, a, "registration_key_1")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := mcp.RegisterAgent(t.Context(), backend, b, "registration_key_1")
	if err != nil || id == otherID {
		t.Fatal("같은 계정의 다른 OAuth client 등록 공유")
	}
	if actor, err := mcp.BindActor(t.Context(), backend, b, id); err == nil || actor != (mcp.Actor{}) {
		t.Fatal("다른 client의 등록으로 행위자 획득")
	}
	if actor, err := mcp.BindActor(t.Context(), backend, a, id); err != nil || actor.AccountID != owner || actor.AgentID != id {
		t.Fatal("인증된 계정/client 바인딩 실패")
	}
	if _, err := service.ExchangeAuthorizationCode(t.Context(), code, "first", callback, config.OAuth.Resource, verifier); err == nil {
		t.Fatal("코드 재사용 허용")
	}
	if _, err := service.VerifyBearer(t.Context(), first.AccessToken); !errors.Is(err, authz.ErrInvalidCredential) {
		t.Fatal("폐기 계열의 인증 허용")
	}
	newGrant, _, _ := issue("first")
	newPrincipal := principal(newGrant)
	if again, err := mcp.RegisterAgent(t.Context(), backend, newPrincipal, "registration_key_1"); err != nil || again != id {
		t.Fatal("OAuth 계열 폐기가 독립 등록을 삭제")
	}
	webSession, err := service.WebSession(t.Context(), owner, "web")
	if err != nil {
		t.Fatal(err)
	}
	otherAccount, err := service.Register(t.Context(), "other_agent_account", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.RevokeAgent(t.Context(), otherAccount, id); err == nil {
		t.Fatal("타 계정의 등록 폐기 허용")
	}
	if err := backend.RevokeAgent(t.Context(), owner, id); err != nil {
		t.Fatal(err)
	}
	actor, err := mcp.BindActor(t.Context(), backend, newPrincipal, id)
	field, ok := errors.AsType[model.FieldError](err)
	if !ok || field.Field != "agent_id" || actor != (mcp.Actor{}) {
		t.Fatal("본인 등록 폐기 미반영")
	}
	if _, err := mcp.BindActor(t.Context(), backend, b, otherID); err != nil {
		t.Fatal("다른 client 등록까지 폐기")
	}
	if _, _, err := service.Verify(t.Context(), webSession.Raw, "web"); err != nil {
		t.Fatal("등록 폐기가 웹 세션 폐기")
	}
}
