package mcp

import (
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newStrictSDKProbe(t *testing.T) *httptest.Server {
	t.Helper()
	return newSDKProbeWithTransport(t, func(server *sdk.Server) http.Handler {
		return newSDKTransport(server, sdk.DefaultMaxRequestBodyBytes)
	})
}

func TestStrictSDKIndependentRequests(t *testing.T) {
	for range 2 {
		server := newStrictSDKProbe(t)
		for position, method := range []string{"tools/call", "tools/list", "server/discover", "tools/call"} {
			body := sdkProbeRequest(method)
			if position == 0 {
				sdkProbeMeta(body)[sdk.MetaKeyClientInfo] = map[string]any{"name": "first-client", "version": "1"}
			}
			response := exchangeSDKProbe(t, server, http.MethodPost, body, nil)
			if response.Status != 200 {
				t.Fatalf("%s: %d %s", method, response.Status, response.Body)
			}
			result := decodeSDKProbe(t, response)["result"].(map[string]any)
			if result["resultType"] != "complete" {
				t.Fatalf("완료 외피: %s", response.Body)
			}
			if method != "tools/call" && (result["ttlMs"] != float64(0) || result["cacheScope"] != "private") {
				t.Fatalf("캐시 경계: %s", response.Body)
			}
			if method == "tools/call" {
				want := ""
				if position == 0 {
					want = "first-client"
				}
				if !reflect.DeepEqual(result["structuredContent"], map[string]any{"client": want}) {
					t.Fatalf("요청별 메타데이터 보존: %s", response.Body)
				}
			}
		}
	}
}

func TestStrictSDKClosesCompatibilityGaps(t *testing.T) {
	server := newStrictSDKProbe(t)
	for _, sample := range []struct {
		name    string
		change  func(map[string]any)
		headers map[string]string
		status  int
		code    float64
	}{
		{"legacy-initialize-no-envelope", func(b map[string]any) { b["method"] = "initialize"; delete(b["params"].(map[string]any), "_meta") }, map[string]string{"MCP-Protocol-Version": "", "Mcp-Method": ""}, 400, -32602},
		{"legacy-list-no-envelope", func(b map[string]any) { b["method"] = "tools/list"; delete(b["params"].(map[string]any), "_meta") }, map[string]string{"MCP-Protocol-Version": "", "Mcp-Method": ""}, 400, -32602},
		{"legacy-initialize-current-envelope", func(b map[string]any) { b["method"] = "initialize" }, nil, 404, -32601},
		{"old-revision", func(b map[string]any) { sdkProbeMeta(b)[sdk.MetaKeyProtocolVersion] = "2025-11-25" }, map[string]string{"MCP-Protocol-Version": "2025-11-25"}, 400, -32022},
		{"future-revision", func(b map[string]any) { sdkProbeMeta(b)[sdk.MetaKeyProtocolVersion] = "2099-01-01" }, map[string]string{"MCP-Protocol-Version": "2099-01-01"}, 400, -32022},
		{"missing-version-meta", func(b map[string]any) { delete(sdkProbeMeta(b), sdk.MetaKeyProtocolVersion) }, nil, 400, -32602},
		{"missing-capabilities", func(b map[string]any) { delete(sdkProbeMeta(b), sdk.MetaKeyClientCapabilities) }, nil, 400, -32602},
		{"null-capabilities", func(b map[string]any) { sdkProbeMeta(b)[sdk.MetaKeyClientCapabilities] = nil }, nil, 400, -32602},
		{"scalar-capabilities", func(b map[string]any) { sdkProbeMeta(b)[sdk.MetaKeyClientCapabilities] = "invalid" }, nil, 400, -32602},
		{"missing-version-header", nil, map[string]string{"MCP-Protocol-Version": ""}, 400, -32020},
		{"revision-mismatch", nil, map[string]string{"MCP-Protocol-Version": "2025-11-25"}, 400, -32020},
		{"missing-method-header", nil, map[string]string{"Mcp-Method": ""}, 400, -32020},
		{"method-case-mismatch", nil, map[string]string{"Mcp-Method": "Tools/Call"}, 400, -32020},
		{"missing-name-header", nil, map[string]string{"Mcp-Name": ""}, 400, -32020},
		{"name-case-mismatch", nil, map[string]string{"Mcp-Name": "Probe"}, 400, -32020},
		{"malformed-base64", nil, map[string]string{"Mcp-Name": "=?base64?%%%?="}, 400, -32020},
		{"wrong-base64-name", nil, map[string]string{"Mcp-Name": "=?base64?b3RoZXI=?="}, 400, -32020},
		{"noncanonical-base64", nil, map[string]string{"Mcp-Name": "=?base64?cHJvYmV=?="}, 400, -32020},
		{"invalid-utf8-base64", nil, map[string]string{"Mcp-Name": "=?base64?/w==?="}, 400, -32020},
		{"method-sentinel-is-not-name-encoding", nil, map[string]string{"Mcp-Method": "=?base64?dG9vbHMvY2FsbA==?="}, 400, -32020},
		{"no-subscriptions", func(b map[string]any) { b["method"] = "subscriptions/listen" }, nil, 404, -32601},
		{"no-ping", func(b map[string]any) { b["method"] = "ping" }, nil, 404, -32601},
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
			failure := decodeSDKProbe(t, response)["error"].(map[string]any)
			if failure["code"] != sample.code {
				t.Fatalf("기대 오류 %v: %s", sample.code, response.Body)
			}
			if sample.code == -32022 {
				data := failure["data"].(map[string]any)
				if !reflect.DeepEqual(data["supported"], []any{sdkProbeRevision}) || data["requested"] != sdkProbeMeta(body)[sdk.MetaKeyProtocolVersion] {
					t.Fatalf("revision 오류 정보: %s", response.Body)
				}
			}
		})
	}
	response := exchangeSDKProbe(t, server, http.MethodPost, sdkProbeRequest("tools/call"), map[string]string{"Mcp-Name": "=?base64?cHJvYmU=?="})
	if response.Status != 200 {
		t.Fatalf("Base64 해독 후 정상 호출: %d %s", response.Status, response.Body)
	}
	result := decodeSDKProbe(t, response)["result"].(map[string]any)
	if !reflect.DeepEqual(result["structuredContent"], map[string]any{"client": ""}) {
		t.Fatalf("해독 뒤 SDK 결과: %s", response.Body)
	}
}

func TestStrictSDKEnvelopeBoundaries(t *testing.T) {
	server := newStrictSDKProbe(t)
	for _, sample := range []struct {
		name string
		body string
	}{
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`},
		{"response", `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
		{"null-id", `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`},
		{"fractional-id", `{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}`},
		{"boolean-id", `{"jsonrpc":"2.0","id":true,"method":"tools/list"}`},
		{"duplicate-method", `{"jsonrpc":"2.0","id":1,"method":"tools/list","method":"initialize"}`},
		{"invalid-json", `{"jsonrpc":"2.0"`},
		{"two-objects", `{} {}`},
		{"null-request", `null`},
		{"scalar-request", `"request"`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			wantCode := -32600
			if sample.name == "duplicate-method" || sample.name == "invalid-json" || sample.name == "two-objects" {
				wantCode = -32700
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/mcp", strings.NewReader(sample.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var message struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if response.StatusCode != 400 || json.UnmarshalRead(response.Body, &message) != nil || message.Error.Code != wantCode {
				t.Fatalf("유효하지 않은 외피: %d %+v", response.StatusCode, message)
			}
		})
	}
}

func TestStrictSDKHeaderDuplicatesAndBodyLimit(t *testing.T) {
	server := newStrictSDKProbe(t)
	for _, name := range []string{"MCP-Protocol-Version", "Mcp-Method", "Mcp-Name"} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(sdkProbeRequest("tools/call"))
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/mcp", strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("MCP-Protocol-Version", sdkProbeRevision)
			request.Header.Set("Mcp-Method", "tools/call")
			request.Header.Set("Mcp-Name", "probe")
			request.Header.Add(name, request.Header.Get(name))
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var message struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if response.StatusCode != 400 || json.UnmarshalRead(response.Body, &message) != nil || message.Error.Code != -32020 {
				t.Fatalf("중복 헤더: %d %+v", response.StatusCode, message)
			}
		})
	}
	limited := newSDKProbeWithTransport(t, func(server *sdk.Server) http.Handler { return newSDKTransport(server, 32) })
	response := exchangeSDKProbe(t, limited, http.MethodPost, sdkProbeRequest("tools/call"), nil)
	if response.Status != 413 {
		t.Fatalf("본문 상한: %d %s", response.Status, response.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		response := exchangeSDKProbe(t, server, method, sdkProbeRequest("tools/list"), nil)
		if response.Status != 405 || response.Header.Get("Allow") != "POST" {
			t.Fatalf("메서드 경계: %d %s", response.Status, response.Header)
		}
	}
}

func TestTransportNameDecoding(t *testing.T) {
	for _, name := range []string{"probe", "한글", " padded ", "=?base64?literal?="} {
		t.Run(name, func(t *testing.T) {
			encoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(name)) + "?="
			decoded, ok := decodeTransportName(encoded)
			if !ok || decoded != name {
				t.Fatalf("이름 정규화: %q %v", decoded, ok)
			}
		})
	}
}

func TestStrictSDKEncodedToolNames(t *testing.T) {
	for _, name := range []string{"한글", "=?base64?literal?="} {
		t.Run(name, func(t *testing.T) {
			server := newSDKProbeWithTransport(t, func(server *sdk.Server) http.Handler {
				return newSDKTransport(server, sdk.DefaultMaxRequestBodyBytes)
			}, name)
			body := sdkProbeRequest("tools/call")
			body["params"].(map[string]any)["name"] = name
			sentinel := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(name)) + "?="
			response := exchangeSDKProbe(t, server, http.MethodPost, body, map[string]string{"Mcp-Name": sentinel})
			if response.Status != 200 || decodeSDKProbe(t, response)["result"].(map[string]any)["resultType"] != "complete" {
				t.Fatalf("인코딩한 이름의 실제 SDK 호출: %d %s", response.Status, response.Body)
			}
		})
	}
}

func TestTransportNormalizationPreservesOriginalHeader(t *testing.T) {
	server := newStrictSDKProbe(t)
	data, err := json.Marshal(sdkProbeRequest("tools/call"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", sdkProbeRevision)
	request.Header.Set("Mcp-Method", "tools/call")
	encoded := "=?base64?cHJvYmU=?="
	request.Header.Set("Mcp-Name", encoded)
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != 200 || request.Header.Get("Mcp-Name") != encoded {
		t.Fatalf("원본 헤더 보존: %d %s %s", recorder.Code, request.Header, recorder.Body)
	}
}
