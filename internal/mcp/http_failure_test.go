package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCP08ToolErrorBoundary(t *testing.T) {
	for _, sample := range []struct {
		name        string
		err         error
		code, field string
		partial     bool
	}{
		{name: "raw error", err: errors.New("private SQL password"), code: "internal"},
		{name: "SDK exception", err: fmt.Errorf("private RPC error"), code: "internal"},
		{name: "field", err: model.FieldError{Field: "body", Message: "private input"}, code: "invalid_argument", field: "field"},
		{name: "grade", err: perm.DeniedError{Required: model.GraphGradeEditor}, code: "permission_denied", field: "required_grade"},
		{name: "hidden", err: perm.NotFoundError{}, code: "not_found"},
		{name: "version", err: model.VersionConflictError{Current: 9007199254740993}, code: "version_conflict", field: "current_version"},
		{name: "limit", err: plan.LimitError{Name: "context_budget", Current: 9007199254740993, Allowed: 10}, code: "limit_exceeded", field: "limit"},
		{name: "web", err: RestoreChannelError{Cause: errors.New("private channel")}, code: "not_supported", field: "alternative_channel"},
		{name: "storage field", err: &DependencyFailure{Cause: model.FieldError{Field: "private", Message: "private db"}}, code: "internal"},
		{name: "cleanup overrides absence", err: errors.Join(model.ErrNotFound, &DependencyFailure{Cause: errors.New("private cleanup")}), code: "internal"},
		{name: "cancel", err: context.Canceled, code: "internal"},
		{name: "timeout", err: context.DeadlineExceeded, code: "internal"},
		{name: "nil result", code: "internal"},
		{name: "normal partial", partial: true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			account, _ := model.NewID()
			p := Principal{AccountID: account, ClientID: "first"}
			handlers := map[string]ToolHandler{}
			for _, tool := range toolDefinitions() {
				handlers[tool.Name] = func(context.Context, Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					if sample.partial {
						return writeResult([]byte(`{"contexts":[],"truncated":{"result_truncated":true,"hop_boundary":0},"channels":{"semantic":{"failure":"embedding_unavailable"}}}`))
					}
					return nil, sample.err
				}
			}
			handler, err := NewHandler(HTTPConfig{AllowedOrigins: []string{"https://allowed.test"}, Handlers: handlers, Protect: func(next AuthenticatedHandler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next(w, r, p) })
			}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			body := sdkProbeRequest("tools/call")
			body["params"].(map[string]any)["name"] = "graph_list"
			response := exchangeSDKProbe(t, server, "POST", body, map[string]string{"Mcp-Name": "graph_list"})
			wire := decodeSDKProbe(t, response)
			result, ok := wire["result"].(map[string]any)
			if response.Status != 200 || !ok || wire["error"] != nil || result["resultType"] != "complete" || response.Header.Get("WWW-Authenticate") != "" || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("오류 계층: %s", response.Body)
			}
			content := result["structuredContent"].(map[string]any)
			if sample.partial {
				if result["isError"] == true || content["code"] != nil {
					t.Fatal("정상 부분 결과를 오류 처리")
				}
			} else if result["isError"] != true || content["code"] != sample.code || sample.field != "" && content[sample.field] == nil {
				t.Fatalf("부가 정보/도메인 오류: %s", response.Body)
			}
			if sample.name == "grade" && content["required_grade"] != "editor" || sample.name == "web" && content["alternative_channel"] != "web" || sample.name == "field" && content["field"] != "body" {
				t.Fatal("원래 오류 값 손실")
			}
			if (sample.name == "version" || sample.name == "limit") && !strings.Contains(string(response.Body), "9007199254740993") {
				t.Fatal("큰 정수 부가 정보 손실")
			}
			if strings.Contains(string(response.Body), "private") {
				t.Fatal("내부 원문 노출")
			}
		})
	}
}

func TestMCP08IndependentInstancesAndAuthenticationCaches(t *testing.T) {
	var authenticated atomic.Int64
	handlers := map[string]ToolHandler{}
	for _, tool := range toolDefinitions() {
		handlers[tool.Name] = func(_ context.Context, p Principal, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			data, _ := json.Marshal(map[string]any{"account": p.AccountID.String(), "client": p.ClientID})
			return writeResult(data)
		}
	}
	accounts := make([]model.ID, 2)
	for i := range accounts {
		accounts[i], _ = model.NewID()
	}
	servers := make([]*httptest.Server, 2)
	for i := range servers {
		handler, err := NewHandler(HTTPConfig{AllowedOrigins: []string{"https://allowed.test"}, Handlers: handlers, Protect: func(next AuthenticatedHandler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authenticated.Add(1)
				index := 0
				if r.Header.Get("Authorization") == "Bearer second" {
					index = 1
				} else if r.Header.Get("Authorization") != "Bearer first" {
					w.WriteHeader(401)
					return
				}
				next(w, r, Principal{AccountID: accounts[index], ClientID: []string{"first", "second"}[index]})
			})
		}})
		if err != nil {
			t.Fatal(err)
		}
		servers[i] = httptest.NewServer(handler)
		t.Cleanup(servers[i].Close)
	}
	var group sync.WaitGroup
	for i := range 24 {
		group.Go(func() {
			who := i % 2
			method := []string{"server/discover", "tools/list", "tools/call"}[i%3]
			body := sdkProbeRequest(method)
			if method == "tools/call" {
				body["params"].(map[string]any)["name"] = "graph_list"
			}
			body["params"].(map[string]any)["_meta"].(map[string]any)[sdk.MetaKeyClientInfo] = map[string]any{"name": "forged", "version": "1"}
			response := exchangeSDKProbe(t, servers[(i/2)%2], "POST", body, map[string]string{"Mcp-Name": "graph_list", "Authorization": "Bearer " + []string{"first", "second"}[who]})
			wire := decodeSDKProbe(t, response)
			result := wire["result"].(map[string]any)
			if result["resultType"] != "complete" || response.Header.Get("Cache-Control") != "no-store" {
				t.Error("표준 완료/HTTP 캐시")
			}
			if method == "tools/call" {
				value := result["structuredContent"].(map[string]any)
				if value["account"] != accounts[who].String() || value["client"] != []string{"first", "second"}[who] {
					t.Error("인증 맥락 혼합")
				}
			} else if result["ttlMs"] != float64(0) || result["cacheScope"] != "private" {
				t.Error("공유 캐시 허용")
			}
		})
	}
	group.Wait()
	for _, server := range servers {
		response := exchangeSDKProbe(t, server, "POST", sdkProbeRequest("tools/list"), map[string]string{"Authorization": "Bearer revoked"})
		if response.Status != 401 || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("발견/목록 결과가 현재 인증 대체")
		}
	}
	if authenticated.Load() != 26 {
		t.Fatal("이전 발견/다른 인스턴스 결과로 인증 생략", authenticated.Load())
	}
}
