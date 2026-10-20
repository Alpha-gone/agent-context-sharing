package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCP08BearerAndTransportErrorsStaySeparate(t *testing.T) {
	for _, mode := range []string{"valid", "absent", "invalid", "scope", "expired", "key outage", "store outage", "malformed authenticated", "malformed unauthenticated"} {
		t.Run(mode, func(t *testing.T) {
			service, claims, sign := bearerFixture(t)
			status := http.StatusUnauthorized
			switch mode {
			case "valid":
				status = 200
			case "scope":
				claims["scope"] = "other"
				status = 403
			case "expired":
				claims["exp"] = service.now().Unix()
			case "key outage":
				service.store = &failingStore{memoryStore: service.store.(*memoryStore), signingKeyFails: true}
				status = 500
			case "store outage":
				service.config.AccessTokens = accessStoreFunc(func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error) {
					return false, errors.New("synthetic-storage-secret")
				})
				status = 500
			case "malformed authenticated":
				status = 400
			}
			raw := sign(claims)
			authHandler, err := NewHandler(service)
			if err != nil {
				t.Fatal(err)
			}
			handlers := map[string]mcp.ToolHandler{}
			called := false
			for _, name := range []string{"agent_register", "graph_list", "graph_create", "graph_get", "graph_update", "node_create", "node_get", "node_update", "node_discard", "node_restore", "context_flow_get", "relation_list", "relation_confirm", "relation_discard"} {
				handlers[name] = func(context.Context, mcp.Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					called = true
					return &sdk.CallToolResult{StructuredContent: map[string]any{"ok": true}, Content: []sdk.Content{&sdk.TextContent{Text: `{"ok":true}`}}}, nil
				}
			}
			handler, err := mcp.NewHandler(mcp.HTTPConfig{AllowedOrigins: []string{"https://allowed.test"}, Handlers: handlers, Protect: func(next mcp.AuthenticatedHandler) http.Handler {
				return authHandler.ProtectBearer(func(w http.ResponseWriter, r *http.Request, p BearerPrincipal) {
					next(w, r, mcp.Principal{AccountID: p.AccountID, ClientID: p.ClientID})
				})
			}})
			if err != nil {
				t.Fatal(err)
			}
			body := `{"jsonrpc":"2.0","id":"auth-errors","method":"tools/call","params":{"name":"graph_list","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
			if strings.HasPrefix(mode, "malformed") {
				body = `{"private":`
			}
			request := httptest.NewRequest("POST", "https://resource.test/mcp", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("MCP-Protocol-Version", "2026-07-28")
			request.Header.Set("Mcp-Method", "tools/call")
			request.Header.Set("Mcp-Name", "graph_list")
			if mode != "absent" && mode != "malformed unauthenticated" {
				value := raw
				if mode == "invalid" {
					value = "invalid-token"
				}
				request.Header.Set("Authorization", "Bearer "+value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != status || called != (mode == "valid") || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("응답 계층: %d called=%t %s", response.Code, called, response.Body.String())
			}
			challenge := response.Header().Get("WWW-Authenticate")
			if (status == 401 || status == 403) != (challenge != "") {
				t.Fatal("내부/외피 장애가 재인가 신호로 변환")
			}
			if status == 500 && !strings.Contains(response.Body.String(), `"error":"server_error"`) {
				t.Fatal("인증 내부 장애")
			}
			if mode == "malformed authenticated" && !strings.Contains(response.Body.String(), `"code":-32700`) {
				t.Fatal("외피 오류가 도메인 오류로 변환")
			}
			for _, secret := range []string{raw, "synthetic-storage-secret", "synthetic-key-secret", "private"} {
				if strings.Contains(response.Body.String()+challenge, secret) {
					t.Fatal("원문 노출")
				}
			}
		})
	}
}
