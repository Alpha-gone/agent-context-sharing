package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func httpTestConfig(t *testing.T, authenticate, calls *atomic.Int64) HTTPConfig {
	t.Helper()
	account, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	config := HTTPConfig{AllowedOrigins: []string{"https://allowed.test"}, Handlers: map[string]ToolHandler{}, Protect: func(next AuthenticatedHandler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			authenticate.Add(1)
			if request.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			next(w, request, Principal{AccountID: account, ClientID: request.Header.Get("Authorization")})
		})
	}}
	for _, tool := range toolDefinitions() {
		config.Handlers[tool.Name] = func(_ context.Context, principal Principal, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			value := map[string]any{"account": principal.AccountID.String(), "client": principal.ClientID}
			data, _ := json.Marshal(value)
			return &sdk.CallToolResult{StructuredContent: value, Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}}, nil
		}
	}
	return config
}

func httpTestRequest(t *testing.T, method string) *http.Request {
	t.Helper()
	body := sdkProbeRequest(method)
	if method == "tools/call" {
		body["params"].(map[string]any)["name"] = "graph_list"
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://resource.test/mcp", bytes.NewReader(data))
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("MCP-Protocol-Version", protocolRevision)
	request.Header.Set("Mcp-Method", method)
	if method == "tools/call" {
		request.Header.Set("Mcp-Name", "graph_list")
	}
	request.Header.Set("Authorization", "client-a")
	return request
}

func TestMCPHTTPBoundary(t *testing.T) {
	for _, sample := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"allowed origin", func(r *http.Request) { r.Header.Set("Origin", "https://allowed.test") }, 200},
		{"unknown origin", func(r *http.Request) { r.Header.Set("Origin", "https://other.test") }, 403},
		{"empty origin", func(r *http.Request) { r.Header.Set("Origin", "") }, 403},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"duplicate origin", func(r *http.Request) { r.Header["Origin"] = []string{"https://allowed.test", "https://allowed.test"} }, 403},
		{"origin list", func(r *http.Request) { r.Header.Set("Origin", "https://allowed.test https://other.test") }, 403},
		{"origin case", func(r *http.Request) { r.Header.Set("Origin", "https://ALLOWED.test") }, 403},
		{"origin slash", func(r *http.Request) { r.Header.Set("Origin", "https://allowed.test/") }, 403},
		{"GET", func(r *http.Request) { r.Method = "GET" }, 405},
		{"DELETE", func(r *http.Request) { r.Method = "DELETE" }, 405},
		{"HEAD", func(r *http.Request) { r.Method = "HEAD" }, 405},
		{"OPTIONS", func(r *http.Request) { r.Method = "OPTIONS" }, 405},
		{"missing content type", func(r *http.Request) { r.Header.Del("Content-Type") }, 415},
		{"wrong content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"duplicate content type", func(r *http.Request) { r.Header.Add("Content-Type", "application/json") }, 415},
		{"JSON parameters", func(r *http.Request) { r.Header.Set("Content-Type", "Application/JSON; charset=utf-8") }, 200},
		{"missing accept", func(r *http.Request) { r.Header.Del("Accept") }, 400},
		{"JSON accept only", func(r *http.Request) { r.Header.Set("Accept", "application/json") }, 400},
		{"SSE accept only", func(r *http.Request) { r.Header.Set("Accept", "text/event-stream") }, 400},
		{"wildcard", func(r *http.Request) { r.Header.Set("Accept", "*/*") }, 200},
		{"q zero", func(r *http.Request) { r.Header.Set("Accept", "application/json;q=0, text/event-stream") }, 400},
		{"q zero specificity", func(r *http.Request) { r.Header.Set("Accept", "*/*, application/json;q=0") }, 400},
		{"q range", func(r *http.Request) { r.Header.Set("Accept", "application/json;q=2, text/event-stream") }, 400},
		{"q malformed", func(r *http.Request) { r.Header.Set("Accept", "application/json;q=NaN, text/event-stream") }, 400},
		{"two accept fields", func(r *http.Request) { r.Header["Accept"] = []string{"application/json;q=0.8", "text/event-stream"} }, 200},
		{"missing revision", func(r *http.Request) { r.Header.Del("MCP-Protocol-Version") }, 400},
		{"duplicate method", func(r *http.Request) { r.Header.Add("Mcp-Method", "tools/call") }, 400},
		{"missing name", func(r *http.Request) { r.Header.Del("Mcp-Name") }, 400},
		{"malformed name", func(r *http.Request) { r.Header.Set("Mcp-Name", "=?base64?%%%?=") }, 400},
		{"encoded name", func(r *http.Request) { r.Header.Set("Mcp-Name", "=?base64?Z3JhcGhfbGlzdA==?=") }, 200},
		{"no bearer", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var authenticate, calls atomic.Int64
			handler, err := NewHandler(httpTestConfig(t, &authenticate, &calls))
			if err != nil {
				t.Fatal(err)
			}
			request := httpTestRequest(t, "tools/call")
			sample.change(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != sample.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, sample.status, response.Body)
			}
			if sample.status != 200 && calls.Load() != 0 {
				t.Fatal("거부한 요청의 업무 호출")
			}
			if sample.status != 200 && sample.status != 401 && authenticate.Load() != 0 {
				t.Fatal("선행 거부가 인증을 호출")
			}
			if sample.status == 405 && response.Header().Get("Allow") != "POST" {
				t.Fatal("Allow 누락")
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Mcp-Session-Id") != "" {
				t.Fatal("HTTP 캐시·세션 경계")
			}
		})
	}
}

type countedHTTPBody struct {
	io.Reader
	bytes   int
	failure error
}

func (body *countedHTTPBody) Read(data []byte) (int, error) {
	if body.failure != nil {
		return 0, body.failure
	}
	n, err := body.Reader.Read(data)
	body.bytes += n
	return n, err
}
func (*countedHTTPBody) Close() error { return nil }

func TestMCPHTTPBodyLimit(t *testing.T) {
	for _, mode := range []string{"below", "exact", "declared over", "unknown over", "small declared over", "chunked over", "large string", "large array", "read error", "parse error", "invalid origin", "invalid header"} {
		t.Run(mode, func(t *testing.T) {
			var authenticate, calls atomic.Int64
			handler, err := NewHandler(httpTestConfig(t, &authenticate, &calls))
			if err != nil {
				t.Fatal(err)
			}
			request := httpTestRequest(t, "tools/call")
			data, _ := io.ReadAll(request.Body)
			limit := sdk.DefaultMaxRequestBodyBytes
			text := string(data) + strings.Repeat(" ", limit-len(data))
			status, code, authCount, callCount := 413, float64(-32600), int64(0), int64(0)
			switch mode {
			case "below":
				text = text[:len(text)-1]
				status, authCount, callCount = 200, 1, 1
			case "exact":
				status, authCount, callCount = 200, 1, 1
			case "large string":
				text = `{"large":"` + strings.Repeat("a", limit) + `"}`
			case "large array":
				text = `[` + strings.Repeat("0,", limit/2) + `0]`
			case "read error":
				text = ""
				status = 400
			case "parse error":
				text = `{"jsonrpc":`
				status, code, authCount = 400, -32700, 1
			default:
				text += " "
			}
			body := &countedHTTPBody{Reader: strings.NewReader(text)}
			request.Body, request.ContentLength = body, -1
			switch mode {
			case "declared over":
				request.ContentLength = int64(len(text))
			case "small declared over":
				request.ContentLength = 1
			case "chunked over":
				request.TransferEncoding = []string{"chunked"}
			case "read error":
				body.failure = errors.New("synthetic private body error")
			case "invalid origin":
				request.Header.Set("Origin", "https://other.test")
				status = 403
			case "invalid header":
				request.Header.Del("Mcp-Method")
				status, code = 400, -32020
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != status || authenticate.Load() != authCount || calls.Load() != callCount {
				t.Fatalf("status=%d auth=%d calls=%d body=%s", response.Code, authenticate.Load(), calls.Load(), response.Body)
			}
			if mode == "declared over" || mode == "invalid origin" || mode == "invalid header" {
				if body.bytes != 0 {
					t.Fatal("선행 거부가 본문을 읽음")
				}
			}
			if body.bytes > limit+1 {
				t.Fatalf("제한 초과 읽기: %d", body.bytes)
			}
			if status != 200 && status != 403 {
				var result map[string]any
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result["error"].(map[string]any)["code"] != code {
					t.Fatal("JSON-RPC 오류 외피", response.Body)
				}
			}
			if strings.Contains(response.Body.String(), "synthetic private") {
				t.Fatal("입력 읽기 오류 원문 노출")
			}
		})
	}
}

func TestMCPHTTPIndependentDiscovery(t *testing.T) {
	for range 2 {
		var authenticate, calls atomic.Int64
		config := httpTestConfig(t, &authenticate, &calls)
		handler, err := NewHandler(config)
		if err != nil {
			t.Fatal(err)
		}
		config.AllowedOrigins[0] = "https://changed.test"
		delete(config.Handlers, "graph_list")
		for _, method := range []string{"tools/call", "server/discover", "tools/list", "tools/call"} {
			for _, client := range []string{"client-a", "client-b"} {
				request := httpTestRequest(t, method)
				request.Header.Set("Authorization", client)
				request.Header.Set("Origin", "https://allowed.test")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 200 {
					t.Fatal(response.Code, response.Body)
				}
				result := decodeSDKProbe(t, sdkProbeResponse{response.Code, response.Header(), response.Body.Bytes()})["result"].(map[string]any)
				if result["resultType"] != "complete" {
					t.Fatal("완료 외피", result)
				}
				if method == "tools/call" {
					if result["structuredContent"].(map[string]any)["client"] != client {
						t.Fatal("인증 주체가 이전 요청에서 유출")
					}
				} else {
					if result["ttlMs"] != float64(0) || result["cacheScope"] != "private" {
						t.Fatal("캐시 경계", result)
					}
					if method == "tools/list" && len(result["tools"].([]any)) != 14 {
						t.Fatal("도구 목록")
					}
					if method == "server/discover" {
						if !reflect.DeepEqual(result["supportedVersions"], []any{protocolRevision}) || result["instructions"] != serverInstructions {
							t.Fatal("발견 고정값", result)
						}
						info := result["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any)
						if info["name"] != "agent-context" {
							t.Fatal("서버 정보")
						}
					}
				}
			}
		}
		if authenticate.Load() != 8 || calls.Load() != 4 {
			t.Fatal("매 요청 인증 경계")
		}
	}
}

func TestMCPHTTPConstruction(t *testing.T) {
	for _, mode := range []string{"no protect", "no origins", "HTTP origin", "wildcard", "duplicate", "path", "missing tool", "nil tool", "extra tool", "nil protected"} {
		t.Run(mode, func(t *testing.T) {
			var authenticate, calls atomic.Int64
			config := httpTestConfig(t, &authenticate, &calls)
			switch mode {
			case "no protect":
				config.Protect = nil
			case "no origins":
				config.AllowedOrigins = nil
			case "HTTP origin":
				config.AllowedOrigins = []string{"http://allowed.test"}
			case "wildcard":
				config.AllowedOrigins = []string{"https://*.test"}
			case "duplicate":
				config.AllowedOrigins = append(config.AllowedOrigins, config.AllowedOrigins[0])
			case "path":
				config.AllowedOrigins = []string{"https://allowed.test/"}
			case "missing tool":
				delete(config.Handlers, "graph_list")
			case "nil tool":
				config.Handlers["graph_list"] = nil
			case "extra tool":
				config.Handlers["extra"] = config.Handlers["graph_list"]
			case "nil protected":
				config.Protect = func(AuthenticatedHandler) http.Handler { return nil }
			}
			if handler, err := NewHandler(config); err == nil || handler != nil {
				t.Fatal("불완전 조립 수락")
			}
			if authenticate.Load() != 0 || calls.Load() != 0 {
				t.Fatal("구성 중 부작용")
			}
		})
	}
}

func TestMCPHTTPCancellationAndMissingIdentity(t *testing.T) {
	for _, mode := range []string{"before", "after auth", "missing principal"} {
		t.Run(mode, func(t *testing.T) {
			var authenticate, calls atomic.Int64
			config := httpTestConfig(t, &authenticate, &calls)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			config.Protect = func(next AuthenticatedHandler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					authenticate.Add(1)
					if mode == "after auth" {
						cancel()
					}
					next(w, r, Principal{})
				})
			}
			handler, err := NewHandler(config)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "before" {
				cancel()
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httpTestRequest(t, "tools/call").WithContext(ctx))
			if calls.Load() != 0 {
				t.Fatal("취소·미인증 업무 호출")
			}
			if mode == "missing principal" && response.Code != 500 {
				t.Fatal("빈 인증 주체 수락")
			}
			if mode == "before" && authenticate.Load() != 0 {
				t.Fatal("취소 뒤 인증 호출")
			}
		})
	}
}

func TestMCPHTTPEnvelopeRejections(t *testing.T) {
	for _, sample := range []struct {
		name   string
		change func(map[string]any)
		header map[string]string
		status int
		code   float64
	}{
		{"missing metadata", func(b map[string]any) { delete(b["params"].(map[string]any), "_meta") }, nil, 400, -32602},
		{"missing capabilities", func(b map[string]any) { delete(sdkProbeMeta(b), sdk.MetaKeyClientCapabilities) }, nil, 400, -32602},
		{"null capabilities", func(b map[string]any) { sdkProbeMeta(b)[sdk.MetaKeyClientCapabilities] = nil }, nil, 400, -32602},
		{"version mismatch", nil, map[string]string{"MCP-Protocol-Version": "2025-11-25"}, 400, -32020},
		{"method mismatch", nil, map[string]string{"Mcp-Method": "tools/list"}, 400, -32020},
		{"name mismatch", nil, map[string]string{"Mcp-Name": "graph_get"}, 400, -32020},
		{"unsupported revision", func(b map[string]any) { sdkProbeMeta(b)[sdk.MetaKeyProtocolVersion] = "2025-11-25" }, map[string]string{"MCP-Protocol-Version": "2025-11-25"}, 400, -32022},
		{"initialize", func(b map[string]any) { b["method"] = "initialize" }, map[string]string{"Mcp-Method": "initialize"}, 404, -32601},
		{"unknown method", func(b map[string]any) { b["method"] = "unknown" }, map[string]string{"Mcp-Method": "unknown"}, 404, -32601},
		{"response", func(b map[string]any) { delete(b, "method"); b["result"] = map[string]any{} }, nil, 400, -32600},
		{"notification", func(b map[string]any) { delete(b, "id"); b["method"] = "notifications/cancelled" }, map[string]string{"Mcp-Method": "notifications/cancelled"}, 400, -32600},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var authenticate, calls atomic.Int64
			handler, err := NewHandler(httpTestConfig(t, &authenticate, &calls))
			if err != nil {
				t.Fatal(err)
			}
			body := sdkProbeRequest("tools/call")
			body["params"].(map[string]any)["name"] = "graph_list"
			if sample.change != nil {
				sample.change(body)
			}
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			request := httpTestRequest(t, "tools/call")
			request.Body = io.NopCloser(bytes.NewReader(data))
			request.ContentLength = int64(len(data))
			for key, value := range sample.header {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var value map[string]any
			if response.Code != sample.status || json.Unmarshal(response.Body.Bytes(), &value) != nil || value["error"].(map[string]any)["code"] != sample.code {
				t.Fatal(response.Code, response.Body)
			}
			if calls.Load() != 0 || authenticate.Load() != 1 {
				t.Fatal("외피 오류의 실행 순서")
			}
			if sample.code == -32022 {
				detail := value["error"].(map[string]any)["data"].(map[string]any)
				if !reflect.DeepEqual(detail["supported"], []any{protocolRevision}) || detail["requested"] != "2025-11-25" {
					t.Fatal("revision 안내")
				}
			}
		})
	}
}

func TestMCPHTTPMaximumEscapedInputs(t *testing.T) {
	for _, tool := range []string{"node_create", "context_flow_get"} {
		t.Run(tool, func(t *testing.T) {
			var authenticate, calls atomic.Int64
			handler, err := NewHandler(httpTestConfig(t, &authenticate, &calls))
			if err != nil {
				t.Fatal(err)
			}
			arguments := catalogSample(tool)
			if tool == "node_create" {
				arguments = eventCatalogSample()
				arguments["body"] = strings.Repeat("한", 8000)
				refs := make([]string, 1000)
				for i := range refs {
					refs[i] = catalogID
				}
				arguments["member_refs"] = refs
			} else {
				arguments["work_context"] = strings.Repeat("한", 8000)
			}
			body := sdkProbeRequest("tools/call")
			body["params"].(map[string]any)["name"], body["params"].(map[string]any)["arguments"] = tool, arguments
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.ReplaceAll(data, []byte("한"), []byte(`\ud55c`))
			request := httpTestRequest(t, "tools/call")
			request.Header.Set("Mcp-Name", tool)
			request.Body = io.NopCloser(bytes.NewReader(data))
			request.ContentLength = int64(len(data))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 || calls.Load() != 1 {
				t.Fatal("최대 입력이 전송 상한으로 거부", response.Code, response.Body)
			}
		})
	}
}

func TestMCPHTTPWireBodyLimit(t *testing.T) {
	for _, mode := range []string{"declared", "chunked"} {
		t.Run(mode, func(t *testing.T) {
			var authenticate, calls atomic.Int64
			handler, err := NewHandler(httpTestConfig(t, &authenticate, &calls))
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			request := httpTestRequest(t, "tools/call")
			request.URL, _ = url.Parse(server.URL + "/mcp")
			request.RequestURI = ""
			request.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", sdk.DefaultMaxRequestBodyBytes+1)))
			request.ContentLength = sdk.DefaultMaxRequestBodyBytes + 1
			if mode == "chunked" {
				request.ContentLength = -1
				request.TransferEncoding = []string{"chunked"}
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if response.StatusCode != 413 || json.Unmarshal(data, &value) != nil || authenticate.Load() != 0 || calls.Load() != 0 {
				t.Fatal("실제 전송 상한", response.StatusCode, string(data))
			}
		})
	}
}

func TestMCPHTTPInFlightCancellation(t *testing.T) {
	var authenticate, calls atomic.Int64
	config := httpTestConfig(t, &authenticate, &calls)
	entered, stopped := make(chan struct{}), make(chan struct{})
	config.Handlers["graph_list"] = func(ctx context.Context, _ Principal, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	handler, err := NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.ServeHTTP(httptest.NewRecorder(), httpTestRequest(t, "tools/call").WithContext(ctx))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("업무 처리기 진입 실패")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("요청 취소가 SDK 업무 context로 전달되지 않음")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("취소 뒤 전송 처리기가 반환되지 않음")
	}
}
