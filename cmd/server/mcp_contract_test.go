package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP-02의 전송 표본이다. 업무 13종은 합성 처리기이며 agent_register만
// 실제 저장 API를 연결한다. MCP-06·07의 업무 도구 인수를 대신하지 않는다.
func TestMCP02ProtectedTLSIntegration(t *testing.T) {
	backend := newPublicAuthenticationStore(t)
	config := authz.Config{Issuer: "https://issuer.test", BcryptCost: 4, AuthorizationCodes: backend, AccessTokens: backend, RefreshTokens: backend, OAuth: &authz.OAuthConfig{Resource: "https://resource.test/mcp", Clients: map[string]authz.OAuthClient{
		"first":  {RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "exact"},
		"second": {RedirectURIs: []string{"http://127.0.0.1:49153/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "exact"},
	}}}
	service, err := authz.New(backend, config)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := service.Register(t.Context(), "mcp_transport", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.Register(t.Context(), "mcp_other", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	type authorization struct {
		tokens                 authz.OAuthTokens
		code, client, callback string
		account                model.ID
	}
	issue := func(account model.ID, client string) authorization {
		t.Helper()
		callback := config.OAuth.Clients[client].RedirectURIs[0]
		code, err := service.IssueAuthorizationCode(t.Context(), account, authz.AuthorizationRequest{ClientID: client, RedirectURI: callback, Resource: config.OAuth.Resource, ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := service.ExchangeAuthorizationCode(t.Context(), code, client, callback, config.OAuth.Resource, "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
		if err != nil {
			t.Fatal(err)
		}
		return authorization{tokens, code, client, callback, account}
	}
	first, second, foreign := issue(owner, "first"), issue(owner, "second"), issue(other, "first")
	var servers []*httptest.Server
	for range 2 {
		instance, err := authz.New(backend, config)
		if err != nil {
			t.Fatal(err)
		}
		public, err := authz.NewHandler(instance)
		if err != nil {
			t.Fatal(err)
		}
		handlers := map[string]mcp.ToolHandler{}
		for _, name := range []string{"agent_register", "graph_list", "graph_create", "graph_get", "graph_update", "node_create", "node_get", "node_update", "node_discard", "node_restore", "context_flow_get", "relation_list", "relation_confirm", "relation_discard"} {
			handlers[name] = func(ctx context.Context, principal mcp.Principal, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				value := map[string]any{"account": principal.AccountID.String(), "client": principal.ClientID}
				if request.Params.Name == "agent_register" {
					var input struct {
						RegistrationKey string `json:"registration_key"`
					}
					if err := json.Unmarshal(request.Params.Arguments, &input); err != nil {
						return nil, err
					}
					id, err := mcp.RegisterAgent(ctx, backend, principal, input.RegistrationKey)
					if err != nil {
						return nil, err
					}
					value = map[string]any{"agent_id": id.String()}
				}
				data, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				return &sdk.CallToolResult{StructuredContent: value, Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}}, nil
			}
		}
		handler, err := newMCPHandler(public, []string{"https://allowed.test"}, handlers)
		if err != nil {
			t.Fatal(err)
		}
		app := newApplication(backend, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true})
		app.mcp = handler
		server := httptest.NewTLSServer(app.handler())
		t.Cleanup(server.Close)
		server.Client().Timeout = 5 * time.Second
		servers = append(servers, server)
	}
	exchange := func(server *httptest.Server, token, method, name string, arguments map[string]any, origin string) (int, http.Header, map[string]any) {
		t.Helper()
		params := map[string]any{"_meta": map[string]any{sdk.MetaKeyProtocolVersion: "2026-07-28", sdk.MetaKeyClientCapabilities: map[string]any{}, sdk.MetaKeyClientInfo: map[string]any{"name": "forged-client", "version": "1"}}}
		if method == "tools/call" {
			params["name"], params["arguments"] = name, arguments
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "mcp-02", "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		// 공개 resource Host를 loopback 수신기에 전달하는 프록시 배치도 대조한다.
		request.Host = "resource.test"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("MCP-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", method)
		if name != "" {
			request.Header.Set("Mcp-Name", name)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if len(data) > 0 && json.Unmarshal(data, &value) != nil {
			t.Fatal("JSON 응답 형식")
		}
		if response.Header.Get("Mcp-Session-Id") != "" || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("캐시·세션 경계")
		}
		for _, secret := range []string{first.tokens.AccessToken, first.tokens.RefreshToken, first.code} {
			if strings.Contains(string(data), secret) {
				t.Fatal("응답 자격 비노출 위반")
			}
		}
		return response.StatusCode, response.Header, value
	}
	// 발견 없이 첫 호출하고 다른 인스턴스·계정·client로 이어도 요청 주체가 바뀐다.
	for _, server := range servers {
		for _, credential := range []authorization{first, second, foreign} {
			status, _, value := exchange(server, credential.tokens.AccessToken, "tools/call", "graph_list", map[string]any{}, "")
			if status != 200 {
				t.Fatal(status, value)
			}
			result := value["result"].(map[string]any)
			data := result["structuredContent"].(map[string]any)
			if result["resultType"] != "complete" || data["account"] != credential.account.String() || data["client"] != credential.client {
				t.Fatal("Bearer와 도구 신원 결합", result)
			}
		}
	}
	for _, method := range []string{"server/discover", "tools/list"} {
		status, _, value := exchange(servers[0], first.tokens.AccessToken, method, "", nil, "https://allowed.test")
		if status != 200 {
			t.Fatal(status, value)
		}
		result := value["result"].(map[string]any)
		if result["ttlMs"] != float64(0) || result["cacheScope"] != "private" {
			t.Fatal("캐시 경계")
		}
	}
	var registration string
	for _, server := range servers {
		status, _, value := exchange(server, first.tokens.AccessToken, "tools/call", "agent_register", map[string]any{"registration_key": "transport_registration_1"}, "")
		if status != 200 {
			t.Fatal(status, value)
		}
		id := value["result"].(map[string]any)["structuredContent"].(map[string]any)["agent_id"].(string)
		if registration != "" && registration != id {
			t.Fatal("인스턴스별 등록 분리")
		}
		registration = id
	}
	for _, token := range []string{"", "synthetic-invalid-token"} {
		status, header, _ := exchange(servers[0], token, "tools/list", "", nil, "")
		if status != 401 || !strings.Contains(header.Get("WWW-Authenticate"), "resource_metadata=") {
			t.Fatal("OAuth 인증 도전")
		}
	}
	if status, _, _ := exchange(servers[0], first.tokens.AccessToken, "tools/list", "", nil, "https://other.test"); status != 403 {
		t.Fatal("Origin 거부")
	}
	if _, err := service.ExchangeAuthorizationCode(t.Context(), first.code, first.client, first.callback, config.OAuth.Resource, "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); err == nil {
		t.Fatal("코드 재사용 허용")
	}
	for _, server := range servers {
		status, _, _ := exchange(server, first.tokens.AccessToken, "tools/list", "", nil, "")
		if status != 401 {
			t.Fatal("폐기 계열을 다른 인스턴스가 허용")
		}
	}
	if status, _, _ := exchange(servers[0], second.tokens.AccessToken, "tools/list", "", nil, ""); status != 200 {
		t.Fatal("다른 계열 폐기")
	}
	backend.Close()
	if status, header, _ := exchange(servers[0], second.tokens.AccessToken, "tools/list", "", nil, ""); status != 500 || header.Get("WWW-Authenticate") != "" {
		t.Fatal("저장 장애를 재인증으로 변환")
	}
}
