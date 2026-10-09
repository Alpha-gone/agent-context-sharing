package mcp

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
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
				"_meta":     requestMeta("2025-03-26"),
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
			"params": map[string]any{"_meta": requestMeta("2025-03-26")},
		}))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		assertRPCError(t, response, http.StatusBadRequest, -32022)
	})

	t.Run("정의하지 않은 RPC 메서드는 404다", func(t *testing.T) {
		request := mcpRequest(t, "resources/list", map[string]any{})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		assertRPCError(t, response, http.StatusNotFound, -32601)
	})
}

// TestDPoPAuthorizationSchemeIsCaseInsensitive는 RFC 9110대로 인증 스킴의 대소문자를
// 구분하지 않고, 스킴 뒤 토큰 형식은 그대로 검사하는지 확인한다.
func TestDPoPAuthorizationSchemeIsCaseInsensitive(t *testing.T) {
	server := testServer(t, nil)
	for authorization, status := range map[string]int{
		"dpop valid-token":  http.StatusOK,
		"DPOP valid-token":  http.StatusOK,
		"DPoP  valid-token": http.StatusUnauthorized,
		"DPoPvalid-token":   http.StatusUnauthorized,
	} {
		request := mcpRequest(t, "tools/list", map[string]any{})
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("Authorization %q 상태 = %d, want %d", authorization, response.Code, status)
		}
	}
}

func TestToolsListRequiresDPoPAuthenticationAndReturnsAllTools(t *testing.T) {
	server := testServer(t, nil)
	request := mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Del("DPoP")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("토큰 없는 요청 상태 = %d, want 401", response.Code)
	}
	const metadata = `DPoP algs="ES256", resource_metadata="https://service.test/.well-known/oauth-protected-resource", scope="agent-context"`
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
	const invalidProof = `DPoP error="invalid_dpop_proof", algs="ES256", resource_metadata="https://service.test/.well-known/oauth-protected-resource", scope="agent-context"`
	if response.Header().Get("WWW-Authenticate") != invalidProof {
		t.Fatalf("DPoP가 아닌 인증 헤더의 WWW-Authenticate = %q, want %q", response.Header().Get("WWW-Authenticate"), invalidProof)
	}

	// 제시된 토큰의 검증 실패도 401이다. OAuth 2.1이 유효하지 않은 접근 토큰에 그것을
	// 요구하며, 도메인 결과로 내려보내면 응답이 HTTP 수준에서 성공으로 보여 표준
	// 클라이언트가 재인증 흐름을 시작하지 못한다.
	request = mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Set("Authorization", "DPoP invalid-token")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("검증 실패 상태 = %d, want 401", response.Code)
	}
	const invalidToken = `DPoP error="invalid_token", algs="ES256", resource_metadata="https://service.test/.well-known/oauth-protected-resource", scope="agent-context"`
	if response.Header().Get("WWW-Authenticate") != invalidToken {
		t.Fatalf("검증 실패의 WWW-Authenticate = %q, want %q", response.Header().Get("WWW-Authenticate"), invalidToken)
	}
	if strings.Contains(response.Body.String(), "unauthenticated") {
		t.Fatalf("인증 실패가 도메인 오류로도 실렸다: %s", response.Body.String())
	}

	request = mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Set("Authorization", "DPoP valid-token")
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
	request.Header.Set("Authorization", "DPoP valid-token")
	request.Header.Set("Mcp-Name", "other_tool")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertRPCError(t, response, http.StatusBadRequest, -32020)

	request = mcpRequest(t, "tools/call", map[string]any{"name": "graph_list", "arguments": map[string]any{}})
	request.Header.Set("Authorization", "DPoP valid-token")
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
	request.Header.Set("Authorization", "DPoP valid-token")
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
	request.Header.Set("Authorization", "DPoP valid-token")
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
			request.Header.Set("Authorization", "DPoP valid-token")
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
	request.Header.Set("Authorization", "DPoP valid-token")
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
	request.Header.Set("Authorization", "DPoP valid-token")
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
	request.Header.Set("Authorization", "DPoP valid-token")
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
	request.Header.Set("Authorization", "DPoP valid-token")
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
	var protectedMetadata map[string]any
	if err := json.Unmarshal(protected.Body.Bytes(), &protectedMetadata); err != nil {
		t.Fatalf("보호 리소스 문서 해석: %v", err)
	}
	if protectedMetadata["dpop_bound_access_tokens_required"] != true {
		t.Fatalf("dpop_bound_access_tokens_required = %#v, want true", protectedMetadata["dpop_bound_access_tokens_required"])
	}
	protectedAlgorithms, ok := protectedMetadata["dpop_signing_alg_values_supported"].([]any)
	if !ok || len(protectedAlgorithms) != 1 || protectedAlgorithms[0] != "ES256" {
		t.Fatalf("보호 리소스 DPoP 알고리즘 = %#v, want [ES256]", protectedMetadata["dpop_signing_alg_values_supported"])
	}

	authorization := httptest.NewRecorder()
	server.AuthorizationServerMetadata(authorization, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if authorization.Code != http.StatusOK {
		t.Fatalf("인가 서버 문서 = %d, %s", authorization.Code, authorization.Body.String())
	}
	var metadata map[string]any
	if err := json.Unmarshal(authorization.Body.Bytes(), &metadata); err != nil {
		t.Fatalf("인가 서버 문서 해석: %v", err)
	}
	if metadata["issuer"] != "https://issuer.test" {
		t.Fatalf("issuer = %#v", metadata["issuer"])
	}
	// 생략하면 RFC 8414가 client_secret_basic을 기본값으로 가정하게 둔다. 이 서버는
	// 공개 클라이언트만 받으므로 그 가정이 남으면 클라이언트가 없는 비밀을 찾는다.
	methods, ok := metadata["token_endpoint_auth_methods_supported"].([]any)
	if !ok || len(methods) != 1 || methods[0] != "none" {
		t.Fatalf("token_endpoint_auth_methods_supported = %#v, want [none]", metadata["token_endpoint_auth_methods_supported"])
	}
	// 싣기만 하고 선언하지 않으면 클라이언트가 iss 없는 응답도 정상으로 받아들인다.
	if metadata["authorization_response_iss_parameter_supported"] != true {
		t.Fatalf("authorization_response_iss_parameter_supported = %#v, want true", metadata["authorization_response_iss_parameter_supported"])
	}
	scopes, ok := metadata["scopes_supported"].([]any)
	if !ok || len(scopes) != 1 || scopes[0] != Scope {
		t.Fatalf("scopes_supported = %#v, want [%s]", metadata["scopes_supported"], Scope)
	}
	algorithms, ok := metadata["dpop_signing_alg_values_supported"].([]any)
	if !ok || len(algorithms) != 1 || algorithms[0] != "ES256" {
		t.Fatalf("인가 서버 DPoP 알고리즘 = %#v, want [ES256]", metadata["dpop_signing_alg_values_supported"])
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
	server, err := New(Config{
		ResourceURL:            resource,
		AuthorizationServerURL: issuer,
		AllowedOrigins:         []*url.URL{origin},
		ServerInfo:             Implementation{Name: "agent-context", Version: "0.1.0"},
	}, func(_ context.Context, authentication Authentication) (model.ID, error) {
		switch {
		case authentication.AccessToken == "unavailable-token":
			// 자격 증명이 아니라 검증을 끝내지 못한 경우다. 401로 답하면 안 된다.
			return model.ID{}, errVerificationUnavailable
		case authentication.AccessToken != "valid-token" || authentication.Proof != "valid-proof" || authentication.Method != http.MethodPost || authentication.Target != resource.String():
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
	params["_meta"] = requestMeta(ProtocolVersion)
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	request := httptest.NewRequest(http.MethodPost, "/mcp", jsonBody(t, payload))
	request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	request.Header.Set("Mcp-Method", method)
	request.Header.Set("DPoP", "valid-proof")
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

// errUnauthorized는 제시한 자격 증명 자체가 유효하지 않은 경우다. 검증 함수를 채우는
// 쪽이 그렇듯 ErrInvalidToken으로 감싸 전송 계층이 401로 옮길 수 있게 한다.
var errUnauthorized = fmt.Errorf("%w: 서명 불일치", ErrInvalidToken)

// errVerificationUnavailable은 자격 증명의 문제가 아니라 검증을 끝내지 못한 경우다.
var errVerificationUnavailable = errors.New("폐기 목록 조회 실패")

// TestRequestIDMustBeStringOrNumber는 JSON-RPC 요청 식별자를 문자열이나 숫자로 제한하는지
// 확인한다. 받아 주면 결과에 "id":null을 실어 클라이언트가 응답을 요청과 잇지 못한다.
func TestRequestIDMustBeStringOrNumber(t *testing.T) {
	called := false
	server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
		called = true
		return ToolResult{}, nil
	})
	verified := false
	verify := server.verify
	server.verify = func(ctx context.Context, authentication Authentication) (model.ID, error) {
		verified = true
		return verify(ctx, authentication)
	}
	for _, id := range []string{"missing", "null", "true", "false", `{}`, `[]`, `[1]`} {
		t.Run(id, func(t *testing.T) {
			payload := map[string]any{
				"jsonrpc": "2.0", "method": "tools/call",
				"params": map[string]any{"name": "graph_list", "arguments": map[string]any{}, "_meta": requestMeta(ProtocolVersion)},
			}
			if id != "missing" {
				payload["id"] = jsontext.Value(id)
			}
			request := httptest.NewRequest(http.MethodPost, "/mcp", jsonBody(t, payload))
			request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
			request.Header.Set("Mcp-Method", "tools/call")
			request.Header.Set("Mcp-Name", "graph_list")
			request.Header.Set("Authorization", "DPoP valid-token")
			request.Header.Set("DPoP", "valid-proof")
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s 응답 상태 = %d, want %d", id, recorder.Code, http.StatusBadRequest)
			}
			if !strings.Contains(recorder.Body.String(), "-32600") {
				t.Fatalf("%s 응답 본문 = %s", id, recorder.Body.String())
			}
			var result struct {
				ID any `json:"id"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.ID != nil {
				t.Fatalf("%s 오류 식별자가 null이 아니다: %v", id, err)
			}
		})
	}
	if called || verified {
		t.Fatal("식별자가 올바르지 않은 요청이 인증 또는 처리기까지 갔다")
	}

	// 문자열과 숫자 식별자는 통과한다.
	for _, id := range []string{`"call-1"`, `""`, `7`, `-1`, `1.5`, `1e4`, `9007199254740993`} {
		payload := map[string]any{
			"jsonrpc": "2.0", "id": jsontext.Value(id), "method": "tools/list",
			"params": map[string]any{"_meta": requestMeta(ProtocolVersion)},
		}
		request := httptest.NewRequest(http.MethodPost, "/mcp", jsonBody(t, payload))
		request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
		request.Header.Set("Mcp-Method", "tools/list")
		request.Header.Set("Authorization", "DPoP valid-token")
		request.Header.Set("DPoP", "valid-proof")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("식별자 %v를 거부했다: %s", id, recorder.Body.String())
		}
		var result struct {
			ID jsontext.Value `json:"id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || string(result.ID) != id {
			t.Fatalf("식별자 원문 = %s, want %s; 오류 %v", result.ID, id, err)
		}
	}
}

// requestMeta는 규약이 요청마다 요구하는 필수 메타데이터를 만든다. 기능 선언은 빈
// 객체여도 유효하며, 이 서비스의 연산은 어느 클라이언트 기능도 요구하지 않는다.
func requestMeta(protocolVersion string) map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    protocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
}

// TestParseAndMetadataFailuresUseDistinctCodes는 본문 해석 실패, 필수 메타데이터 누락과
// 헤더 불일치가 서로 다른 코드로 갈리는지 확인한다.
//
// 세 코드가 각각 다른 수정을 가리킨다. 해석 불가를 -32020으로 답하면 본문을 읽지도
// 못한 상태에서 클라이언트가 헤더를 고치려 든다.
func TestParseAndMetadataFailuresUseDistinctCodes(t *testing.T) {
	server := testServer(t, nil)

	t.Run("손상된 JSON은 Parse error다", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,`))
		request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
		request.Header.Set("Mcp-Method", "tools/list")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		assertRPCError(t, response, http.StatusBadRequest, -32700)
	})

	for name, meta := range map[string]map[string]any{
		"protocolVersion 누락":    {"io.modelcontextprotocol/clientCapabilities": map[string]any{}},
		"clientCapabilities 누락": {"io.modelcontextprotocol/protocolVersion": ProtocolVersion},
		"_meta 자체가 없음":          nil,
	} {
		t.Run(name+"은 Invalid params다", func(t *testing.T) {
			params := map[string]any{}
			if meta != nil {
				params["_meta"] = meta
			}
			payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": params}
			request := httptest.NewRequest(http.MethodPost, "/mcp", jsonBody(t, payload))
			request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
			request.Header.Set("Mcp-Method", "tools/list")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			// protocolVersion이 빠지면 헤더 대조가 먼저 걸리므로 두 코드 모두 허용한다.
			if name == "protocolVersion 누락" || name == "_meta 자체가 없음" {
				assertRPCError(t, response, http.StatusBadRequest, -32020)
				return
			}
			assertRPCError(t, response, http.StatusBadRequest, -32602)
		})
	}
}

func TestInvalidRequestShapesAreNotParseErrors(t *testing.T) {
	server := testServer(t, nil)
	server.verify = func(context.Context, Authentication) (model.ID, error) {
		t.Fatal("잘못된 요청 형식이 인증으로 진행되었다")
		return model.ID{}, nil
	}
	for name, body := range map[string]string{
		"배치":          `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
		"null":        `null`,
		"빈 객체":        `{}`,
		"잘못된 판":       `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`,
		"null method": `{"jsonrpc":"2.0","id":1,"method":null}`,
		"배열 params":   `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`,
		"숫자 method":   `{"jsonrpc":"2.0","id":1,"method":42}`,
		"문자열 params":  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":"잘못된 입력"}`,
		"중복 필드":       `{"jsonrpc":"2.0","id":1,"id":2,"method":"tools/list"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
			request.Header.Set("Mcp-Method", "tools/list")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			assertRPCError(t, response, http.StatusBadRequest, -32600)
			if !strings.Contains(response.Body.String(), `"id":null`) {
				t.Fatalf("잘못된 요청의 ID가 노출되었다: %s", response.Body.String())
			}
		})
	}
}

// TestUnsupportedVersionUsesReservedCode는 미지원 버전이 규약이 확정한 코드로 나가는지
// 확인한다. -32000~-32019는 legacy 구간이라 표준 클라이언트가 재협상하지 않는다.
func TestUnsupportedVersionUsesReservedCode(t *testing.T) {
	server := testServer(t, nil)
	request := mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Set("MCP-Protocol-Version", "2025-11-25")
	request.Body = io.NopCloser(jsonBody(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
		"params": map[string]any{"_meta": requestMeta("2025-11-25")},
	}))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertRPCError(t, response, http.StatusBadRequest, -32022)

	var payload struct {
		Error struct {
			Data struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("오류 응답 해석: %v", err)
	}
	if len(payload.Error.Data.Supported) != 1 || payload.Error.Data.Supported[0] != ProtocolVersion {
		t.Fatalf("지원 목록 = %#v, want [%s]", payload.Error.Data.Supported, ProtocolVersion)
	}
	if payload.Error.Data.Requested != "2025-11-25" {
		t.Fatalf("요청 버전 = %q, want 2025-11-25", payload.Error.Data.Requested)
	}
}

// TestServerDiscoverAnnouncesVersionAndCapabilities는 규약이 필수로 요구한
// `server/discover`가 지원 revision과 기능을 알리는지 확인한다.
//
// 인증보다 앞에 두는 것도 함께 본다. 클라이언트가 인가를 받기 전에 버전을 확인하는 것이
// 이 메서드의 목적이며, 담는 값에 계정이나 컨텍스트가 없다.
func TestServerDiscoverAnnouncesVersionAndCapabilities(t *testing.T) {
	server := testServer(t, nil)
	request := mcpRequest(t, "server/discover", map[string]any{})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("server/discover 상태 = %d, want 200: %s", response.Code, response.Body.String())
	}
	result := decodeResult(t, response)
	versions, ok := result["supportedVersions"].([]any)
	if !ok || len(versions) != 1 || versions[0] != ProtocolVersion {
		t.Fatalf("supportedVersions = %#v, want [%s]", result["supportedVersions"], ProtocolVersion)
	}
	capabilities, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities = %#v", result["capabilities"])
	}
	if _, found := capabilities["tools"]; !found {
		t.Fatalf("tools 기능을 알리지 않았다: %#v", capabilities)
	}
	extensions, ok := capabilities["extensions"].(map[string]any)
	if !ok {
		t.Fatalf("extensions = %#v", capabilities["extensions"])
	}
	idempotency, ok := extensions[writeIdempotencyExtension].(map[string]any)
	if !ok || idempotency["retentionMs"] != float64(writeIdempotencyRetention.Milliseconds()) {
		t.Fatalf("쓰기 멱등성 확장 = %#v", extensions[writeIdempotencyExtension])
	}
	assertCacheHints(t, result, 3600000)
}

func TestNegotiatedWriteIdempotencyForwardsKeyHeader(t *testing.T) {
	key := newTestID(t)
	arguments := map[string]any{"name": "그래프"}
	fingerprint, err := requestFingerprint("graph_create", arguments)
	if err != nil {
		t.Fatalf("요청 지문: %v", err)
	}
	for _, header := range []string{key, "bad-key"} {
		called := false
		server := testServer(t, func(ctx context.Context, _ model.ID, name string, arguments map[string]any) (ToolResult, error) {
			called = true
			if name != "graph_create" || arguments["name"] != "그래프" {
				t.Fatalf("호출 = %s %#v", name, arguments)
			}
			request, ok := IdempotencyFromContext(ctx)
			if !ok || !slices.Equal(request.KeyHeader, []string{"\"" + header + "\""}) || request.Fingerprint != fingerprint {
				t.Fatalf("멱등성 요청 = %#v, found = %t", request, ok)
			}
			return result(map[string]any{"ok": true}), nil
		})
		request := idempotencyToolCallRequest(t, "graph_create", arguments, header)
		request.Header.Set("Authorization", "DPoP valid-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		// 키 형식 검증은 권한 확인 뒤의 7단계이므로 전송 계층은 형식과 무관하게 넘긴다.
		if !called || response.Code != http.StatusOK {
			t.Fatalf("키 %q: 상태 = %d, 호출됨 = %t: %s", header, response.Code, called, response.Body.String())
		}
	}
}

func TestParseIdempotencyKey(t *testing.T) {
	key := newTestID(t)
	valid, ok := parseIdempotencyKey([]string{"\"" + key + "\""})
	if !ok || valid.String() != key {
		t.Fatalf("올바른 키 = %v, ok = %t", valid, ok)
	}
	for name, values := range map[string][]string{
		"없음":     nil,
		"따옴표 없음": {key},
		"하이픈 없음": {"\"" + strings.ReplaceAll(key, "-", "") + "\""},
		"UUIDv4": {"\"0b4a6b56-8c1d-4c43-9f2e-6f1d2a3b4c5d\""},
		"형식 오류":  {"\"bad-key\""},
		"헤더 두 개": {"\"" + key + "\"", "\"" + key + "\""},
	} {
		if _, ok := parseIdempotencyKey(values); ok {
			t.Fatalf("%s 키 %#v를 받아들였다", name, values)
		}
	}
}

// TestResultsCarryEnvelope는 모든 결과가 규약의 응답 외피를 갖는지 확인한다.
//
// `resultType`이 빠지면 클라이언트가 해석을 시작하지 못하거나 예전 판의 응답으로
// 취급한다. 결과마다 붙이므로 한 연산이라도 빠지면 안 된다.
func TestResultsCarryEnvelope(t *testing.T) {
	server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
		return ToolResult{Content: []Content{{Type: "text", Text: "ok"}}}, nil
	})
	requests := map[string]*http.Request{
		"server/discover": mcpRequest(t, "server/discover", map[string]any{}),
		"tools/list":      mcpRequest(t, "tools/list", map[string]any{}),
		"tools/call":      toolCallRequest(t, "graph_list", map[string]any{}),
	}
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			request.Header.Set("Authorization", "DPoP valid-token")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("상태 = %d: %s", response.Code, response.Body.String())
			}
			result := decodeResult(t, response)
			if result["resultType"] != "complete" {
				t.Fatalf("resultType = %#v, want complete", result["resultType"])
			}
			meta, ok := result["_meta"].(map[string]any)
			if !ok {
				t.Fatalf("_meta = %#v", result["_meta"])
			}
			info, ok := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
			if !ok || info["name"] != "agent-context" || info["version"] != "0.1.0" {
				t.Fatalf("serverInfo = %#v", meta["io.modelcontextprotocol/serverInfo"])
			}
		})
	}
}

// TestToolsListCarriesCacheHints는 목록 결과가 규약이 요구한 캐시 힌트를 싣는지 확인한다.
// cacheScope가 public인 이유는 도구 목록이 계정과 무관하기 때문이다.
func TestToolsListCarriesCacheHints(t *testing.T) {
	server := testServer(t, nil)
	request := mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Set("Authorization", "DPoP valid-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertCacheHints(t, decodeResult(t, response), 300000)
}

// TestVerificationFailureSeparatesCredentialFromOutage는 자격 증명 실패와 내부 장애가
// 서로 다른 상태 코드로 갈리는지 확인한다.
//
// 장애를 401로 숨기면 클라이언트가 재인증하고 돌아와도 같은 자리에서 다시 막히고,
// 실제 장애가 재인증 문제로 보여 관측과 장애 대응이 늦는다.
func TestVerificationFailureSeparatesCredentialFromOutage(t *testing.T) {
	server := testServer(t, nil)
	request := mcpRequest(t, "tools/list", map[string]any{})
	request.Header.Set("Authorization", "DPoP unavailable-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("내부 장애 상태 = %d, want 500: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("내부 장애에 재인증 도전을 보냈다")
	}
	assertRPCError(t, response, http.StatusInternalServerError, -32603)
	// 실패한 조회의 원인은 응답에 담지 않는다.
	if strings.Contains(response.Body.String(), "폐기 목록") {
		t.Fatalf("내부 원인이 응답에 실렸다: %s", response.Body.String())
	}
}

// decodeResult는 응답의 result 객체를 꺼낸다.
func decodeResult(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("결과 해석: %v; 본문 = %s", err, response.Body.String())
	}
	return payload.Result
}

// assertCacheHints는 규약이 요구한 캐시 힌트를 확인한다.
func assertCacheHints(t *testing.T, result map[string]any, wantTTL float64) {
	t.Helper()
	ttl, ok := result["ttlMs"].(float64)
	if !ok || ttl != wantTTL {
		t.Fatalf("ttlMs = %#v, want %v", result["ttlMs"], wantTTL)
	}
	if result["cacheScope"] != "public" {
		t.Fatalf("cacheScope = %#v, want public", result["cacheScope"])
	}
}

// toolCallRequest는 헤더와 본문이 맞는 tools/call 요청을 만든다.
func toolCallRequest(t *testing.T, name string, arguments map[string]any) *http.Request {
	t.Helper()
	request := mcpRequest(t, "tools/call", map[string]any{"name": name, "arguments": arguments})
	request.Header.Set("Mcp-Name", name)
	return request
}

func idempotencyToolCallRequest(t *testing.T, name string, arguments map[string]any, key string) *http.Request {
	t.Helper()
	meta := requestMeta(ProtocolVersion)
	meta["io.modelcontextprotocol/clientCapabilities"] = map[string]any{
		"extensions": map[string]any{writeIdempotencyExtension: map[string]any{}},
	}
	payload := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments, "_meta": meta},
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", jsonBody(t, payload))
	request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", name)
	request.Header.Set("DPoP", "valid-proof")
	request.Header.Set("Idempotency-Key", "\""+key+"\"")
	return request
}
