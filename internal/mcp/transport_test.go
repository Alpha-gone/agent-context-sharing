package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestTransportValidationOrder(t *testing.T) {
	server := testServer(t, nil)

	t.Run("허용되지 않은 Origin이 먼저 거부된다", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		request.Header.Set("Origin", "https://attacker.test")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("상태 = %d, want 403", response.Code)
		}
	})

	t.Run("POST 외 메서드는 405다", func(t *testing.T) {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/mcp", nil))
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("메서드 거부 응답 = %d, Allow = %q", response.Code, response.Header().Get("Allow"))
		}
	})

	t.Run("필수 헤더가 없으면 HeaderMismatch다", func(t *testing.T) {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1}`)))
		assertRPCError(t, response, http.StatusBadRequest, -32020)
	})

	t.Run("tools/call의 Mcp-Name은 protocol revision보다 먼저 확인한다", func(t *testing.T) {
		request := mcpRequest(t, "tools/call", map[string]any{"name": "graph_list", "arguments": map[string]any{}})
		request.Header.Set("MCP-Protocol-Version", "2025-03-26")
		request.Body = io.NopCloser(jsonBody(t, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{
				"name":      "graph_list",
				"arguments": map[string]any{},
				"_meta":     map[string]any{"io.modelcontextprotocol/protocolVersion": "2025-03-26"},
			},
		}))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		assertRPCError(t, response, http.StatusBadRequest, -32020)
	})

	t.Run("본문과 헤더 메서드가 다르면 HeaderMismatch다", func(t *testing.T) {
		request := mcpRequest(t, "tools/list", map[string]any{})
		request.Header.Set("Mcp-Method", "tools/call")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		assertRPCError(t, response, http.StatusBadRequest, -32020)
	})

	t.Run("지원하지 않는 protocol version은 목록을 돌려준다", func(t *testing.T) {
		request := mcpRequest(t, "tools/list", map[string]any{})
		request.Header.Set("MCP-Protocol-Version", "2025-03-26")
		request.Body = io.NopCloser(jsonBody(t, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/list",
			"params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2025-03-26"}},
		}))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		assertRPCError(t, response, http.StatusBadRequest, -32019)
	})

	t.Run("정의하지 않은 RPC 메서드는 404다", func(t *testing.T) {
		request := mcpRequest(t, "resources/list", map[string]any{})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		assertRPCError(t, response, http.StatusNotFound, -32601)
	})
}

func TestToolsListRequiresBearerTokenAndReturnsAllTools(t *testing.T) {
	server := testServer(t, nil)
	request := mcpRequest(t, "tools/list", map[string]any{})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("토큰 없는 요청 상태 = %d, want 401", response.Code)
	}
	const metadata = `Bearer resource_metadata="https://service.test/.well-known/oauth-protected-resource"`
	if response.Header().Get("WWW-Authenticate") != metadata {
		t.Fatalf("WWW-Authenticate = %q, want %q", response.Header().Get("WWW-Authenticate"), metadata)
	}

	request = mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Set("Authorization", "Basic abc")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("Bearer가 아닌 인증 헤더 상태 = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if response.Header().Get("WWW-Authenticate") != metadata {
		t.Fatalf("Bearer가 아닌 인증 헤더의 WWW-Authenticate = %q, want %q", response.Header().Get("WWW-Authenticate"), metadata)
	}

	request = mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Set("Authorization", "Bearer invalid-token")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("검증 실패 상태 = %d, want 200", response.Code)
	}
	assertDomainCode(t, response, "unauthenticated")

	request = mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Set("Authorization", "Bearer valid-token")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("tools/list 상태 = %d, want 200", response.Code)
	}
	var payload struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("tools/list 응답 해석: %v", err)
	}
	if _, found := payload.Result["structuredContent"]; found {
		t.Fatalf("tools/list 결과가 tools/call 형식으로 감싸졌다: %#v", payload.Result)
	}
	tools, ok := payload.Result["tools"].([]any)
	if !ok || len(tools) != 13 {
		t.Fatalf("도구 목록 = %#v, want 13개", payload.Result["tools"])
	}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok || tool["description"] == "" {
			t.Fatalf("도구 정의가 올바르지 않다: %#v", item)
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" || schema["properties"] == nil {
			t.Fatalf("입력 스키마가 올바르지 않다: %#v", item)
		}
	}
}

func TestToolsCallValidatesMcpNameHeader(t *testing.T) {
	server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
		return ToolResult{StructuredContent: map[string]any{"ok": true}}, nil
	})
	request := mcpRequest(t, "tools/call", map[string]any{"name": "graph_list", "arguments": map[string]any{}})
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Mcp-Name", "other_tool")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertRPCError(t, response, http.StatusBadRequest, -32020)

	request = mcpRequest(t, "tools/call", map[string]any{"name": "graph_list", "arguments": map[string]any{}})
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Mcp-Name", "graph_list")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("정상 tools/call 상태 = %d, want 200", response.Code)
	}
}

func TestToolsCallValidatesArgumentsBeforeDispatch(t *testing.T) {
	called := false
	server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
		called = true
		return ToolResult{}, nil
	})
	request := mcpRequest(t, "tools/call", map[string]any{"name": "node_create", "arguments": map[string]any{"graph_id": "not-a-uuid", "layer": "source", "body": "본문"}})
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Mcp-Name", "node_create")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || called {
		t.Fatalf("잘못된 인자 처리 상태 = %d, 호출됨 = %t", response.Code, called)
	}
	assertDomainCode(t, response, "invalid_argument")
}

func TestToolsCallRejectsInvalidDateTimeBeforeDispatch(t *testing.T) {
	graphID := newTestID(t)
	agentID := newTestID(t)
	called := false
	server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
		called = true
		return ToolResult{}, nil
	})
	request := mcpRequest(t, "tools/call", map[string]any{"name": "node_create", "arguments": map[string]any{
		"graph_id":         graphID,
		"created_by_agent": agentID,
		"layer":            "source",
		"body":             "본문",
		"occurred_at":      "banana",
	}})
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Mcp-Name", "node_create")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if called {
		t.Fatal("invalid date-time request reached the domain call")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	assertDomainCode(t, response, "invalid_argument")
	assertDomainField(t, response, "field", "occurred_at")
}

func TestValidateToolCallRejectsInvalidDateTimes(t *testing.T) {
	graphID := newTestID(t)
	contextID := newTestID(t)
	agentID := newTestID(t)
	nodeCreateArguments := func(field string) map[string]any {
		return map[string]any{
			"graph_id":         graphID,
			"created_by_agent": agentID,
			"layer":            "source",
			"body":             "본문",
			field:              "banana",
		}
	}
	nodeUpdateArguments := func(field string) map[string]any {
		return map[string]any{
			"graph_id":         graphID,
			"context_id":       contextID,
			"expected_version": float64(1),
			"created_by_agent": agentID,
			field:              "banana",
		}
	}

	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
		field     string
	}{
		{name: "occurred_at", tool: "node_create", arguments: nodeCreateArguments("occurred_at"), field: "occurred_at"},
		{name: "valid_from", tool: "node_update", arguments: nodeUpdateArguments("valid_from"), field: "valid_from"},
		{name: "valid_to", tool: "node_update", arguments: nodeUpdateArguments("valid_to"), field: "valid_to"},
		{name: "start", tool: "node_update", arguments: nodeUpdateArguments("start"), field: "start"},
		{name: "end", tool: "node_update", arguments: nodeUpdateArguments("end"), field: "end"},
		{
			name: "as_of",
			tool: "context_flow_get",
			arguments: map[string]any{
				"graph_id":     graphID,
				"work_context": "task-123",
				"as_of":        "banana",
			},
			field: "as_of",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateToolCall(test.tool, test.arguments)
			if err == nil {
				t.Fatal("validateToolCall returned nil error")
			}
			if err.Field != test.field {
				t.Fatalf("error field = %q, want %q", err.Field, test.field)
			}
		})
	}
}

func TestDateTimeStringAcceptsRFC3339(t *testing.T) {
	if !dateTimeString("2026-09-12T14:30:45.123456789+09:00") {
		t.Fatal("RFC 3339 date-time was rejected")
	}
}

func TestValidateToolCallAcceptsLifecycleAndRelationActors(t *testing.T) {
	graphID, contextID, agentID := newTestID(t), newTestID(t), newTestID(t)
	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{
			name: "파생 대체", tool: "node_create", arguments: map[string]any{
				"graph_id": graphID, "created_by_agent": agentID, "layer": "derived", "body": "대체 파생",
				"derivation_kind": "proposition", "evidence_state": "observation", "derived_from": []any{contextID}, "supersedes_context_id": contextID,
			},
		},
		{
			name: "유지 판단", tool: "node_update", arguments: map[string]any{
				"graph_id": graphID, "context_id": contextID, "expected_version": float64(1), "created_by_agent": agentID, "management_action": "keep",
			},
		},
		{
			name: "관계 확정", tool: "relation_confirm", arguments: map[string]any{
				"graph_id": graphID, "relation_type": "precedes", "from_context_id": contextID, "to_context_id": newTestID(t), "created_by_agent": agentID,
			},
		},
		{
			name: "관계 폐기", tool: "relation_discard", arguments: map[string]any{
				"graph_id": graphID, "relation_id": contextID, "created_by_agent": agentID,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateToolCall(test.tool, test.arguments); err != nil {
				t.Fatalf("유효한 입력을 거부했다: %s %v", err.Field, err)
			}
		})
	}
}

func TestValidateToolCallRequiresRelationActor(t *testing.T) {
	err := validateToolCall("relation_confirm", map[string]any{
		"graph_id": newTestID(t), "relation_type": "precedes", "from_context_id": newTestID(t), "to_context_id": newTestID(t),
	})
	if err == nil || err.Field != "created_by_agent" {
		t.Fatalf("관계 실행 에이전트 누락 결과 = %#v", err)
	}
}

func TestToolsCallRejectsWebOnlyToolBeforeArgumentValidation(t *testing.T) {
	for _, name := range []string{"graph_delete", "graph_grant", "team_create"} {
		t.Run(name, func(t *testing.T) {
			called := false
			server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
				called = true
				return ToolResult{}, nil
			})
			request := mcpRequest(t, "tools/call", map[string]any{"name": name, "arguments": map[string]any{"unexpected": true}})
			request.Header.Set("Authorization", "Bearer valid-token")
			request.Header.Set("Mcp-Name", name)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK || called {
				t.Fatalf("웹 전용 도구 처리 상태 = %d, 호출됨 = %t", response.Code, called)
			}
			assertDomainCode(t, response, "not_supported")
			assertDomainField(t, response, "alternative_channel", "web")
		})
	}
}

func TestToolsCallDoesNotInferWebOnlyToolFromNamePattern(t *testing.T) {
	called := false
	server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
		called = true
		return ToolResult{}, nil
	})
	request := mcpRequest(t, "tools/call", map[string]any{"name": "graph_permission_report", "arguments": map[string]any{}})
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Mcp-Name", "graph_permission_report")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if called {
		t.Fatal("unregistered tool reached the domain call")
	}
	assertDomainCode(t, response, "invalid_argument")
	assertDomainField(t, response, "field", "name")
}

func TestToolsCallWithoutHandlerDoesNotSuggestWebChannel(t *testing.T) {
	server := testServer(t, nil)
	request := mcpRequest(t, "tools/call", map[string]any{"name": "node_create", "arguments": map[string]any{
		"graph_id":         newTestID(t),
		"created_by_agent": newTestID(t),
		"layer":            "source",
		"body":             "본문",
	}})
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Mcp-Name", "node_create")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertDomainCode(t, response, "internal")
	assertDomainFieldAbsent(t, response, "alternative_channel")
}

func TestToolsCallMapsDomainError(t *testing.T) {
	server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
		return ToolResult{}, &Error{Code: "permission_denied", Data: map[string]any{"required_grade": "editor"}}
	})
	request := mcpRequest(t, "tools/call", map[string]any{"name": "graph_list", "arguments": map[string]any{}})
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Mcp-Name", "graph_list")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertDomainCode(t, response, "permission_denied")
}

func TestToolsCallDoesNotExposeUnknownDomainCode(t *testing.T) {
	server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
		return ToolResult{}, &Error{Code: "database_timeout"}
	})
	request := mcpRequest(t, "tools/call", map[string]any{"name": "graph_list", "arguments": map[string]any{}})
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Mcp-Name", "graph_list")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertDomainCode(t, response, "internal")
}

func TestMetadataDocuments(t *testing.T) {
	server := testServer(t, nil)
	protected := httptest.NewRecorder()
	server.ProtectedResourceMetadata(protected, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
	if protected.Code != http.StatusOK || !bytes.Contains(protected.Body.Bytes(), []byte(`"resource":"https://service.test/mcp"`)) {
		t.Fatalf("보호 리소스 문서 = %d, %s", protected.Code, protected.Body.String())
	}

	authorization := httptest.NewRecorder()
	server.AuthorizationServerMetadata(authorization, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if authorization.Code != http.StatusOK || !bytes.Contains(authorization.Body.Bytes(), []byte(`"issuer":"https://issuer.test"`)) {
		t.Fatalf("인가 서버 문서 = %d, %s", authorization.Code, authorization.Body.String())
	}
}

func testServer(t *testing.T, call CallFunc) *Server {
	t.Helper()
	resource := mustURL(t, "https://service.test/mcp")
	issuer := mustURL(t, "https://issuer.test")
	origin := mustURL(t, "https://client.test")
	accountID, err := model.NewID()
	if err != nil {
		t.Fatalf("계정 ID 생성: %v", err)
	}
	server, err := New(Config{ResourceURL: resource, AuthorizationServerURL: issuer, AllowedOrigins: []*url.URL{origin}}, func(_ context.Context, token, audience string) (model.ID, error) {
		if token != "valid-token" || audience != resource.String() {
			return model.ID{}, errUnauthorized
		}
		return accountID, nil
	}, call)
	if err != nil {
		t.Fatalf("MCP 서버 생성: %v", err)
	}
	return server
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL 해석: %v", err)
	}
	return parsed
}

func newTestID(t *testing.T) string {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("ID 생성: %v", err)
	}
	return id.String()
}

func mcpRequest(t *testing.T, method string, params map[string]any) *http.Request {
	t.Helper()
	params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": ProtocolVersion}
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	request := httptest.NewRequest(http.MethodPost, "/mcp", jsonBody(t, payload))
	request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	request.Header.Set("Mcp-Method", method)
	return request
}

func jsonBody(t *testing.T, value any) *bytes.Reader {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("JSON 직렬화: %v", err)
	}
	return bytes.NewReader(encoded)
}

func assertRPCError(t *testing.T, response *httptest.ResponseRecorder, status, code int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("상태 = %d, want %d; 본문 = %s", response.Code, status, response.Body.String())
	}
	var payload struct {
		Error rpcError `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("오류 응답 해석: %v", err)
	}
	if payload.Error.Code != code {
		t.Fatalf("오류 코드 = %d, want %d; 본문 = %s", payload.Error.Code, code, response.Body.String())
	}
}

func assertDomainCode(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	var payload struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("도구 오류 응답 해석: %v", err)
	}
	if payload.Result.StructuredContent["code"] != code {
		t.Fatalf("도메인 오류 코드 = %#v, want %q", payload.Result.StructuredContent, code)
	}
}

func assertDomainField(t *testing.T, response *httptest.ResponseRecorder, field, want string) {
	t.Helper()
	var payload struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("도구 오류 응답 해석: %v", err)
	}
	if payload.Result.StructuredContent[field] != want {
		t.Fatalf("도메인 오류 %s = %#v, want %q", field, payload.Result.StructuredContent, want)
	}
}

func assertDomainFieldAbsent(t *testing.T, response *httptest.ResponseRecorder, field string) {
	t.Helper()
	var payload struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("도구 오류 응답 해석: %v", err)
	}
	if _, found := payload.Result.StructuredContent[field]; found {
		t.Fatalf("도메인 오류에 %s가 포함됐다: %#v", field, payload.Result.StructuredContent)
	}
}

var errUnauthorized = &unauthorizedError{}

type unauthorizedError struct{}

func (*unauthorizedError) Error() string { return "unauthenticated" }
