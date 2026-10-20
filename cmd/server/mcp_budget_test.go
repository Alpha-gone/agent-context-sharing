package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcp08Authorization struct {
	principal authz.BearerPrincipal
	called    atomic.Int64
}

func (a *mcp08Authorization) ProtectBearer(next func(http.ResponseWriter, *http.Request, authz.BearerPrincipal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.called.Add(1); next(w, r, a.principal) })
}
func mcp08Handlers(run mcp.ToolHandler) map[string]mcp.ToolHandler {
	result := map[string]mcp.ToolHandler{}
	for _, name := range []string{"agent_register", "graph_list", "graph_create", "graph_get", "graph_update", "node_create", "node_get", "node_update", "node_discard", "node_restore", "context_flow_get", "relation_list", "relation_confirm", "relation_discard"} {
		result[name] = run
	}
	return result
}
func mcp08RequestBody(tool string, arguments string) string {
	return `{"jsonrpc":"2.0","id":"mcp08","method":"tools/call","params":{"name":"` + tool + `","arguments":` + arguments + `,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
}
func mcp08Request(t *testing.T, ctx context.Context, address, tool, args string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, "POST", address+"/mcp", strings.NewReader(mcp08RequestBody(tool, args)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", tool)
	return request
}
func TestMCP08RequestBudgetBoundaries(t *testing.T) {
	for _, mode := range []string{"own deadline", "earlier parent", "parent cancel", "existing response", "completed", "read deadline failure"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "earlier parent" {
				var stop context.CancelFunc
				parent, stop = context.WithTimeout(parent, 10*time.Millisecond)
				defer stop()
			}
			response := httptest.NewRecorder()
			var observed time.Time
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed, _ = r.Context().Deadline()
				if mode == "completed" {
					w.WriteHeader(204)
					return
				}
				if mode == "existing response" {
					w.WriteHeader(202)
					w.Write([]byte("preserved"))
				}
				if mode == "parent cancel" {
					cancel()
				}
				<-r.Context().Done()
			})
			var writer http.ResponseWriter = response
			if mode == "read deadline failure" {
				writer = mcp08ReadDeadlineFailure{response}
			}
			withMCPRequestBudget(next, 30*time.Millisecond).ServeHTTP(writer, httptest.NewRequest("POST", "https://service.test/mcp", nil).WithContext(parent))
			want := 500
			switch mode {
			case "parent cancel":
				want = 200
			case "existing response":
				want = 202
			case "completed":
				want = 204
			}
			if response.Code != want {
				t.Fatalf("요청 예산 응답: %d %s", response.Code, response.Body.String())
			}
			if mode != "read deadline failure" && observed.IsZero() {
				t.Fatal("마감이 전달되지 않음")
			}
			if mode == "earlier parent" {
				expected, _ := parent.Deadline()
				if !observed.Equal(expected) {
					t.Fatal("기존 마감 연장")
				}
			}
			if mode == "existing response" && response.Body.String() != "preserved" {
				t.Fatal("시작한 응답 덧쓰기")
			}
			if want == 500 && (response.Header().Get("WWW-Authenticate") != "" || response.Header().Get("Cache-Control") != "no-store" || response.Body.String() != `{"error":"server_error"}`) {
				t.Fatal("예산 소진이 재인가/내부 원문으로 변환")
			}
		})
	}
}

type mcp08ReadDeadlineFailure struct{ http.ResponseWriter }

func (mcp08ReadDeadlineFailure) SetReadDeadline(time.Time) error {
	return errors.New("private socket failure")
}

func TestMCP08ProductionBudgetReachesAuthorization(t *testing.T) {
	account, _ := model.NewID()
	authorization := &mcp08Authorization{principal: authz.BearerPrincipal{AccountID: account, ClientID: "first"}}
	var deadline time.Time
	handler, err := newMCPHandler(authorization, []string{"https://allowed.test"}, mcp08Handlers(func(ctx context.Context, _ mcp.Principal, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		deadline, _ = ctx.Deadline()
		return &sdk.CallToolResult{StructuredContent: map[string]any{"ok": true}, Content: []sdk.Content{&sdk.TextContent{Text: `{"ok":true}`}}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, mcp08Request(t, t.Context(), "https://service.test", "graph_list", "{}"))
	after := time.Now()
	if response.Code != 200 || deadline.Before(before.Add(mcpRequestTimeout)) || deadline.After(after.Add(mcpRequestTimeout)) || authorization.called.Load() != 1 {
		t.Fatal("기본 30초 예산/인증/도구 전달", response.Code, deadline)
	}
}
func TestMCP08SlowBodyUsesRequestDeadline(t *testing.T) {
	account, _ := model.NewID()
	authorization := &mcp08Authorization{principal: authz.BearerPrincipal{AccountID: account, ClientID: "first"}}
	handler, err := newMCPHandler(authorization, []string{"https://allowed.test"}, mcp08Handlers(func(context.Context, mcp.Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		t.Error("본문 수신 실패 뒤 도구 호출")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(withMCPRequestBudget(handler, 40*time.Millisecond))
	t.Cleanup(server.Close)
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nMCP-Protocol-Version: 2026-07-28\r\nMcp-Method: tools/call\r\nMcp-Name: graph_list\r\nContent-Length: 100\r\n\r\n{")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 500 || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("WWW-Authenticate") != "" || !strings.Contains(string(data), `"code":-32603`) || authorization.called.Load() != 0 {
		t.Fatal("본문 시간 초과의 계층/인증 차단", response.StatusCode, string(data), err)
	}
}
