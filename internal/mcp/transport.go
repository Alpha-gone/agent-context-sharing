// Package mcp는 Streamable HTTP MCP 전송 검증과 tools 표면을 제공한다.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"agent_context_sharing/internal/model"
)

const (
	// ProtocolVersion은 이 서버가 지원하는 유일한 MCP protocol revision이다.
	ProtocolVersion = "2026-07-28"
	// Scope는 모든 MCP 접근 토큰에 쓰는 단일 OAuth scope다.
	Scope = "agent-context"
)

// Config는 MCP 리소스 서버의 고정된 공개 경계를 모은다.
type Config struct {
	ResourceURL            *url.URL
	AuthorizationServerURL *url.URL
	AllowedOrigins         []*url.URL
}

// VerifyFunc는 Bearer 토큰을 검증하고 요청 계정을 돌려준다.
type VerifyFunc func(context.Context, string, string) (model.ID, error)

// CallFunc는 전송 검증을 지난 도구 호출을 처리한다.
type CallFunc func(context.Context, model.ID, string, map[string]any) (ToolResult, error)

// Server는 단일 Streamable HTTP MCP 엔드포인트와 discovery 문서를 제공한다.
type Server struct {
	config Config
	verify VerifyFunc
	call   CallFunc
}

// ToolResult는 tools/call의 성공 또는 도메인 오류 결과다.
type ToolResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitzero"`
	IsError           bool      `json:"isError,omitzero"`
}

// Error는 도메인 처리 결과를 MCP 오류 코드와 안전한 부가 정보로 전달한다.
// 내부 원인은 Error에 넣지 않고 호출 지점에서 감싼다.
type Error struct {
	Code string
	Data map[string]any
}

// Error는 로그에 남길 도메인 오류의 분류를 만든다.
func (error *Error) Error() string { return error.Code }

// Content는 도구 결과의 사람이 읽을 수 있는 내용을 나타낸다.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// New는 고정 공개 경계와 토큰 검증 함수를 확인해 MCP 서버를 만든다.
func New(config Config, verify VerifyFunc, call CallFunc) (*Server, error) {
	if config.ResourceURL == nil || config.AuthorizationServerURL == nil || verify == nil {
		return nil, fmt.Errorf("MCP 서버 구성이 올바르지 않다")
	}
	if !validResourceURL(config.ResourceURL) || !validOrigin(config.AuthorizationServerURL) || len(config.AllowedOrigins) == 0 {
		return nil, fmt.Errorf("MCP 공개 URL 구성이 올바르지 않다")
	}
	for _, origin := range config.AllowedOrigins {
		if !validOrigin(origin) {
			return nil, fmt.Errorf("MCP 공개 URL 구성이 올바르지 않다")
		}
	}
	return &Server{config: config, verify: verify, call: call}, nil
}

// ServeHTTP는 Origin, 메서드, 메타데이터 헤더와 본문을 검증한 뒤 MCP 요청을 처리한다.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !s.allowedOrigin(request.Header.Get("Origin")) {
		writer.WriteHeader(http.StatusForbidden)
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	protocolVersion, method, ok := requestHeaders(request)
	if !ok {
		s.writeRPCError(writer, http.StatusBadRequest, jsontext.Value("null"), -32020, "Header mismatch", nil)
		return
	}

	var message rpcRequest
	if err := json.UnmarshalRead(request.Body, &message); err != nil {
		s.writeRPCError(writer, http.StatusBadRequest, jsontext.Value("null"), -32600, "Invalid request", nil)
		return
	}
	if message.JSONRPC != "2.0" || message.Params.Meta.ProtocolVersion != protocolVersion || message.Method != method {
		s.writeRPCError(writer, http.StatusBadRequest, message.ID, -32020, "Header mismatch", nil)
		return
	}
	if protocolVersion != ProtocolVersion {
		s.writeRPCError(writer, http.StatusBadRequest, message.ID, -32019, "Unsupported protocol version", map[string]any{"supported": []string{ProtocolVersion}})
		return
	}
	if !slices.Contains([]string{"tools/list", "tools/call"}, method) {
		s.writeRPCError(writer, http.StatusNotFound, message.ID, -32601, "Method not found", nil)
		return
	}
	if method == "tools/call" && !matchesToolHeader(request, message.Params.Name) {
		s.writeRPCError(writer, http.StatusBadRequest, message.ID, -32020, "Header mismatch", nil)
		return
	}

	token := bearerToken(request)
	if token == "" {
		s.writeMissingToken(writer)
		return
	}
	accountID, err := s.verify(request.Context(), token, s.config.ResourceURL.String())
	if err != nil {
		s.writeResult(writer, message.ID, domainError("unauthenticated", nil))
		return
	}
	if method == "tools/list" {
		s.writeResult(writer, message.ID, map[string]any{"tools": toolDefinitions()})
		return
	}
	if isWebOnlyTool(message.Params.Name) {
		s.writeResult(writer, message.ID, domainError("not_supported", map[string]any{"alternative_channel": "web"}))
		return
	}
	if err := validateToolCall(message.Params.Name, message.Params.Arguments); err != nil {
		s.writeResult(writer, message.ID, domainError("invalid_argument", map[string]any{"field": err.Field}))
		return
	}
	if s.call == nil {
		s.writeResult(writer, message.ID, domainError("internal", nil))
		return
	}
	result, err := s.call(request.Context(), accountID, message.Params.Name, message.Params.Arguments)
	if err != nil {
		if domain, ok := errors.AsType[*Error](err); ok && validDomainCode(domain.Code) {
			s.writeResult(writer, message.ID, domainError(domain.Code, domain.Data))
			return
		}
		s.writeResult(writer, message.ID, domainError("internal", nil))
		return
	}
	s.writeResult(writer, message.ID, result)
}

// ProtectedResourceMetadata는 RFC 9728 보호 리소스 메타데이터를 반환한다.
func (s *Server) ProtectedResourceMetadata(writer http.ResponseWriter, _ *http.Request) {
	s.writeJSON(writer, http.StatusOK, map[string]any{
		"resource":              s.config.ResourceURL.String(),
		"authorization_servers": []string{s.config.AuthorizationServerURL.String()},
		"scopes_supported":      []string{Scope},
	})
}

// AuthorizationServerMetadata는 RFC 8414 인가 서버 메타데이터를 반환한다.
func (s *Server) AuthorizationServerMetadata(writer http.ResponseWriter, _ *http.Request) {
	base := s.config.AuthorizationServerURL.String()
	s.writeJSON(writer, http.StatusOK, map[string]any{
		"issuer":                           base,
		"authorization_endpoint":           base + "/authorize",
		"token_endpoint":                   base + "/token",
		"jwks_uri":                         base + "/jwks.json",
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code"},
		"code_challenge_methods_supported": []string{"S256"},
	})
}

func (s *Server) allowedOrigin(raw string) bool {
	if raw == "" {
		return true
	}
	origin, err := url.Parse(raw)
	return err == nil && validOrigin(origin) && slices.ContainsFunc(s.config.AllowedOrigins, func(allowed *url.URL) bool {
		return allowed.Scheme == origin.Scheme && strings.EqualFold(allowed.Host, origin.Host)
	})
}

func validOrigin(value *url.URL) bool {
	return value != nil && (value.Scheme == "http" || value.Scheme == "https") && value.Host != "" && value.Path == "" && value.RawQuery == "" && value.Fragment == "" && value.User == nil
}

func validResourceURL(value *url.URL) bool {
	return value != nil && (value.Scheme == "http" || value.Scheme == "https") && value.Host != "" && value.Path == "/mcp" && value.RawQuery == "" && value.Fragment == "" && value.User == nil
}

func requestHeaders(request *http.Request) (string, string, bool) {
	protocolVersion := request.Header.Values("MCP-Protocol-Version")
	method := request.Header.Values("Mcp-Method")
	if len(protocolVersion) != 1 || len(method) != 1 || protocolVersion[0] == "" || method[0] == "" {
		return "", "", false
	}
	if method[0] == "tools/call" {
		name := request.Header.Values("Mcp-Name")
		if len(name) != 1 || name[0] == "" {
			return "", "", false
		}
	}
	return protocolVersion[0], method[0], true
}

func matchesToolHeader(request *http.Request, name string) bool {
	values := request.Header.Values("Mcp-Name")
	if len(values) != 1 || name == "" {
		return false
	}
	value, ok := decodeHeaderValue(values[0])
	return ok && value == name
}

func decodeHeaderValue(value string) (string, bool) {
	encoded, found := strings.CutPrefix(value, "=?base64?")
	if !found {
		return value, true
	}
	encoded, found = strings.CutSuffix(encoded, "?=")
	if !found {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

func bearerToken(request *http.Request) string {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	value, found := strings.CutPrefix(values[0], "Bearer ")
	if !found || value == "" || strings.ContainsAny(value, " \t") {
		return ""
	}
	return value
}

func (s *Server) writeRPCError(writer http.ResponseWriter, status int, id jsontext.Value, code int, message string, data any) {
	s.writeJSON(writer, status, rpcErrorResponse{JSONRPC: "2.0", ID: id, Error: rpcError{Code: code, Message: message, Data: data}})
}

func (s *Server) writeResult(writer http.ResponseWriter, id jsontext.Value, result any) {
	s.writeJSON(writer, http.StatusOK, rpcResult{JSONRPC: "2.0", ID: id, Result: result})
}

// writeMissingToken은 보호 리소스 메타데이터 위치를 알려 클라이언트가 인가 서버를
// 발견할 수 있게 한다. 유효한 Bearer 토큰이 제시되지 않았을 때 HTTP 인증 도전을 쓰고,
// Bearer 형식으로 제시된 토큰의 검증 실패는 tools 결과의 unauthenticated로 분리한다.
func (s *Server) writeMissingToken(writer http.ResponseWriter) {
	metadataURL := s.config.ResourceURL.Clone()
	metadataURL.Path = "/.well-known/oauth-protected-resource"
	metadataURL.RawPath = ""
	writer.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, metadataURL.String()))
	writer.WriteHeader(http.StatusUnauthorized)
}

func (s *Server) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.MarshalWrite(writer, value); err != nil {
		return
	}
}

func domainError(code string, data map[string]any) ToolResult {
	payload := map[string]any{"code": code}
	for key, value := range data {
		payload[key] = value
	}
	encoded, _ := json.Marshal(payload)
	return ToolResult{Content: []Content{{Type: "text", Text: string(encoded)}}, StructuredContent: payload, IsError: true}
}

func validDomainCode(code string) bool {
	return slices.Contains([]string{
		"unauthenticated", "permission_denied", "not_found", "invalid_argument",
		"version_conflict", "limit_exceeded", "result_truncated", "not_supported", "internal",
	}, code)
}

// isWebOnlyTool은 현재 명시된 웹 전용 요청만 판정한다. 새 이름은 명세와 이 목록을
// 함께 갱신하기 전까지 추측해 웹 전용으로 처리하지 않는다.
func isWebOnlyTool(name string) bool {
	switch name {
	case "graph_delete", "graph_grant", "team_create":
		return true
	}
	return false
}

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Method  string         `json:"method"`
	Params  rpcParams      `json:"params"`
}

type rpcParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Meta      rpcMeta        `json:"_meta"`
}

type rpcMeta struct {
	ProtocolVersion string `json:"io.modelcontextprotocol/protocolVersion"`
}

type rpcErrorResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Error   rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitzero"`
}

type rpcResult struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Result  any            `json:"result"`
}

type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		tool("graph_list", "접근 가능한 컨텍스트 그래프를 조회한다", graphListSchema()),
		tool("graph_create", "컨텍스트 그래프를 만든다", graphCreateSchema()),
		tool("graph_get", "컨텍스트 그래프를 조회한다", graphIDSchema()),
		tool("graph_update", "컨텍스트 그래프 정보를 갱신한다", graphUpdateSchema()),
		tool("node_create", "컨텍스트 노드를 만든다", nodeCreateSchema()),
		tool("node_get", "컨텍스트 노드와 홉 범위를 조회한다", nodeGetSchema()),
		tool("node_update", "컨텍스트 노드를 갱신한다", nodeUpdateSchema()),
		tool("node_discard", "컨텍스트 노드를 폐기한다", contextIDSchema()),
		tool("node_restore", "폐기된 컨텍스트 노드를 복구한다", contextIDSchema()),
		tool("context_flow_get", "현재 작업 컨텍스트와 연결된 흐름을 조회한다", contextFlowSchema()),
		tool("relation_list", "사건 관계를 조회한다", relationListSchema()),
		tool("relation_confirm", "사건 관계를 확정한다", relationConfirmSchema()),
		tool("relation_discard", "사건 관계를 폐기한다", relationDiscardSchema()),
	}
}

func tool(name, description string, schema map[string]any) toolDefinition {
	return toolDefinition{Name: name, Description: description, InputSchema: schema}
}
