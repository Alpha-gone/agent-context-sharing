package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const sdkProbeRevision = "2026-07-28"

// MCP-01의 최소 서버다. 업무 도구·인증·실제 서버 조립에 연결하지 않는다.
// SDK를 그대로 실행하여 서비스 계약과 다른 동작도 감추지 않고 관찰한다.
func newSDKProbe(t *testing.T) *httptest.Server {
	t.Helper()
	return newSDKProbeWithTransport(t, func(server *sdk.Server) http.Handler {
		return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{
			Stateless: true, JSONResponse: true,
		})
	})
}

func newSDKProbeWithTransport(t *testing.T, transport func(*sdk.Server) http.Handler, toolName ...string) *httptest.Server {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "mcp-01-probe", Version: "1"}, &sdk.ServerOptions{
		SupportedProtocolVersions: []string{sdkProbeRevision},
		Instructions:              "MCP-01 합성 적합성 시험",
		Capabilities:              &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
		SetCacheable: func(_ context.Context, _ sdk.Request, cache *sdk.Cacheable) {
			cache.TTLMs = 0
			cache.CacheScope = "private"
		},
	})
	type input struct{}
	type output struct {
		Client string `json:"client"`
	}
	name := "probe"
	if len(toolName) > 0 {
		name = toolName[0]
	}
	sdk.AddTool(server, &sdk.Tool{Name: name, Description: "합성 입력의 요청별 메타데이터 확인"},
		func(_ context.Context, request *sdk.CallToolRequest, _ input) (*sdk.CallToolResult, output, error) {
			name := ""
			if info := request.ClientInfo(); info != nil {
				name = info.Name
			}
			return nil, output{Client: name}, nil
		})
	mux := http.NewServeMux()
	mux.Handle("/mcp", transport(server))
	probe := httptest.NewServer(mux)
	t.Cleanup(probe.Close)
	return probe
}

func sdkProbeRequest(method string) map[string]any {
	params := map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    sdkProbeRevision,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
	}
	if method == "tools/call" {
		params["name"] = "probe"
		params["arguments"] = map[string]any{}
	}
	return map[string]any{"jsonrpc": "2.0", "id": "probe-1", "method": method, "params": params}
}

func sdkProbeMeta(request map[string]any) map[string]any {
	return request["params"].(map[string]any)["_meta"].(map[string]any)
}

type sdkProbeResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func exchangeSDKProbe(t *testing.T, server *httptest.Server, method string, body map[string]any, headers map[string]string) sdkProbeResponse {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, server.URL+"/mcp", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", sdkProbeRevision)
	request.Header.Set("Mcp-Method", body["method"].(string))
	if body["method"] == "tools/call" {
		request.Header.Set("Mcp-Name", "probe")
	}
	for name, value := range headers {
		if value == "" {
			request.Header.Del(name)
		} else {
			request.Header.Set(name, value)
		}
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Mcp-Session-Id") != "" {
		t.Fatal("무상태 응답에 세션 식별자가 있다")
	}
	return sdkProbeResponse{response.StatusCode, response.Header, data}
}

func decodeSDKProbe(t *testing.T, response sdkProbeResponse) map[string]any {
	t.Helper()
	if response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("JSON 응답이 아니다: %d %s %s", response.Status, response.Header, response.Body)
	}
	var message map[string]any
	if err := json.Unmarshal(response.Body, &message); err != nil {
		t.Fatal(err)
	}
	if message["jsonrpc"] != "2.0" || message["id"] != "probe-1" {
		t.Fatalf("JSON-RPC 외피 불일치: %s", response.Body)
	}
	return message
}

func TestSDKIndependentRequests(t *testing.T) {
	first, second := newSDKProbe(t), newSDKProbe(t)
	// 두 인스턴스 모두 발견·initialize 없이 첫 요청부터 도구를 처리한다.
	for _, server := range []*httptest.Server{first, second} {
		for _, client := range []string{"first-client", "second-client", ""} {
			body := sdkProbeRequest("tools/call")
			if client != "" {
				sdkProbeMeta(body)["io.modelcontextprotocol/clientInfo"] = map[string]any{"name": client, "version": "1"}
			}
			response := exchangeSDKProbe(t, server, http.MethodPost, body, nil)
			if response.Status != http.StatusOK {
				t.Fatalf("도구 호출: %d %s", response.Status, response.Body)
			}
			result := decodeSDKProbe(t, response)["result"].(map[string]any)
			want := map[string]any{"client": client}
			if result["resultType"] != "complete" || !reflect.DeepEqual(result["structuredContent"], want) || result["isError"] == true {
				t.Fatalf("완료 결과 불일치: %s", response.Body)
			}
			content := result["content"].([]any)
			var textValue map[string]any
			text := content[0].(map[string]any)
			if text["type"] != "text" || json.Unmarshal([]byte(text["text"].(string)), &textValue) != nil || !reflect.DeepEqual(textValue, want) {
				t.Fatalf("구조화 결과와 text가 다르다: %s", response.Body)
			}
		}
	}
}

func TestSDKDiscoveryAndList(t *testing.T) {
	server := newSDKProbe(t)
	for _, method := range []string{"tools/list", "server/discover", "tools/list"} {
		t.Run(method, func(t *testing.T) {
			response := exchangeSDKProbe(t, server, http.MethodPost, sdkProbeRequest(method), nil)
			if response.Status != http.StatusOK {
				t.Fatalf("%d %s", response.Status, response.Body)
			}
			result := decodeSDKProbe(t, response)["result"].(map[string]any)
			if result["resultType"] != "complete" || result["ttlMs"] != float64(0) || result["cacheScope"] != "private" {
				t.Fatalf("완료·캐시 계약 불일치: %s", response.Body)
			}
			if method == "server/discover" {
				if !reflect.DeepEqual(result["supportedVersions"], []any{sdkProbeRevision}) || result["instructions"] != "MCP-01 합성 적합성 시험" {
					t.Fatalf("발견 계약 불일치: %s", response.Body)
				}
				if !reflect.DeepEqual(result["capabilities"], map[string]any{"tools": map[string]any{}}) {
					t.Fatalf("허용하지 않은 기능 광고: %s", response.Body)
				}
				info := result["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any)
				if info["name"] != "mcp-01-probe" || info["version"] != "1" {
					t.Fatalf("서버 정보 불일치: %s", response.Body)
				}
			} else {
				tools := result["tools"].([]any)
				if len(tools) != 1 || tools[0].(map[string]any)["name"] != "probe" {
					t.Fatalf("합성 도구 목록 불일치: %s", response.Body)
				}
			}
		})
	}
}

func TestSDKRequestRejections(t *testing.T) {
	server := newSDKProbe(t)
	for _, sample := range []struct {
		name    string
		change  func(map[string]any)
		headers map[string]string
		status  int
		code    float64
	}{
		{"missing-version-meta", func(b map[string]any) { delete(sdkProbeMeta(b), "io.modelcontextprotocol/protocolVersion") }, nil, 400, -32602},
		{"missing-capabilities", func(b map[string]any) { delete(sdkProbeMeta(b), "io.modelcontextprotocol/clientCapabilities") }, nil, 400, -32602},
		{"missing-meta", func(b map[string]any) { delete(b["params"].(map[string]any), "_meta") }, nil, 400, -32602},
		{"missing-version-header", nil, map[string]string{"MCP-Protocol-Version": ""}, 400, -32020},
		{"missing-method-header", nil, map[string]string{"Mcp-Method": ""}, 400, -32020},
		{"wrong-method", nil, map[string]string{"Mcp-Method": "tools/list"}, 400, -32020},
		{"missing-name", nil, map[string]string{"Mcp-Name": ""}, 400, -32020},
		{"wrong-name", nil, map[string]string{"Mcp-Name": "other"}, 400, -32020},
		{"future-revision", func(b map[string]any) { sdkProbeMeta(b)["io.modelcontextprotocol/protocolVersion"] = "2099-01-01" }, map[string]string{"MCP-Protocol-Version": "2099-01-01"}, 400, -32022},
		{"unknown-method", func(b map[string]any) { b["method"] = "unknown/method" }, nil, 404, -32601},
	} {
		t.Run(sample.name, func(t *testing.T) {
			body := sdkProbeRequest("tools/call")
			if sample.change != nil {
				sample.change(body)
			}
			response := exchangeSDKProbe(t, server, http.MethodPost, body, sample.headers)
			if response.Status != sample.status {
				t.Fatalf("HTTP %d, 기대 %d: %s", response.Status, sample.status, response.Body)
			}
			message := decodeSDKProbe(t, response)
			failure, ok := message["error"].(map[string]any)
			if !ok || failure["code"] != sample.code {
				t.Fatalf("기대 오류 %v: %s", sample.code, response.Body)
			}
			if sample.code == -32022 {
				data := failure["data"].(map[string]any)
				if data["requested"] != "2099-01-01" || !reflect.DeepEqual(data["supported"], []any{sdkProbeRevision}) {
					t.Fatalf("미지원 revision 정보: %s", response.Body)
				}
			}
		})
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			response := exchangeSDKProbe(t, server, method, sdkProbeRequest("tools/list"), nil)
			if response.Status != 405 || response.Header.Get("Allow") != "POST" {
				t.Fatalf("무상태 메서드 경계: %d %s", response.Status, response.Header)
			}
		})
	}
}

// 공급자 동작의 관찰 시험이다. 통과를 서비스 계약 충족으로 집계하지 않는다.
// MCP-02의 엄격한 경계가 구현되면 해당 경계의 목표 계약 시험을 따로 수행한다.
func TestSDKCompatibilityGaps(t *testing.T) {
	server := newSDKProbe(t)
	for _, sample := range []struct {
		name    string
		body    map[string]any
		headers map[string]string
		status  int
		code    float64
	}{
		{"legacy-initialize", map[string]any{"jsonrpc": "2.0", "id": "probe-1", "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "legacy", "version": "1"}}}, map[string]string{"MCP-Protocol-Version": "", "Mcp-Method": ""}, 200, 0},
		{"legacy-list-without-envelope", map[string]any{"jsonrpc": "2.0", "id": "probe-1", "method": "tools/list", "params": map[string]any{}}, map[string]string{"MCP-Protocol-Version": "", "Mcp-Method": ""}, 200, 0},
		{"old-version-plaintext", sdkProbeRequest("tools/call"), map[string]string{"MCP-Protocol-Version": "2025-11-25"}, 400, 0},
		{"base64-method", sdkProbeRequest("tools/call"), map[string]string{"Mcp-Method": "=?base64?" + base64.StdEncoding.EncodeToString([]byte("tools/call")) + "?="}, 400, -32020},
		{"base64-name", sdkProbeRequest("tools/call"), map[string]string{"Mcp-Name": "=?base64?" + base64.StdEncoding.EncodeToString([]byte("probe")) + "?="}, 400, -32020},
	} {
		t.Run(sample.name, func(t *testing.T) {
			response := exchangeSDKProbe(t, server, http.MethodPost, sample.body, sample.headers)
			if response.Status != sample.status {
				t.Fatalf("SDK 동작 변경: %d %s", response.Status, response.Body)
			}
			if sample.name == "old-version-plaintext" {
				if response.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
					t.Fatalf("SDK 동작 변경: %s %s", response.Header, response.Body)
				}
			} else {
				message := decodeSDKProbe(t, response)
				if sample.code != 0 && message["error"].(map[string]any)["code"] != sample.code {
					t.Fatalf("SDK 오류 변경: %s", response.Body)
				}
				if sample.name == "legacy-initialize" && message["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
					t.Fatalf("SDK 이전 revision 협상 변경: %s", response.Body)
				}
			}
			t.Logf("서비스 계약과 다른 SDK 동작: HTTP %d %s", response.Status, response.Body)
		})
	}
}
