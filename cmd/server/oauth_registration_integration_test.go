package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
)

func TestDCRAccountAndRestartIntegration(t *testing.T) {
	database := newPublicAuthenticationStore(t)
	cfg := authz.Config{Issuer: "https://issuer.test", BcryptCost: 4, OAuth: &authz.OAuthConfig{Resource: "https://resource.test/mcp", Clients: database}, AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database, Connections: database}
	service, err := authz.New(database, cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := authz.NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(database, nil, transportSecurity{directTLS: true})
	app.oauthMetadata = handler
	app.oauthTokens = handler
	app.oauthRegistration = authz.NewRegistrationHandler(service, app.transport.clientAddress)
	server := httptest.NewTLSServer(app.handler())
	t.Cleanup(server.Close)
	response, err := server.Client().Post(server.URL+"/oauth/register", "application/json", strings.NewReader(`{"redirect_uris":["http://127.0.0.1:49152/callback"],"client_name":"Codex"}`))
	if err != nil {
		t.Fatal(err)
	}
	var client authz.RegisteredClient
	err = json.UnmarshalRead(response.Body, &client)
	response.Body.Close()
	if err != nil || response.StatusCode != 201 || !strings.HasPrefix(client.ClientID, "dcr_") {
		t.Fatalf("DCR registration: %d %v", response.StatusCode, err)
	}
	issue := func(login string) (authz.OAuthTokens, model.ID) {
		t.Helper()
		account, err := service.Register(t.Context(), login, "correct password")
		if err != nil {
			t.Fatal(err)
		}
		code, err := service.IssueAuthorizationCode(t.Context(), account, authz.AuthorizationRequest{ClientID: client.ClientID, RedirectURI: client.RedirectURIs[0], Resource: cfg.OAuth.Resource, Scope: "agent-context", ResponseType: "code", ChallengeMethod: "S256", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"})
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().PostForm(server.URL+"/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ClientID}, "redirect_uri": {client.RedirectURIs[0]}, "resource": {cfg.OAuth.Resource}, "code": {code}, "code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"}})
		if err != nil {
			t.Fatal(err)
		}
		var tokens authz.OAuthTokens
		err = json.UnmarshalRead(response.Body, &tokens)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("token exchange: %d %v", response.StatusCode, err)
		}
		return tokens, account
	}
	first, firstAccount := issue("dcr_first")
	second, secondAccount := issue("dcr_second")
	restarted, err := authz.New(database, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		token   authz.OAuthTokens
		account model.ID
	}{{first, firstAccount}, {second, secondAccount}} {
		principal, err := restarted.VerifyBearer(t.Context(), sample.token.AccessToken)
		if err != nil || principal.AccountID != sample.account || principal.ClientID != client.ClientID {
			t.Fatalf("account binding after restart: %+v %v", principal, err)
		}
	}
	refreshed, err := restarted.Refresh(t.Context(), first.RefreshToken, client.ClientID, cfg.OAuth.Resource, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.VerifyBearer(t.Context(), refreshed.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.VerifyBearer(t.Context(), second.AccessToken); err != nil {
		t.Fatal("other account affected", err)
	}
	if err := restarted.ValidateAuthorizationTarget(t.Context(), "codex-cli-first-test", client.RedirectURIs[0], cfg.OAuth.Resource); err == nil {
		t.Fatal("사전 등록 ID를 허용했습니다")
	}
}
