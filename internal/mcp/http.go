package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// AuthenticatedHandler는 Bearer 검증을 마친 결합만 받는다.
type AuthenticatedHandler func(http.ResponseWriter, *http.Request, Principal)

// ToolHandler는 도구 인자·clientInfo와 독립된 인증 주체를 명시적으로 받는다.
type ToolHandler func(context.Context, Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error)

// HTTPConfig는 cmd/server가 검증한 구성과 인증 경계를 주입한다.
// Protect는 매 요청의 Bearer·scope·현재 폐기를 검사하고 실패 시 응답을 끝낸다.
type HTTPConfig struct {
	AllowedOrigins []string
	Protect        func(AuthenticatedHandler) http.Handler
	Handlers       map[string]ToolHandler
}

type principalKey struct{}

// NewHandler는 완전한 14종 처리기를 요구한다. 미구현 도구를 임시 성공으로
// 공개하지 않으며 TLS 판정·요청 시간 예산은 바깥 HTTP 서버가 소유한다.
func NewHandler(config HTTPConfig) (http.Handler, error) {
	if config.Protect == nil || len(config.AllowedOrigins) == 0 {
		return nil, fmt.Errorf("MCP 인증 경계와 허용 출처가 필요하다")
	}
	origins := make(map[string]bool, len(config.AllowedOrigins))
	for _, origin := range config.AllowedOrigins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
			strings.ContainsAny(origin, "* \t\r\n") || origins[origin] {
			return nil, fmt.Errorf("MCP 허용 출처 구성을 확인해 주십시오")
		}
		origins[origin] = true
	}
	catalog, err := newToolCatalog()
	if err != nil {
		return nil, err
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "agent-context", Version: "1"}, &sdk.ServerOptions{
		SupportedProtocolVersions: []string{protocolRevision},
		Instructions:              serverInstructions,
		Capabilities:              &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
		SetCacheable: func(_ context.Context, _ sdk.Request, cache *sdk.Cacheable) {
			cache.TTLMs, cache.CacheScope = 0, "private"
		},
	})
	handlers := make(map[string]sdk.ToolHandler, len(config.Handlers))
	for name, handler := range config.Handlers {
		if handler == nil {
			return nil, fmt.Errorf("MCP 업무 처리기가 필요하다")
		}
		handlers[name] = func(ctx context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			principal, ok := ctx.Value(principalKey{}).(Principal)
			if !ok {
				return writeErrorResult(&DependencyFailure{})
			}
			result, err := handler(ctx, principal, request)
			if ctx.Err() != nil {
				return writeErrorResult(ctx.Err())
			}
			if err != nil {
				return writeErrorResult(err)
			}
			if result == nil {
				return writeErrorResult(&DependencyFailure{})
			}
			return result, nil
		}
	}
	if err := catalog.register(server, handlers); err != nil {
		return nil, err
	}
	next := sdkTransport(server, sdk.DefaultMaxRequestBodyBytes, true)
	protected := config.Protect(func(w http.ResponseWriter, request *http.Request, principal Principal) {
		if request.Context().Err() != nil {
			return
		}
		if !principal.AccountID.IsV7() || principal.ClientID == "" {
			writeTransportError(w, 500, nil, -32603, "authentication unavailable", nil)
			return
		}
		next.ServeHTTP(w, request.WithContext(context.WithValue(request.Context(), principalKey{}, principal)))
	})
	if protected == nil {
		return nil, fmt.Errorf("MCP 인증 처리기가 필요하다")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w = privateResponseWriter{w}
		w.Header().Set("Cache-Control", "no-store")
		if request.Context().Err() != nil {
			return
		}
		if values := request.Header.Values("Origin"); len(values) != 0 && (len(values) != 1 || !origins[values[0]]) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if request.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		values := request.Header.Values("Content-Type")
		mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if len(values) != 1 || err != nil || mediaType != "application/json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		if !acceptsMCP(request.Header.Values("Accept")) {
			writeTransportError(w, 400, nil, -32600, "Accept must support JSON and SSE", nil)
			return
		}
		_, revisionOK := transportHeader(request.Header, "MCP-Protocol-Version")
		method, methodOK := transportHeader(request.Header, "Mcp-Method")
		nameOK := true
		if method == "tools/call" {
			name, ok := transportHeader(request.Header, "Mcp-Name")
			_, decoded := decodeTransportName(name)
			nameOK = ok && decoded
		}
		if !revisionOK || !methodOK || !nameOK {
			writeTransportError(w, 400, nil, -32020, "missing or invalid MCP headers", nil)
			return
		}
		// 바이트 수만 제한해 버퍼링한다. JSON 해석·스키마·SDK 호출은 인증 뒤다.
		data, ok := readTransportBody(w, request, sdk.DefaultMaxRequestBodyBytes)
		if !ok || request.Context().Err() != nil {
			return
		}
		forward := request.Clone(request.Context())
		forward.Body = io.NopCloser(bytes.NewReader(data))
		forward.ContentLength = int64(len(data))
		protected.ServeHTTP(w, forward)
	}), nil
}

func acceptsMCP(values []string) bool {
	// RFC 9110의 구체적인 범위가 wildcard보다 우선한다. q=0인 JSON/SSE를
	// SDK가 허용하는 차이를 인증 전에 보완한다.
	for _, target := range []string{"application/json", "text/event-stream"} {
		quality, specificity := 0.0, -1
		for _, value := range values {
			for part := range strings.SplitSeq(value, ",") {
				mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
				if err != nil {
					return false
				}
				q := 1.0
				if text, present := params["q"]; present {
					q, err = strconv.ParseFloat(text, 64)
					if err != nil || !(q >= 0 && q <= 1) {
						return false
					}
				}
				score := -1
				switch {
				case mediaType == target:
					score = 2
				case mediaType == strings.Split(target, "/")[0]+"/*":
					score = 1
				case mediaType == "*/*":
					score = 0
				}
				if score >= 0 && (score > specificity || score == specificity && q > quality) {
					specificity, quality = score, q
				}
			}
		}
		if quality == 0 {
			return false
		}
	}
	return true
}

// SDK는 JSON 응답에도 SSE용 Cache-Control을 설정한다. 실제 헤더 전송 시
// 보호 리소스의 no-store를 유지하며 JSON-RPC의 private·ttlMs=0도 별도로 둔다.
type privateResponseWriter struct{ http.ResponseWriter }

func (w privateResponseWriter) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(status)
}

func (w privateResponseWriter) Write(data []byte) (int, error) {
	w.Header().Set("Cache-Control", "no-store")
	return w.ResponseWriter.Write(data)
}

func (w privateResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
