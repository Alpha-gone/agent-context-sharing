package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
	"agent_context_sharing/client/internal/client/host"
)

func TestHostPreparationSharesRetryBudgetAndSafeLogs(t *testing.T) {
	fixture := newCatalogSource(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.MessageKey {
			return slog.Attr{}
		}
		return a
	}}))
	listAttempts, callAttempts := 0, 0
	c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		switch r.Header.Get("Mcp-Method") {
		case "server/discover":
			return rpcResponse(id, fixture.discovery), nil
		case "tools/list":
			listAttempts++
			if listAttempts == 1 {
				return nil, io.EOF
			}
			return rpcResponse(id, fixture.tools), nil
		default:
			callAttempts++
			return nil, io.EOF
		}
	}), HTTPOptions{Logger: logger})
	policy, _ := contract.NewPolicy("all", nil)
	relay := host.NewTools(c, policy, uuid.NewV7())
	if _, err := relay.CallTool(t.Context(), "graph_create", jsontext.Value(`{"name":"private-body"}`)); !errors.Is(err, contract.ErrIndeterminate) || listAttempts != 2 || callAttempts != 3 {
		t.Fatalf("목록/도구 공통 예산: %v lists=%d calls=%d", err, listAttempts, callAttempts)
	}
	allowed := map[string]bool{"time": true, "level": true, "event": true, "correlation_id": true, "tool": true, "outcome": true, "duration_ms": true, "request_bytes": true, "response_bytes": true, "retry_index": true}
	correlations := make(map[string]bool)
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte{'\n'}) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		for key := range record {
			if !allowed[key] {
				t.Fatalf("로그 필드 유출: %s", key)
			}
		}
		correlations[record["correlation_id"].(string)] = true
	}
	if len(correlations) != 1 || strings.Contains(logs.String(), "private-body") || strings.Contains(logs.String(), "resource.test") {
		t.Fatal("상관 식별자 혼선·비밀 로그")
	}
}

func TestDeliveredWriteCancellationIsIndeterminate(t *testing.T) {
	for _, tool := range []string{"graph_list", "graph_create"} {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
			wireID(t, r)
			cancel()
			return nil, io.EOF
		}), HTTPOptions{})
		_, err := executeTest(ctx, c, tool)
		want := error(context.Canceled)
		if tool == "graph_create" {
			want = contract.ErrIndeterminate
		}
		if !errors.Is(err, want) {
			t.Fatalf("전달 뒤 %s 취소: %v", tool, err)
		}
	}
}

func TestInvalidRemoteCatalogBecomesStdioClientProtocolWithoutPartialTools(t *testing.T) {
	for _, mutation := range []string{"missing", "extra", "schema"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := newCatalogSource(t)
			var tools []contract.Tool
			if err := json.Unmarshal(contract.Manifest(), &tools); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "missing":
				tools = tools[:len(tools)-1]
			case "extra":
				tools = append(tools, contract.Tool{Name: "audit", Description: "내부 도구", InputSchema: jsontext.Value(`{"type":"object"}`)})
			case "schema":
				tools[0].InputSchema = jsontext.Value(`{"type":"object"}`)
			}
			badList, _ := json.Marshal(map[string]any{"tools": tools})
			c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
				id := wireID(t, r)
				if r.Header.Get("Mcp-Method") == "server/discover" {
					return rpcResponse(id, fixture.discovery), nil
				}
				return rpcResponse(id, badList), nil
			}), HTTPOptions{})
			policy, _ := contract.NewPolicy("all", nil)
			line := stdioExchange(t, host.NewTools(c, policy, uuid.NewV7()), `{"jsonrpc":"2.0","id":"original-host","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
			if !strings.Contains(line, `"code":"client_protocol"`) || !strings.Contains(line, `"id":"original-host"`) || strings.Contains(line, `"tools"`) || !strings.Contains(line, `"error":`) || !strings.Contains(line, `"code":-32603`) {
				t.Fatalf("부분 목록·오류 계약: %s", line)
			}
		})
	}
}

func TestClosedClientAndNewProcessDoNotReuseWriteKey(t *testing.T) {
	fixture := newCatalogSource(t)
	var keys []string
	create := func() *Client {
		return newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
			id := wireID(t, r)
			switch r.Header.Get("Mcp-Method") {
			case "server/discover":
				return rpcResponse(id, fixture.discovery), nil
			case "tools/list":
				return rpcResponse(id, fixture.tools), nil
			default:
				keys = append(keys, r.Header.Get("Idempotency-Key"))
				return rpcResponse(id, jsontext.Value(`{"content":[],"isError":false}`)), nil
			}
		}), HTTPOptions{})
	}
	old := create()
	if _, err := old.CallTool(t.Context(), "graph_create", jsontext.Value(`{"name":"쓰기"}`)); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if _, err := old.CallTool(t.Context(), "graph_create", jsontext.Value(`{"name":"쓰기"}`)); !errors.Is(err, context.Canceled) {
		t.Fatal("종료한 클라이언트 재사용")
	}
	fresh := create()
	if _, err := fresh.CallTool(t.Context(), "graph_create", jsontext.Value(`{"name":"쓰기"}`)); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] == keys[1] {
		t.Fatal("새 프로세스 쓰기 키 재사용")
	}
}

func TestRemoteProtocolErrorPreservedThroughActualStdio(t *testing.T) {
	fixture := newCatalogSource(t)
	raw := jsontext.Value(`{"code":-32603,"message":"<원격 오류>","data":{"private":"서버 값","version":9007199254740993}}`)
	c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		switch r.Header.Get("Mcp-Method") {
		case "server/discover":
			return rpcResponse(id, fixture.discovery), nil
		case "tools/list":
			return rpcResponse(id, fixture.tools), nil
		default:
			return rawResponse(500, []byte(`{"jsonrpc":"2.0","id":"`+id+`","error":`+string(raw)+`}`)), nil
		}
	}), HTTPOptions{})
	policy, _ := contract.NewPolicy("all", nil)
	line := stdioExchange(t, host.NewTools(c, policy, uuid.NewV7()), `{"jsonrpc":"2.0","id":"host-original","method":"tools/call","params":{"name":"graph_list","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	var response map[string]jsontext.Value
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response["error"], raw) || string(response["id"]) != `"host-original"` || response["result"] != nil {
		t.Fatalf("유효한 원격 오류 손상: %s", line)
	}
}

func stdioExchange(t *testing.T, tools *host.Tools, frame string) string {
	t.Helper()
	in, input := io.Pipe()
	output, out := io.Pipe()
	defer input.Close()
	defer output.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- host.Run(ctx, in, out, slog.New(slog.NewJSONHandler(io.Discard, nil)), "test", tools)
		out.Close()
	}()
	if _, err := io.WriteString(input, frame+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	input.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return line
}

type blockedHostWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedHostWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestHostOutputBackpressureKeepsSlotsAndQueueBounded(t *testing.T) {
	fixture := newCatalogSource(t)
	var calls atomic.Int32
	c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		switch r.Header.Get("Mcp-Method") {
		case "server/discover":
			return rpcResponse(id, fixture.discovery), nil
		case "tools/list":
			return rpcResponse(id, fixture.tools), nil
		default:
			calls.Add(1)
			return rpcResponse(id, jsontext.Value(`{"content":[],"isError":false}`)), nil
		}
	}), HTTPOptions{})
	policy, _ := contract.NewPolicy("all", nil)
	in, input := io.Pipe()
	defer input.Close()
	writer := &blockedHostWriter{started: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(writer.release) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- host.Run(ctx, in, writer, slog.New(slog.NewJSONHandler(io.Discard, nil)), "test", host.NewTools(c, policy, uuid.NewV7()))
	}()
	sent := make(chan error, 1)
	go func() {
		for id := range 136 {
			frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"graph_list","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, id+1)
			if _, err := io.WriteString(input, frame+"\n"); err != nil {
				sent <- err
				return
			}
		}
		sent <- nil
	}()
	select {
	case <-writer.started:
	case <-time.After(3 * time.Second):
		t.Fatal("출력 시작 시간 초과")
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.admission.mu.Lock()
		active, waiting := c.admission.active, len(c.admission.waiters)
		c.admission.mu.Unlock()
		if active == 8 && waiting == 128 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("느린 출력에서 상한 미유지 active=%d waiting=%d calls=%d", active, waiting, calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	await(t, func() bool { return calls.Load() == 8 })
	if calls.Load() != 8 {
		t.Fatalf("출력 대기 중 추가 원격 호출=%d", calls.Load())
	}
	cancel()
	release.Do(func() { close(writer.release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("출력 대기 취소 뒤 종료 시간 초과")
	}
}
