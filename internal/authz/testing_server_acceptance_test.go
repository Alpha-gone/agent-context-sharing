package authz

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 실제 서명 검증·TLS·SDK는 하나의 서버를 공유한다. 의존 장애와 업무 결과만 주입해
// 직전 응답·발견 캐시가 다음 요청의 인증이나 오류 계층을 바꾸지 않는지 검사한다.
func TestTEST07SameTLSServerFailureLayers(t *testing.T) {
	service, claims, sign := bearerFixture(t)
	valid := sign(claims)
	scopeClaims := maps.Clone(claims)
	scopeClaims["scope"] = "other"
	scope := sign(scopeClaims)
	type scenario struct {
		name, mode, credential, rpcMethod string
		status, rpcCode                   int
		failure                           error
		code, field, value                string
		partial                           bool
	}
	cases := []scenario{
		{name: "direct call before discovery", status: 200},
		{name: "discover", rpcMethod: "server/discover", status: 200},
		{name: "list", rpcMethod: "tools/list", status: 200},
		{name: "missing bearer after discovery", credential: "absent", status: 401},
		{name: "invalid bearer", credential: "invalid", status: 401},
		{name: "scope", credential: "scope", status: 403},
		{name: "authentication storage outage", mode: "outage", status: 500},
		{name: "recovered authentication", status: 200},
		{name: "revoked bearer after success", mode: "revoked", status: 401},
		{name: "parse error", mode: "malformed", status: 400, rpcCode: -32700},
		{name: "missing method header", mode: "header", status: 400, rpcCode: -32020},
		{name: "unsupported revision", mode: "revision", status: 400, rpcCode: -32022},
		{name: "unsupported rpc", rpcMethod: "ping", status: 404, rpcCode: -32601},
		{name: "GET", mode: "GET", status: 405},
		{name: "DELETE", mode: "DELETE", status: 405},
		{name: "invalid argument", failure: model.FieldError{Field: "body", Message: "private input"}, status: 200, code: "invalid_argument", field: "field", value: "body"},
		{name: "permission denied", failure: perm.DeniedError{Required: model.GraphGradeEditor}, status: 200, code: "permission_denied", field: "required_grade", value: "editor"},
		{name: "not found", failure: model.ErrNotFound, status: 200, code: "not_found"},
		{name: "version conflict", failure: model.VersionConflictError{Current: 9007199254740993}, status: 200, code: "version_conflict", field: "current_version", value: "9007199254740993"},
		{name: "limit exceeded", failure: plan.LimitError{Name: "context_budget", Current: 12, Allowed: 10}, status: 200, code: "limit_exceeded", field: "limit", value: "context_budget"},
		{name: "not supported", failure: mcp.RestoreChannelError{}, status: 200, code: "not_supported", field: "alternative_channel", value: "web"},
		{name: "internal tool failure", failure: errors.New("private SQL failure"), status: 200, code: "internal"},
		{name: "normal truncated partial", partial: true, status: 200},
		{name: "success after all failures", status: 200},
	}
	var current, calls atomic.Int64
	service.config.AccessTokens = accessStoreFunc(func(context.Context, store.OAuthAccessBinding, time.Time) (bool, error) {
		switch cases[current.Load()].mode {
		case "outage":
			return false, errors.New("private authentication storage")
		case "revoked":
			return false, nil
		default:
			return true, nil
		}
	})
	authorization, err := NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	handlers := map[string]mcp.ToolHandler{}
	for _, name := range []string{"agent_register", "graph_list", "graph_create", "graph_get", "graph_update", "node_create", "node_get", "node_update", "node_discard", "node_restore", "context_flow_get", "relation_list", "relation_confirm", "relation_discard"} {
		handlers[name] = func(_ context.Context, principal mcp.Principal, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			if principal.AccountID.String() != claims["sub"] || principal.ClientID != "first" {
				t.Error("검증한 Bearer와 다른 업무 주체")
			}
			sample := cases[current.Load()]
			if sample.failure != nil {
				return nil, sample.failure
			}
			value := map[string]any{"ok": true}
			if sample.partial {
				value = map[string]any{"contexts": []any{}, "truncated": map[string]any{"result_truncated": true, "hop_boundary": 0}, "channels": map[string]any{"semantic": map[string]any{"failure": "embedding_unavailable"}}}
			}
			data, err := json.Marshal(value)
			return &sdk.CallToolResult{StructuredContent: value, Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}}, err
		}
	}
	handler, err := mcp.NewHandler(mcp.HTTPConfig{AllowedOrigins: []string{"https://allowed.test"}, Handlers: handlers, Protect: func(next mcp.AuthenticatedHandler) http.Handler {
		return authorization.ProtectBearer(func(w http.ResponseWriter, r *http.Request, p BearerPrincipal) {
			next(w, r, mcp.Principal{AccountID: p.AccountID, ClientID: p.ClientID})
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	for index, sample := range cases {
		t.Run(sample.name, func(t *testing.T) {
			current.Store(int64(index))
			before := calls.Load()
			method := sample.rpcMethod
			if method == "" {
				method = "tools/call"
			}
			revision := "2026-07-28"
			if sample.mode == "revision" {
				revision = "2025-11-25"
			}
			params := map[string]any{"_meta": map[string]any{sdk.MetaKeyProtocolVersion: revision, sdk.MetaKeyClientCapabilities: map[string]any{}}}
			if method == "tools/call" {
				params["name"], params["arguments"] = "graph_list", map[string]any{}
			}
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "test07", "method": method, "params": params})
			if err != nil {
				t.Fatal(err)
			}
			if sample.mode == "malformed" {
				body = []byte(`{"private":`)
			}
			httpMethod := http.MethodPost
			if sample.status == 405 {
				httpMethod = sample.mode
			}
			request, err := http.NewRequestWithContext(t.Context(), httpMethod, server.URL+"/mcp", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("MCP-Protocol-Version", revision)
			request.Header.Set("Mcp-Method", method)
			if method == "tools/call" {
				request.Header.Set("Mcp-Name", "graph_list")
			}
			if sample.mode == "header" {
				request.Header.Del("Mcp-Method")
			}
			token := valid
			if sample.credential == "scope" {
				token = scope
			} else if sample.credential == "invalid" {
				token = "invalid-token"
			}
			if sample.credential != "absent" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("실제 TLS 요청 실패", err)
			}
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != sample.status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Mcp-Session-Id") != "" || response.Header.Get("Location") != "" || len(response.Cookies()) != 0 {
				t.Fatalf("HTTP 계층: status=%d body=%s err=%v", response.StatusCode, data, err)
			}
			challenge := response.Header.Get("WWW-Authenticate")
			if (sample.status == 401 || sample.status == 403) != (challenge != "") {
				t.Fatal("오류 계층이 재인가 신호와 혼합됨")
			}
			for _, secret := range []string{valid, scope, "private input", "private SQL failure", "private authentication storage"} {
				if strings.Contains(string(data)+challenge, secret) {
					t.Fatal("자격 또는 내부 원문 노출")
				}
			}
			wantCalls := int64(0)
			if sample.status == 200 && method == "tools/call" {
				wantCalls = 1
			}
			if calls.Load()-before != wantCalls {
				t.Fatal("거부 요청이 업무에 진입하거나 업무를 중복 실행함")
			}
			if sample.status == 405 {
				if response.Header.Get("Allow") != "POST" || len(data) != 0 {
					t.Fatal("GET/DELETE 거부 형식")
				}
				return
			}
			if sample.status == 401 || sample.status == 403 || sample.status == 500 {
				var failure map[string]string
				if json.Unmarshal(data, &failure) != nil || failure["error"] == "" || sample.status == 500 && failure["error"] != "server_error" {
					t.Fatal("인증 오류 외피")
				}
				if sample.status == 401 && !strings.Contains(challenge, `resource_metadata="https://resource.test/.well-known/oauth-protected-resource/mcp"`) {
					t.Fatal("보호 리소스 메타데이터 위치")
				}
				return
			}
			var wire struct {
				JSONRPC string `json:"jsonrpc"`
				ID      string `json:"id"`
				Error   *struct {
					Code int `json:"code"`
				} `json:"error"`
				Result *struct {
					ResultType        string           `json:"resultType"`
					IsError           bool             `json:"isError"`
					TTLMs             int              `json:"ttlMs"`
					CacheScope        string           `json:"cacheScope"`
					Tools             []map[string]any `json:"tools"`
					StructuredContent map[string]any   `json:"structuredContent"`
					Content           []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			if json.Unmarshal(data, &wire) != nil || wire.JSONRPC != "2.0" {
				t.Fatalf("JSON-RPC 해석: %s", data)
			}
			if sample.rpcCode != 0 {
				if wire.Error == nil || wire.Error.Code != sample.rpcCode || wire.Result != nil {
					t.Fatalf("RPC 오류가 다른 계층으로 변환됨: %s", data)
				}
				return
			}
			result := wire.Result
			if wire.ID != "test07" || wire.Error != nil || result == nil || result.ResultType != "complete" {
				t.Fatalf("표준 완료 결과: %s", data)
			}
			if method != "tools/call" {
				if result.TTLMs != 0 || result.CacheScope != "private" || method == "tools/list" && len(result.Tools) != 14 {
					t.Fatal("발견·14종 목록의 캐시 계약")
				}
				return
			}
			content := result.StructuredContent
			if result.IsError != (sample.code != "") || sample.code != "" && content["code"] != sample.code || sample.code == "" && content["code"] != nil {
				t.Fatalf("도메인 완료 결과: %s", data)
			}
			if sample.field != "" {
				if sample.field == "current_version" {
					if !bytes.Contains(data, []byte(sample.value)) {
						t.Fatal("큰 판 번호의 정밀도 손실")
					}
				} else if content[sample.field] != sample.value {
					t.Fatal("도메인 부가 정보 손실")
				}
			}
			if sample.partial && (content["truncated"].(map[string]any)["result_truncated"] != true || content["channels"].(map[string]any)["semantic"].(map[string]any)["failure"] != "embedding_unavailable") {
				t.Fatal("정상 부분 상태 손실")
			}
			var text map[string]any
			if len(result.Content) != 1 || result.Content[0].Type != "text" || json.Unmarshal([]byte(result.Content[0].Text), &text) != nil || !reflect.DeepEqual(text, content) {
				t.Fatal("text·structuredContent 불일치")
			}
		})
	}
}
