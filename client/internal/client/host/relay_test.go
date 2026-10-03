package host

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func toolRequest(t *testing.T, id any, method, name string, args jsontext.Value) string {
	t.Helper()
	params := map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    protocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}}
	if method == "tools/call" {
		params["name"], params["arguments"] = name, args
	}
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return string(frame) + "\n"
}

func exchange(t *testing.T, running *runningHost, request string) map[string]jsontext.Value {
	t.Helper()
	if _, err := io.WriteString(running.input, request); err != nil {
		t.Fatal(err)
	}
	var response map[string]jsontext.Value
	if err := json.Unmarshal([]byte(running.readLine(t)), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func assertClientError(t *testing.T, raw jsontext.Value, code string, retryable bool) {
	t.Helper()
	var result struct {
		IsError           bool  `json:"isError"`
		Content           []any `json:"content"`
		StructuredContent struct {
			Error contract.ClientError `json:"client_error"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("오류 결과=%s: %v", raw, err)
	}
	if !result.IsError || result.Content == nil || result.StructuredContent.Error.Code != code || result.StructuredContent.Error.Retryable != retryable || result.StructuredContent.Error.Message == "" {
		t.Fatalf("클라이언트 오류 외피 = %s", raw)
	}
}

func expectedHostResult(raw jsontext.Value) jsontext.Value {
	return append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"agent-context-client","version":"test-version"}},"resultType":"complete"}`)...)
}

func TestStdioAllToolsPoliciesResultsAndInjection(t *testing.T) {
	agent := uuid.MustParse("019f0000-0000-7000-8000-000000000001")
	all, _ := contract.NewPolicy("all", nil)
	for _, mode := range []string{"all", "read_only", "allowlist"} {
		t.Run(mode, func(t *testing.T) {
			var names []string
			if mode == "allowlist" {
				names = []string{"graph_list", "node_create"}
			}
			policy, _ := contract.NewPolicy(mode, names)
			remote := newToolRemote(t)
			running := startHost(t, NewTools(remote, policy, agent))
			listed := exchange(t, running, toolRequest(t, "list-host-id", "tools/list", "", nil))
			var list struct {
				Tools []contract.Tool `json:"tools"`
			}
			if err := json.Unmarshal(listed["result"], &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Tools) != len(remote.catalog.Tools(policy)) {
				t.Fatal("stdio 목록 정책 불일치")
			}
			for _, tool := range remote.catalog.Tools(all) {
				args := hostArguments(t, tool, agent.String())
				before := remote.calls
				response := exchange(t, running, toolRequest(t, tool.Name, "tools/call", tool.Name, args))
				if string(response["id"]) != `"`+tool.Name+`"` {
					t.Fatal("호스트 ID 혼선")
				}
				if !policy.Allows(tool.Name) {
					assertClientError(t, response["result"], "client_protocol", false)
					if remote.calls != before {
						t.Fatal("정책 밖 전송")
					}
					continue
				}
				if !bytes.Equal(response["result"], expectedHostResult(remote.result.Raw())) {
					t.Fatalf("원문 손상: %s", response["result"])
				}
				if contract.InjectsAgent(tool.Name) {
					if !bytes.Contains(remote.arguments, []byte(`"created_by_agent":"`+agent.String()+`"`)) {
						t.Fatal("행위 에이전트 주입 누락")
					}
					var bypass map[string]jsontext.Value
					json.Unmarshal(args, &bypass)
					bypass["created_by_agent"] = jsontext.Value(`"` + agent.String() + `"`)
					raw, _ := json.Marshal(bypass)
					blocked := exchange(t, running, toolRequest(t, "bypass", "tools/call", tool.Name, raw))
					assertClientError(t, blocked["result"], "client_protocol", false)
					if remote.calls != before+1 {
						t.Fatal("우회 입력 전송")
					}
				}
				for _, domain := range []string{"permission_denied", "not_found", "invalid_argument", "version_conflict", "limit_exceeded", "result_truncated", "not_supported", "internal"} {
					raw := jsontext.Value(`{"content":[{"type":"text","text":"<원문>"}],"isError":true,"structuredContent":{"error":{"code":"` + domain + `"},"version":9007199254740993}}`)
					remote.result, _ = contract.NewResult(raw)
					response := exchange(t, running, toolRequest(t, tool.Name, "tools/call", tool.Name, args))
					if !bytes.Equal(response["result"], expectedHostResult(raw)) {
						t.Fatalf("도메인 오류 재포장·재직렬화: got=%s want=%s", response["result"], raw)
					}
				}
			}
		})
	}
}

type protocolFixtureError struct{ raw jsontext.Value }

func (protocolFixtureError) Error() string         { return "비공개 내부 오류" }
func (e protocolFixtureError) Raw() jsontext.Value { return e.raw }

func TestStdioClientErrorsAndRemoteProtocolError(t *testing.T) {
	policy, _ := contract.NewPolicy("all", nil)
	remote := newToolRemote(t)
	running := startHost(t, NewTools(remote, policy, uuid.UUID{}))
	for _, tc := range []struct {
		err       error
		code      string
		retryable bool
	}{
		{contract.ErrConfiguration, "client_configuration", false},
		{contract.ErrAuthorization, "client_authorization", true},
		{contract.ErrIdentityChanged, "client_authorization", true},
		{contract.ErrTransport, "client_transport", true},
		{context.DeadlineExceeded, "client_timeout", true},
		{contract.ErrProtocol, "client_protocol", false},
		{contract.ErrIndeterminate, "client_indeterminate", false},
		{contract.ErrBusy, "client_busy", true},
		{errors.New("https://secret.example/token?code=secret"), "client_protocol", false},
	} {
		remote.err = fmt.Errorf("비공개: %w", tc.err)
		for _, tool := range remote.catalog.Tools(policy) {
			args := hostArguments(t, tool, "019f0000-0000-7000-8000-000000000001")
			response := exchange(t, running, toolRequest(t, tool.Name, "tools/call", tool.Name, args))
			assertClientError(t, response["result"], tc.code, tc.retryable)
			if bytes.Contains(response["result"], []byte("secret")) || bytes.Contains(response["result"], []byte("비공개:")) {
				t.Fatal("내부 오류 유출")
			}
		}
	}
	raw := jsontext.Value(`{"code":-32603,"message":"서버 메시지 <그대로>","data":{"version":9007199254740993,"extra":null}}`)
	remote.err = protocolFixtureError{raw}
	response := exchange(t, running, toolRequest(t, "host-id", "tools/call", "graph_list", jsontext.Value(`{}`)))
	if !bytes.Equal(response["error"], raw) || string(response["id"]) != `"host-id"` || response["result"] != nil {
		t.Fatal("원격 protocol 오류 보존 실패")
	}
	response = exchange(t, running, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"graph_list","arguments":{}}}`+"\n")
	assertClientError(t, response["result"], "client_protocol", false)
}

type cancellationRemote struct {
	catalog   *contract.Catalog
	started   chan struct{}
	cancelled chan struct{}
}

func (r *cancellationRemote) ListTools(context.Context) (*contract.Catalog, error) {
	return r.catalog, nil
}
func (r *cancellationRemote) CallTool(ctx context.Context, _ string, _ jsontext.Value) (*contract.Result, error) {
	close(r.started)
	<-ctx.Done()
	close(r.cancelled)
	return nil, ctx.Err()
}

func TestStdioCancellationAndEOFDrainToolResponse(t *testing.T) {
	for _, eof := range []bool{false, true} {
		t.Run(fmt.Sprint(eof), func(t *testing.T) {
			remote := &cancellationRemote{newToolRemote(t).catalog, make(chan struct{}), make(chan struct{})}
			policy, _ := contract.NewPolicy("all", nil)
			running := startHost(t, NewTools(remote, policy, uuid.UUID{}))
			io.WriteString(running.input, toolRequest(t, "cancel-me", "tools/call", "graph_list", jsontext.Value(`{}`)))
			select {
			case <-remote.started:
			case <-time.After(3 * time.Second):
				t.Fatal("호출 시작 시간 초과")
			}
			if eof {
				running.input.Close()
			} else {
				io.WriteString(running.input, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel-me","reason":"secret"}}`+"\n")
			}
			select {
			case <-remote.cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("취소 미전파")
			}
			if !eof {
				// EOF까지 모든 접수 요청을 정리하므로 뒤늦은 취소 응답도 확인한다.
				io.WriteString(running.input, discoverRequest+"\n")
				running.input.Close()
			}
			line := running.readLine(t)
			var response map[string]jsontext.Value
			json.Unmarshal([]byte(line), &response)
			if eof {
				assertClientError(t, response["result"], "client_timeout", true)
			} else if string(response["id"]) != "1" {
				t.Fatalf("명시적 취소 뒤 응답 전송: %s", line)
			}
			if extra, err := running.output.ReadString('\n'); err != io.EOF || extra != "" {
				t.Fatalf("추가 응답: %q err=%v", extra, err)
			}
		})
	}
}

func TestContextFlowAndOutputLimitPreserveFraming(t *testing.T) {
	remote := newToolRemote(t)
	policy, _ := contract.NewPolicy("all", nil)
	running := startHost(t, NewTools(remote, policy, uuid.UUID{}))
	raw := jsontext.Value(`{"structuredContent":{"contexts":[{"id":"b"},{"id":"a"}],"references":[],"relations":[],"entry_points":["b","a"],"budget":123,"channels":["vector","graph"],"cursor":"opaque+/=","version":9007199254740993,"truncated":true,"search":{"score":0.1234567890123456789,"matched_terms":["z","a"],"partial":true}},"content":[],"isError":false,"extension":null}`)
	remote.result, _ = contract.NewResult(raw)
	response := exchange(t, running, toolRequest(t, 1, "tools/call", "context_flow_get", jsontext.Value(`{"graph_id":"019f0000-0000-7000-8000-000000000001","work_context":"작업"}`)))
	if !bytes.Equal(response["result"], expectedHostResult(raw)) {
		t.Fatalf("평면 구조·검색 metadata 손상: %s", response["result"])
	}
	remote.result, _ = contract.NewResult(jsontext.Value(`{"content":[{"type":"text","text":"` + strings.Repeat("x", maxOutputBytes) + `"}],"isError":false}`))
	response = exchange(t, running, toolRequest(t, 2, "tools/call", "graph_list", jsontext.Value(`{}`)))
	assertClientError(t, response["result"], "client_protocol", false)
	remote.result, _ = contract.NewResult(jsontext.Value(`{"content":[],"isError":false}`))
	response = exchange(t, running, toolRequest(t, 3, "tools/call", "graph_list", jsontext.Value(`{}`)))
	if string(response["id"]) != "3" {
		t.Fatal("초과 응답 뒤 프레이밍 실패")
	}
}

type pausedWriter struct {
	bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *pausedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.Buffer.Write(p)
}

func TestCancellationSuppressesLateSuccessBehindSlowOutput(t *testing.T) {
	output := &pausedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	connection := newTestConnection(t, "", output)
	policy, _ := contract.NewPolicy("all", nil)
	connection.tools = NewTools(newToolRemote(t), policy, uuid.UUID{})
	first := make(chan error, 1)
	go func() { first <- connection.writeFrame(t.Context(), []byte("already-started\n")) }()
	<-output.entered
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(output.release) }) })
	message, err := jsonrpc.DecodeMessage([]byte(toolRequest(t, "late-success", "tools/call", "graph_list", jsontext.Value(`{}`))))
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.dispatch(t.Context(), message.(*jsonrpc.Request)); err != nil {
		t.Fatal(err)
	}
	connection.cancelRequest([]byte(`{"requestId":"late-success"}`))
	release.Do(func() { close(output.release) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { connection.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("취소 응답 대기·slot 미해제")
	}
	if output.String() != "already-started\n" {
		t.Fatalf("출력 대기 중 취소된 성공 응답: %s", output.String())
	}
}

func TestListInvalidInputAndBusyUseJSONRPCError(t *testing.T) {
	remote := newToolRemote(t)
	policy, _ := contract.NewPolicy("all", nil)
	running := startHost(t, NewTools(remote, policy, uuid.UUID{}))
	for _, params := range []string{`{}`, `{"cursor":"next","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`} {
		response := exchange(t, running, `{"jsonrpc":"2.0","id":"bad-list","method":"tools/list","params":`+params+"}\n")
		if response["result"] != nil || !bytes.Contains(response["error"], []byte(`"code":-32603`)) || !bytes.Contains(response["error"], []byte(`"code":"client_protocol"`)) {
			t.Fatalf("잘못된 목록 입력: %v", response)
		}
	}
	var output bytes.Buffer
	connection := newTestConnection(t, "", &output)
	connection.toolPending = 136
	message, _ := jsonrpc.DecodeMessage([]byte(toolRequest(t, 137, "tools/list", "", nil)))
	if err := connection.dispatch(t.Context(), message.(*jsonrpc.Request)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":-32603`) || !strings.Contains(output.String(), `"code":"client_busy"`) || strings.Contains(output.String(), `"result":`) {
		t.Fatalf("목록 접수 상한: %s", output.String())
	}
}

func TestListEnvelopeOutputLimitUsesJSONRPCError(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, "", &output)
	id, _ := jsonrpc.MakeID(strings.Repeat("i", 2048))
	raw := jsontext.Value(`{"tools":[],"padding":"` + strings.Repeat("x", maxOutputBytes-1000) + `"}`)
	if err := connection.writeRaw(t.Context(), id, "tools/list", "result", raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":-32603`) || !strings.Contains(output.String(), `"code":"client_protocol"`) || strings.Contains(output.String(), `"result":`) {
		t.Fatal("호스트 ID를 포함한 목록 출력 상한이 빈 성공 목록으로 바뀜")
	}
}

func TestSDKClientReadsActualHostToolsAndErrors(t *testing.T) {
	remote := newToolRemote(t)
	policy, _ := contract.NewPolicy("all", nil)
	in, input := io.Pipe()
	output, out := io.Pipe()
	defer input.Close()
	defer output.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, in, out, logger, "sdk-host", NewTools(remote, policy, uuid.UUID{}))
		out.Close()
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-sdk-host", Version: "1"}, &mcp.ClientOptions{Logger: logger})
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: output, Writer: input}, &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 13 {
		t.Fatalf("SDK 목록=%+v err=%v", listed, err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "graph_list", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("SDK 정상 호출=%+v err=%v", result, err)
	}
	if info, ok := result.GetMeta()[mcp.MetaKeyServerInfo].(map[string]any); !ok || info["name"] != "agent-context-client" || info["version"] != "sdk-host" {
		t.Fatal("SDK 클라이언트 serverInfo 불일치")
	}
	for _, failure := range []error{contract.ErrConfiguration, contract.ErrAuthorization, contract.ErrTransport, context.DeadlineExceeded, contract.ErrProtocol, contract.ErrIndeterminate, contract.ErrBusy} {
		remote.err = failure
		listed, err = session.ListTools(ctx, nil)
		if err == nil {
			t.Fatalf("SDK 목록 실패가 성공으로 보임: %+v", listed)
		}
		protocolError, ok := errors.AsType[*jsonrpc.Error](err)
		if !ok || protocolError.Code != jsonrpc.CodeInternalError {
			t.Fatalf("SDK 목록 오류 외피: %v", err)
		}
		var data struct {
			ClientError contract.ClientError `json:"client_error"`
		}
		if json.Unmarshal(protocolError.Data, &data) != nil || data.ClientError != contract.ClassifyError(failure) || protocolError.Message != data.ClientError.Message {
			t.Fatalf("SDK 목록 오류 상세: %s", protocolError.Data)
		}
	}
	remote.err = contract.ErrAuthorization
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "graph_list", Arguments: map[string]any{}})
	if err != nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("SDK 클라이언트 오류=%+v err=%v", result, err)
	}
	session.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SDK 세션 종료 시간 초과")
	}
}
