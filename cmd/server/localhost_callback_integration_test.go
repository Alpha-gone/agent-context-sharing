package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/web"
)

func TestLocalhostCallbackConsentAndTokenIntegration(t *testing.T) {
	database := newPublicAuthenticationStore(t)
	cfg := authz.Config{Issuer: "https://issuer.test", BcryptCost: 4, OAuth: &authz.OAuthConfig{Resource: "https://resource.test/mcp", Clients: database}, AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database}
	service, err := authz.New(database, cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := authz.NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	security := transportSecurity{directTLS: true}
	adapter := webAuthentication{service: service}
	screen, err := web.New(adapter, database, web.Config{SecureCookie: security.isTLSRequest, Authorization: adapter})
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(database, nil, security)
	app.web = screen
	app.oauthMetadata = handler
	app.oauthTokens = handler
	app.oauthRegistration = authz.NewRegistrationHandler(service, security.clientAddress)
	server := httptest.NewTLSServer(app.handler())
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	registration, err := client.Post(server.URL+"/oauth/register", "application/json", strings.NewReader(`{"redirect_uris":["http://localhost:49153/callback?fixed=1"],"client_name":"Claude Code"}`))
	if err != nil {
		t.Fatal(err)
	}
	var registered authz.RegisteredClient
	err = json.UnmarshalRead(registration.Body, &registered)
	registration.Body.Close()
	if err != nil || registration.StatusCode != 201 {
		t.Fatalf("localhost DCR: %d %v", registration.StatusCode, err)
	}
	account, err := service.Register(t.Context(), "localhost_owner", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.WebSession(t.Context(), account, "web")
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"client_id": {registered.ClientID}, "redirect_uri": {registered.RedirectURIs[0]}, "resource": {cfg.OAuth.Resource}, "response_type": {"code"}, "scope": {"agent-context"}, "code_challenge_method": {"S256"}, "code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "state": {"issue300"}}
	do := func(method string, parameters url.Values, body string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+"/authorize?"+parameters.Encode(), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: "__Host-agent_context_session", Value: session.Raw})
		if method == "POST" {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", server.URL)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response
	}
	for _, callback := range []string{"http://localhost:49154/callback?fixed=1", "http://127.0.0.1:49153/callback?fixed=1", "http://localhost:49153/other?fixed=1", "http://localhost:49153/callback?fixed=2", "http://localhost.evil.test:49153/callback?fixed=1"} {
		query := values.Clone()
		query.Set("redirect_uri", callback)
		response := do("GET", query, "")
		if response.StatusCode != 400 || response.Header.Get("Location") != "" {
			t.Fatalf("unregistered redirect allowed: %q %d", callback, response.StatusCode)
		}
	}
	response := do("GET", values, "")
	if response.StatusCode != 200 {
		t.Fatalf("consent view=%d", response.StatusCode)
	}
	response = do("POST", values, "decision=allow")
	if response.StatusCode != 303 {
		t.Fatalf("consent=%d", response.StatusCode)
	}
	callback, err := url.Parse(response.Header.Get("Location"))
	if err != nil || callback.Scheme != "http" || callback.Host != "localhost:49153" || callback.Path != "/callback" || callback.Query().Get("fixed") != "1" || callback.Query().Get("state") != "issue300" || callback.Query().Get("iss") != cfg.Issuer {
		t.Fatal("callback binding lost")
	}
	tokenForm := url.Values{"grant_type": {"authorization_code"}, "client_id": {registered.ClientID}, "redirect_uri": {registered.RedirectURIs[0]}, "resource": {cfg.OAuth.Resource}, "code": {callback.Query().Get("code")}, "code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"}}
	wrong := tokenForm.Clone()
	wrong.Set("redirect_uri", "http://localhost:49154/callback?fixed=1")
	denied, err := client.PostForm(server.URL+"/token", wrong)
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != 400 {
		t.Fatal("token redirect port changed")
	}
	granted, err := client.PostForm(server.URL+"/token", tokenForm)
	if err != nil {
		t.Fatal(err)
	}
	var tokens authz.OAuthTokens
	err = json.UnmarshalRead(granted.Body, &tokens)
	granted.Body.Close()
	if err != nil || granted.StatusCode != 200 {
		t.Fatalf("token exchange=%d %v", granted.StatusCode, err)
	}
	principal, err := service.VerifyBearer(t.Context(), tokens.AccessToken)
	if err != nil || principal.AccountID != account {
		t.Fatal("localhost token validation", err)
	}
}
