package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/web"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type connectionLog struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (log *connectionLog) Write(data []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.data.Write(data)
}
func (log *connectionLog) String() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.data.String()
}

func TestAUTH10ConnectionsTLSIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	config := authz.Config{Issuer: "https://issuer.test", BcryptCost: 4, AuthorizationCodes: f.database, AccessTokens: f.database, RefreshTokens: f.database, Connections: f.database, OAuth: &authz.OAuthConfig{Resource: "https://resource.test/mcp", Clients: map[string]authz.OAuthClient{"first": {RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "exact"}}}}
	var services []*authz.Service
	var servers []*httptest.Server
	logs := &connectionLog{}
	for range 2 {
		service, err := authz.New(f.database, config)
		if err != nil {
			t.Fatal(err)
		}
		services = append(services, service)
		public, err := authz.NewHandler(service)
		if err != nil {
			t.Fatal(err)
		}
		handlers := map[string]mcp.ToolHandler{}
		for name, handler := range f.handlers {
			handlers[name] = handler
		}
		for _, name := range []string{"agent_register", "graph_list", "graph_get", "node_get", "context_flow_get", "relation_list"} {
			handlers[name] = func(context.Context, mcp.Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				t.Error("AUTH-10 specimen called unrelated tool")
				return nil, io.ErrUnexpectedEOF
			}
		}
		protected, err := newMCPHandler(public, []string{"https://allowed.test"}, handlers)
		if err != nil {
			t.Fatal(err)
		}
		security := transportSecurity{directTLS: true}
		adapter := webAuthentication{service: service}
		screen, err := web.New(adapter, f.database, web.Config{SecureCookie: security.isTLSRequest, Connections: adapter, Authorization: adapter})
		if err != nil {
			t.Fatal(err)
		}
		app := newApplication(f.database, slog.New(slog.NewJSONHandler(logs, nil)), security)
		app.web, app.oauthTokens, app.mcp = screen, public, protected
		server := httptest.NewTLSServer(app.handler())
		t.Cleanup(server.Close)
		server.Client().Timeout = 5 * time.Second
		server.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		t.Cleanup(server.Client().CloseIdleConnections)
		servers = append(servers, server)
	}
	issue := func() (authz.OAuthTokens, model.ID) {
		t.Helper()
		code, err := services[0].IssueAuthorizationCode(t.Context(), f.principal.AccountID, authz.AuthorizationRequest{ClientID: "first", RedirectURI: "http://127.0.0.1:49152/callback", Resource: config.OAuth.Resource, ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := services[0].ExchangeAuthorizationCode(t.Context(), code, "first", "http://127.0.0.1:49152/callback", config.OAuth.Resource, "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
		if err != nil {
			t.Fatal(err)
		}
		p, err := services[1].VerifyBearer(t.Context(), tokens.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		return tokens, p.GrantID
	}
	tokens, grant := issue()
	session, err := services[0].WebSession(t.Context(), f.principal.AccountID, "web")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "agent_context_session", Value: session.Raw}
	do := func(index int, method, path, body string, cookie *http.Cookie, headers map[string]string) (*http.Response, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, servers[index].URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if method == "POST" {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := servers[index].Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.ParseID(response.Header.Get("X-Correlation-ID")); err != nil {
			t.Fatal("flow ID missing")
		}
		if response.Header.Get("Cache-Control") != "no-store" || len(response.Cookies()) != 0 {
			t.Fatal("cache or session cookie changed")
		}
		return response, data
	}
	revoke := func(action string, target model.ID) {
		t.Helper()
		response, _ := do(1, "POST", "/connections", url.Values{"action": {action}, "target_id": {target.String()}}.Encode(), cookie, nil)
		if response.StatusCode != 303 || response.Header.Get("Location") != "/connections" {
			t.Fatal("web revoke failed")
		}
	}
	write := func(token string, key string) (int, map[string]any) {
		t.Helper()
		args := f.args("node_update")
		args["context_id"], args["expected_version"], args["body"], args["request_id"] = f.derived.String(), 1, "AUTH-10 TLS 변경", key
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "auth-10", "method": "tools/call", "params": map[string]any{"name": "node_update", "arguments": args, "_meta": map[string]any{sdk.MetaKeyProtocolVersion: "2026-07-28", sdk.MetaKeyClientCapabilities: map[string]any{}, sdk.MetaKeyClientInfo: map[string]any{"name": "auth10", "version": "1"}}}})
		if err != nil {
			t.Fatal(err)
		}
		response, data := do(0, "POST", "/mcp", string(body), nil, map[string]string{"Authorization": "Bearer " + token, "Origin": "https://allowed.test", "Content-Type": "application/json", "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "node_update"})
		var value map[string]any
		if json.Unmarshal(data, &value) != nil {
			t.Fatal("MCP response invalid")
		}
		return response.StatusCode, value
	}
	response, data := do(1, "GET", "/connections", "", cookie, nil)
	if response.StatusCode != 200 || !bytes.Contains(data, []byte(grant.String())) || !bytes.Contains(data, []byte(f.agent.String())) {
		t.Fatal("own connections missing")
	}
	for _, secret := range []string{tokens.AccessToken, tokens.RefreshToken, session.Raw, "registration_key_1"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("credential shown")
		}
	}
	status, result := write(tokens.AccessToken, "auth10_write_before")
	if status != 200 || result["result"].(map[string]any)["isError"] == true {
		t.Fatal("initial authenticated write failed")
	}
	before := mcpWriteFingerprint(t, f.pool)
	revoke("revoke_grant", grant)
	if mcpWriteFingerprint(t, f.pool) != before {
		t.Fatal("grant revocation changed contribution or audit")
	}
	if status, _ := write(tokens.AccessToken, "auth10_write_denied"); status != 401 {
		t.Fatal("remote bearer survived revoke")
	}
	response, data = do(0, "POST", "/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens.RefreshToken}, "client_id": {"first"}, "resource": {config.OAuth.Resource}}.Encode(), nil, nil)
	if response.StatusCode != 400 || !bytes.Contains(data, []byte("invalid_grant")) {
		t.Fatal("refresh survived revoke")
	}
	response, _ = do(0, "GET", "/connections", "", cookie, nil)
	if response.StatusCode != 200 {
		t.Fatal("web session lost")
	}
	fresh, _ := issue()
	if err := f.database.ValidateAgent(t.Context(), f.principal.AccountID, "first", f.agent); err != nil {
		t.Fatal("grant deleted registration")
	}
	before = mcpWriteFingerprint(t, f.pool)
	revoke("revoke_agent", f.agent)
	if mcpWriteFingerprint(t, f.pool) != before {
		t.Fatal("agent revocation changed prior contribution or audit")
	}
	status, result = write(fresh.AccessToken, "auth10_agent_denied")
	if status != 200 || result["result"].(map[string]any)["isError"] != true || result["result"].(map[string]any)["structuredContent"].(map[string]any)["code"] != "invalid_argument" {
		t.Fatal("revoked agent accepted write")
	}
	for _, service := range services {
		if _, err := service.VerifyBearer(t.Context(), fresh.AccessToken); err != nil {
			t.Fatal("agent revoke invalidated bearer")
		}
	}
	response, data = do(0, "GET", "/connections", "", cookie, nil)
	if response.StatusCode != 200 || bytes.Contains(data, []byte(f.agent.String())) {
		t.Fatal("revoked registration still listed")
	}
	for _, secret := range []string{tokens.AccessToken, tokens.RefreshToken, fresh.AccessToken, fresh.RefreshToken, session.Raw, "registration_key_1"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("credential in request logs")
		}
	}
}
