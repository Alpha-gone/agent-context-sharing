package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type test07LostResponse struct {
	http.ResponseWriter
	committed, dropped *atomic.Bool
}

func (w test07LostResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w test07LostResponse) Write(data []byte) (int, error) {
	if w.committed.Load() && w.dropped.CompareAndSwap(false, true) {
		conn, _, err := http.NewResponseController(w.ResponseWriter).Hijack()
		if err != nil {
			return 0, err
		}
		_ = conn.Close()
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(data)
}

// 요청의 업무 커밋 뒤 첫 응답 본문을 쓰는 지점에서 실제 TLS 연결을 닫는다.
// 다른 저장소 인스턴스의 같은 키 재생이 최초 판을 돌려주고 재적용하지 않아야 한다.
func TestTEST07CommittedWriteAfterTLSResponseLossIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	var committed, dropped atomic.Bool
	servers := make([]*httptest.Server, 2)
	for index, writes := range []map[string]mcp.ToolHandler{f.handlers, f.other} {
		handlers := mcp08Handlers(func(context.Context, mcp.Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			t.Error("응답 유실 시나리오 밖의 업무 호출")
			return nil, io.ErrUnexpectedEOF
		})
		write := writes["graph_update"]
		handlers["graph_update"] = func(ctx context.Context, p mcp.Principal, r *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			result, err := write(ctx, p, r)
			if err == nil && result != nil && !result.IsError {
				committed.Store(true)
			}
			return result, err
		}
		authorization := &mcp08Authorization{principal: authz.BearerPrincipal{AccountID: f.principal.AccountID, ClientID: f.principal.ClientID}}
		handler, err := newMCPHandler(authorization, []string{"https://allowed.test"}, handlers)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			next := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(test07LostResponse{ResponseWriter: w, committed: &committed, dropped: &dropped}, r)
			})
		}
		servers[index] = httptest.NewTLSServer(handler)
		t.Cleanup(servers[index].Close)
		servers[index].Client().Timeout = 5 * time.Second
		t.Cleanup(servers[index].Client().CloseIdleConnections)
	}
	before, err := f.database.Graph(t.Context(), f.graph)
	if err != nil {
		t.Fatal(err)
	}
	args := f.args("graph_update")
	args["expected_version"], args["name"], args["request_id"] = before.Version, "TEST-07 응답 유실", "test07_response_loss"
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	request := mcp08Request(t, t.Context(), servers[0].URL, "graph_update", string(encoded))
	response, transportErr := servers[0].Client().Do(request)
	if response != nil {
		_, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if transportErr == nil {
			transportErr = readErr
		}
	}
	if transportErr == nil || !committed.Load() || !dropped.Load() {
		t.Fatal("업무 커밋 뒤 실제 응답 유실을 관찰하지 못함")
	}
	stored, err := f.database.Graph(t.Context(), f.graph)
	if err != nil || stored.Version != before.Version+1 || stored.Name != "TEST-07 응답 유실" {
		t.Fatal("응답 유실이 커밋된 그래프를 되돌림", err)
	}
	fingerprint := mcpWriteFingerprint(t, f.pool)
	for range 2 {
		request := mcp08Request(t, t.Context(), servers[1].URL, "graph_update", string(encoded))
		response, err := servers[1].Client().Do(request)
		if err != nil {
			t.Fatal("다른 인스턴스의 재생 요청 실패", err)
		}
		var wire struct {
			Result *sdk.CallToolResult `json:"result"`
		}
		err = json.UnmarshalRead(response.Body, &wire)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || response.Header.Get("WWW-Authenticate") != "" || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("응답 유실 뒤 재생의 전송 계층", err)
		}
		value := writeContent(t, wire.Result, nil, "")
		graph := value["graph"].(map[string]any)
		if graph["name"] != stored.Name || graph["version"] != float64(stored.Version) || fingerprint != mcpWriteFingerprint(t, f.pool) {
			t.Fatal("재생이 최초 결과를 잃거나 판·감사·큐·저장량·기록을 재적용함")
		}
	}
}
