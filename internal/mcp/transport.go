// Package mcp는 Streamable HTTP MCP 전송 검증과 tools 표면을 제공한다.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
)

const (
	// ProtocolVersion은 이 서버가 지원하는 유일한 MCP protocol revision이다.
	ProtocolVersion = "2026-07-28"
	// Scope는 모든 MCP 접근 토큰에 쓰는 단일 OAuth scope다.
	Scope = "agent-context"
	// cacheScopePublic은 계정과 무관한 결과에 쓰는 캐시 범위다.
	cacheScopePublic = "public"
	// toolsListTTL과 discoverTTL은 「MCP 표면」이 정한 캐시 힌트다. 두 값이 배포로만
	// 바뀌므로 배포 주기보다 짧게 잡은 시작값이며 측정 근거는 아직 없다.
	toolsListTTL              = 5 * time.Minute
	discoverTTL               = time.Hour
	writeIdempotencyExtension = "io.github.alpha-gone/write-idempotency"
	writeIdempotencyRetention = 24 * time.Hour
	// maxRequestBodyBytes는 인증 전 JSON 해석에 적용하는 고정 전송 상한이다.
	// 도구별 문자·참조 상한과 달리 JSON 외피·공백·이스케이프도 포함한다.
	maxRequestBodyBytes = 1 << 20
)

// IdempotencyRequest는 협상된 쓰기 요청을 재생할 때 필요한 전송 외피 값이다.
// 도구 인자와 분리해, 재시도 식별자가 도메인 입력이나 도구 스키마에 섞이지 않게 한다.
type IdempotencyRequest struct {
	// KeyHeader는 검증 전의 Idempotency-Key 헤더 값이다. 형식 검증은 「처리 순서」의
	// 7단계라 권한 확인 뒤에 처리기가 한다.
	KeyHeader   []string
	Fingerprint [sha256.Size]byte
}

type idempotencyContextKey struct{}

// IdempotencyFromContext는 전송 계층이 협상을 확인해 연결한 멱등성 요청을 돌려준다.
func IdempotencyFromContext(ctx context.Context) (IdempotencyRequest, bool) {
	request, ok := ctx.Value(idempotencyContextKey{}).(IdempotencyRequest)
	return request, ok
}

var (
	// ErrInvalidToken은 DPoP proof까지 제시됐지만 접근 토큰 검증에 실패한 결과다.
	ErrInvalidToken = errors.New("invalid_token")
	// ErrInvalidDPoPProof는 proof 형식·결합·재생 또는 Bearer 하향 제시의 실패다.
	ErrInvalidDPoPProof = errors.New("invalid_dpop_proof")
)

// Config는 MCP 리소스 서버의 고정된 공개 경계를 모은다.
type Config struct {
	ResourceURL            *url.URL
	AuthorizationServerURL *url.URL
	AllowedOrigins         []*url.URL
	// ServerInfo는 규약이 모든 결과의 `_meta`에 싣도록 권고한 구현 식별 정보다.
	ServerInfo Implementation
}

// Authentication은 요청별 DPoP 결합을 검증하는 데 필요한 값만 담는다.
type Authentication struct {
	AccessToken string
	Proof       string
	Method      string
	Target      string
}

// VerifyFunc는 DPoP 접근 토큰과 요청 proof를 함께 검증하고 요청 계정을 돌려준다.
type VerifyFunc func(context.Context, Authentication) (model.ID, error)

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
	if config.ServerInfo.Name == "" || config.ServerInfo.Version == "" {
		return nil, fmt.Errorf("MCP 서버 식별 정보가 필요하다")
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
	// Content-Length만 믿으면 길이 미상·chunked 입력이 경계를 우회한다.
	// JSON을 해석하기 전에 스트림을 제한하고 초과 본문을 끝까지 읽지 않는다.
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes)
	defer request.Body.Close()
	if request.ContentLength > maxRequestBodyBytes {
		s.writeBodyTooLarge(writer)
		return
	}

	var body jsontext.Value
	// JSON 문법을 요청 필드의 자료형과 분리한다. 중복 이름은 문법상 허용하지만
	// 아래 요청 해석에서는 거부하므로 Parse error가 아니라 Invalid request다.
	if err := json.UnmarshalRead(request.Body, &body, jsontext.AllowDuplicateNames(true)); err != nil {
		if _, exceeded := errors.AsType[*http.MaxBytesError](err); exceeded {
			s.writeBodyTooLarge(writer)
			return
		}
		s.writeRPCError(writer, http.StatusBadRequest, jsontext.Value("null"), -32700, "Parse error", nil)
		return
	}
	var message rpcRequest
	if err := json.Unmarshal(body, &message); err != nil || body.Kind() != '{' || message.JSONRPC != "2.0" || message.Method == "" {
		s.writeRPCError(writer, http.StatusBadRequest, jsontext.Value("null"), -32600, "Invalid request", nil)
		return
	}
	// 이 MCP 프로필은 문자열·숫자 id만 받고 알림을 쓰지 않는다. 일반 JSON-RPC는
	// null도 허용하지만 여기서는 누락·null·불리언·객체·배열 id를 거부한다.
	// 받아 주면 결과에 "id":null을 실어 클라이언트가 응답을 요청과 잇지 못한다.
	if !validRPCID(message.ID) {
		s.writeRPCError(writer, http.StatusBadRequest, jsontext.Value("null"), -32600, "Invalid request", nil)
		return
	}
	if message.Params.Meta.ProtocolVersion != protocolVersion || message.Method != method {
		s.writeRPCError(writer, http.StatusBadRequest, message.ID, -32020, "Header mismatch", nil)
		return
	}
	// 1f다. 해석은 되었으나 필수 메타데이터가 빠진 것은 파라미터 문제이므로 Invalid params다.
	// 값의 내용은 보지 않고 선언 여부만 본다. 빈 객체도 유효한 기능 선언이다.
	if !message.Params.Meta.declared() {
		s.writeRPCError(writer, http.StatusBadRequest, message.ID, -32602, "Invalid params", map[string]any{"missing": message.Params.Meta.missing()})
		return
	}
	if protocolVersion != ProtocolVersion {
		s.writeRPCError(writer, http.StatusBadRequest, message.ID, -32022, "Unsupported protocol version", map[string]any{"supported": []string{ProtocolVersion}, "requested": protocolVersion})
		return
	}
	if !slices.Contains([]string{"server/discover", "tools/list", "tools/call"}, method) {
		s.writeRPCError(writer, http.StatusNotFound, message.ID, -32601, "Method not found", nil)
		return
	}
	if method == "tools/call" && !matchesToolHeader(request, message.Params.Name) {
		s.writeRPCError(writer, http.StatusBadRequest, message.ID, -32020, "Header mismatch", nil)
		return
	}
	// `server/discover`는 인증보다 앞이다. 클라이언트가 인가를 받기 전에 버전과 기능을
	// 확인하는 것이 이 메서드의 목적이며, 담는 값에 계정이나 컨텍스트가 없다.
	if method == "server/discover" {
		s.writeResult(writer, message.ID, discoverResult())
		return
	}

	authentication, state := dpopAuthentication(request, s.config.ResourceURL.String())
	if state == authenticationMissing {
		s.writeAuthenticationChallenge(writer, "")
		return
	}
	if state == authenticationInvalid {
		s.writeAuthenticationChallenge(writer, "invalid_dpop_proof")
		return
	}
	accountID, err := s.verify(request.Context(), authentication)
	if err != nil {
		// 자격 증명 실패와 내부 장애를 나눈다. 「토큰 검증」이 확정한 대로 재인증이
		// 답이 아닌 실패에 401을 돌려주면 클라이언트가 같은 자리에서 다시 막힌다.
		if errors.Is(err, ErrInvalidToken) {
			s.writeAuthenticationChallenge(writer, "invalid_token")
			return
		}
		if errors.Is(err, ErrInvalidDPoPProof) {
			s.writeAuthenticationChallenge(writer, "invalid_dpop_proof")
			return
		}
		s.writeRPCError(writer, http.StatusInternalServerError, message.ID, -32603, "Internal error", nil)
		return
	}
	if method == "tools/list" {
		s.writeResult(writer, message.ID, listToolsResult())
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
	callContext := request.Context()
	if message.Params.Meta.supportsWriteIdempotency() && isIdempotentWrite(message.Params.Name) {
		fingerprint, err := requestFingerprint(message.Params.Name, message.Params.Arguments)
		if err != nil {
			s.writeResult(writer, message.ID, domainError("internal", nil))
			return
		}
		callContext = context.WithValue(callContext, idempotencyContextKey{}, IdempotencyRequest{
			KeyHeader: request.Header.Values("Idempotency-Key"), Fingerprint: fingerprint,
		})
	}
	result, err := s.call(callContext, accountID, message.Params.Name, message.Params.Arguments)
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
		"resource":                          s.config.ResourceURL.String(),
		"authorization_servers":             []string{s.config.AuthorizationServerURL.String()},
		"scopes_supported":                  []string{Scope},
		"dpop_bound_access_tokens_required": true,
		"dpop_signing_alg_values_supported": []string{"ES256"},
	})
}

// AuthorizationServerMetadata는 RFC 8414 인가 서버 메타데이터를 반환한다.
func (s *Server) AuthorizationServerMetadata(writer http.ResponseWriter, _ *http.Request) {
	base := s.config.AuthorizationServerURL.String()
	s.writeJSON(writer, http.StatusOK, map[string]any{
		"issuer":                            base,
		"authorization_endpoint":            base + "/authorize",
		"token_endpoint":                    base + "/token",
		"jwks_uri":                          base + "/jwks.json",
		"response_types_supported":          []string{"code"},
		"grant_types_supported":             []string{"authorization_code"},
		"code_challenge_methods_supported":  []string{"S256"},
		"dpop_signing_alg_values_supported": []string{"ES256"},
		"scopes_supported":                  []string{Scope},
		// 생략하면 RFC 8414가 client_secret_basic을 기본값으로 가정하게 둔다. 이 서버는
		// 공개 클라이언트만 받으므로 선언하지 않으면 클라이언트가 없는 비밀을 찾는다.
		"token_endpoint_auth_methods_supported": []string{"none"},
		// 인가 응답에 iss를 싣는다는 사실을 알린다. 싣기만 하고 선언하지 않으면
		// 클라이언트가 iss 없는 응답도 정상으로 받아들여 mix-up 방어가 성립하지 않는다.
		"authorization_response_iss_parameter_supported": true,
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

type authenticationState uint8

const (
	authenticationMissing authenticationState = iota
	authenticationInvalid
	authenticationReady
)

func dpopAuthentication(request *http.Request, target string) (Authentication, authenticationState) {
	authorization := request.Header.Values("Authorization")
	proof := request.Header.Values("DPoP")
	if len(authorization) == 0 && len(proof) == 0 {
		return Authentication{}, authenticationMissing
	}
	if len(authorization) != 1 || len(proof) != 1 {
		return Authentication{}, authenticationInvalid
	}
	// 스킴 이름은 RFC 9110대로 대소문자를 구분하지 않는다.
	scheme, accessToken, found := strings.Cut(authorization[0], " ")
	if !found || !strings.EqualFold(scheme, "DPoP") || accessToken == "" || strings.ContainsAny(accessToken, " \t") || proof[0] == "" {
		return Authentication{}, authenticationInvalid
	}
	return Authentication{AccessToken: accessToken, Proof: proof[0], Method: request.Method, Target: target}, authenticationReady
}

func (s *Server) writeBodyTooLarge(writer http.ResponseWriter) {
	s.writeRPCError(writer, http.StatusRequestEntityTooLarge, jsontext.Value("null"), -32600, "Request body too large", map[string]any{"max_body_bytes": maxRequestBodyBytes})
}

func (s *Server) writeRPCError(writer http.ResponseWriter, status int, id jsontext.Value, code int, message string, data any) {
	s.writeJSON(writer, status, rpcErrorResponse{JSONRPC: "2.0", ID: id, Error: rpcError{Code: code, Message: message, Data: data}})
}

// writeResult는 결과를 규약의 응답 외피에 넣어 내보낸다.
//
// 「MCP 표면」이 모든 결과에 resultType과 서버 정보를 요구한다. 호출 지점마다 넣지 않고
// 여기에서 한 번에 붙이는 이유는 빠뜨릴 자리를 없애기 위해서다. 이 서버는 다회 왕복을
// 쓰지 않으므로 resultType은 언제나 complete다.
func (s *Server) writeResult(writer http.ResponseWriter, id jsontext.Value, result any) {
	envelope, err := s.envelope(result)
	if err != nil {
		s.writeRPCError(writer, http.StatusInternalServerError, id, -32603, "Internal error", nil)
		return
	}
	s.writeJSON(writer, http.StatusOK, rpcResult{JSONRPC: "2.0", ID: id, Result: envelope})
}

// envelope는 연산이 만든 결과의 필드 위에 규약의 공통 필드를 얹는다.
//
// 필드 값을 원시 JSON으로 다루는 이유는 다시 해석하지 않기 위해서다. Go 값으로 되돌려
// 담으면 정수가 부동소수점을 거치며 표현이 바뀔 수 있는데, `ttlMs`처럼 규약이 정수로
// 정한 값에서 그 변환을 만들 이유가 없다.
func (s *Server) envelope(result any) (map[string]jsontext.Value, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]jsontext.Value)
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	meta, err := json.Marshal(resultMeta{ServerInfo: s.config.ServerInfo})
	if err != nil {
		return nil, err
	}
	fields["resultType"] = jsontext.Value(`"complete"`)
	fields["_meta"] = meta
	return fields, nil
}

// writeAuthenticationChallenge는 보호 리소스 메타데이터 위치와 필요한 scope를 알려
// 클라이언트가 인가 서버를 발견하고 재인증을 시작할 수 있게 한다.
//
// proof 오류와 토큰 오류는 클라이언트가 재인증 여부를 판단할 수 있게 구분하지만, 세부
// 실패 원인은 노출하지 않는다.
func (s *Server) writeAuthenticationChallenge(writer http.ResponseWriter, code string) {
	metadataURL := s.config.ResourceURL.Clone()
	metadataURL.Path = "/.well-known/oauth-protected-resource"
	metadataURL.RawPath = ""
	challenge := fmt.Sprintf(`DPoP algs="ES256", resource_metadata=%q, scope=%q`, metadataURL.String(), Scope)
	if code != "" {
		challenge = fmt.Sprintf(`DPoP error=%q, algs="ES256", resource_metadata=%q, scope=%q`, code, metadataURL.String(), Scope)
	}
	writer.Header().Set("WWW-Authenticate", challenge)
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
	// 인증 실패는 이 목록에 없다. 「오류 코드」가 그것을 도메인 결과가 아니라 전송
	// 계층의 401로 확정했으므로 tools 결과로 나갈 경로가 없다.
	return slices.Contains([]string{
		"permission_denied", "not_found", "invalid_argument",
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

// validRPCID는 JSON-RPC 요청 식별자가 문자열 또는 숫자인지 본다.
func validRPCID(id jsontext.Value) bool {
	return id.Kind() == '"' || id.Kind() == '0'
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

// rpcMeta는 규약이 요청마다 요구하는 per-request 메타데이터를 담는다.
//
// 규약이 세션을 두지 않으므로 protocol revision과 클라이언트 기능이 요청마다 실린다.
// ClientCapabilities를 문자열이나 구조체가 아니라 원시 JSON으로 받는 이유는 이 서버가
// 값을 해석하지 않기 때문이다. 확인할 것은 선언 여부뿐이고, 빈 객체도 유효한 선언이다.
type rpcMeta struct {
	ProtocolVersion    string         `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities jsontext.Value `json:"io.modelcontextprotocol/clientCapabilities"`
}

// declared는 필수 메타데이터가 모두 실렸는지 본다.
func (meta rpcMeta) declared() bool { return len(meta.missing()) == 0 }

// missing은 빠진 필수 메타데이터 키를 돌려준다. 클라이언트가 무엇을 더해야 하는지
// 알 수 있도록 오류의 부가 정보로 그대로 실린다.
func (meta rpcMeta) missing() []string {
	missing := make([]string, 0, 2)
	if meta.ProtocolVersion == "" {
		missing = append(missing, "io.modelcontextprotocol/protocolVersion")
	}
	if len(meta.ClientCapabilities) == 0 {
		missing = append(missing, "io.modelcontextprotocol/clientCapabilities")
	}
	return missing
}

func (meta rpcMeta) supportsWriteIdempotency() bool {
	var capabilities struct {
		Extensions map[string]jsontext.Value `json:"extensions"`
	}
	if err := json.Unmarshal(meta.ClientCapabilities, &capabilities); err != nil {
		return false
	}
	_, ok := capabilities.Extensions[writeIdempotencyExtension]
	return ok
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
	JSONRPC string                    `json:"jsonrpc"`
	ID      jsontext.Value            `json:"id"`
	Result  map[string]jsontext.Value `json:"result"`
}

type resultMeta struct {
	ServerInfo Implementation `json:"io.modelcontextprotocol/serverInfo"`
}

// Implementation은 규약이 정한 구현 식별 정보다. 표시와 진단에만 쓰이며 판정 근거가
// 되지 않는다.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// discoverResult는 `server/discover`가 알릴 지원 revision과 기능을 만든다.
//
// 이 서버는 `tools`만 제공하므로 기능 선언도 그 하나다. 빈 객체는 추가 설정 없이
// 지원한다는 뜻이다.
func discoverResult() map[string]any {
	return map[string]any{
		"supportedVersions": []string{ProtocolVersion},
		"capabilities": map[string]any{
			"tools": map[string]any{},
			"extensions": map[string]any{writeIdempotencyExtension: map[string]any{
				"retentionMs": writeIdempotencyRetention.Milliseconds(),
			}},
		},
		"ttlMs":      discoverTTL.Milliseconds(),
		"cacheScope": cacheScopePublic,
	}
}

func isIdempotentWrite(name string) bool {
	return slices.Contains([]string{
		"graph_create", "graph_update", "node_create", "node_update",
		"node_discard", "node_restore", "relation_confirm", "relation_discard",
	}, name)
}

// parseIdempotencyKey는 하이픈 표기의 UUIDv7을 담은 Structured Fields String 하나만 받는다.
func parseIdempotencyKey(values []string) (model.ID, bool) {
	if len(values) != 1 {
		return model.ID{}, false
	}
	quoted, ok := strings.CutPrefix(values[0], "\"")
	if !ok {
		return model.ID{}, false
	}
	key, ok := strings.CutSuffix(quoted, "\"")
	if !ok || len(key) != 36 {
		return model.ID{}, false
	}
	parsed, err := model.ParseID(key)
	if err != nil || !parsed.IsV7() {
		return model.ID{}, false
	}
	return parsed, true
}

func requestFingerprint(name string, arguments map[string]any) ([sha256.Size]byte, error) {
	encoded, err := json.Marshal(map[string]any{"arguments": arguments, "name": name}, json.Deterministic(true))
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("멱등성 요청 지문 직렬화: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

// listToolsResult는 도구 목록과 규약이 요구하는 캐시 힌트를 함께 만든다.
//
// cacheScope가 public인 이유는 목록이 계정과 무관하기 때문이다. 「MCP 연산 매핑」이
// 연산 13종을 고정했으므로 어느 토큰으로 물어도 같은 목록이 나오고, 공유 캐시가 이를
// 다른 호출자에게 돌려줘도 새어 나갈 것이 없다.
func listToolsResult() map[string]any {
	return map[string]any{
		"tools":      toolDefinitions(),
		"ttlMs":      toolsListTTL.Milliseconds(),
		"cacheScope": cacheScopePublic,
	}
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
		tool("node_discard", "컨텍스트 노드를 폐기한다", nodeLifecycleSchema()),
		tool("node_restore", "폐기된 컨텍스트 노드를 복구한다", nodeLifecycleSchema()),
		tool("context_flow_get", "현재 작업 컨텍스트와 연결된 흐름을 조회한다", contextFlowSchema()),
		tool("relation_list", "사건 관계를 조회한다", relationListSchema()),
		tool("relation_confirm", "사건 관계를 확정한다", relationConfirmSchema()),
		tool("relation_discard", "사건 관계를 폐기한다", relationDiscardSchema()),
	}
}

func tool(name, description string, schema map[string]any) toolDefinition {
	return toolDefinition{Name: name, Description: description, InputSchema: schema}
}
