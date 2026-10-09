package host

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
)

const initializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"host","version":"1"}}}`

func initializeHost(t *testing.T, running *runningHost, revision string) {
	t.Helper()
	response := exchange(t, running, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"`+revision+`","capabilities":{},"clientInfo":{"name":"jetbrains-ai-assistant-client","version":"1.0.0"}}}`+"\n")
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(response["result"], &result); err != nil || result.ProtocolVersion != revision || result.Capabilities.Tools == nil || result.ServerInfo.Name != "agent-context-client" {
		t.Fatalf("호환 초기 연결: %s, %v", response["result"], err)
	}
	if _, err := io.WriteString(running.input, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
		t.Fatal(err)
	}
}

func legacyToolRequest(t *testing.T, id any, method, name string, arguments jsontext.Value) string {
	t.Helper()
	var params any = map[string]any{}
	if method == "tools/call" {
		params = map[string]any{"name": name, "arguments": arguments}
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func TestLegacyToolsPoliciesInjectionAndResults(t *testing.T) {
	agent := uuid.MustParse("019f0000-0000-7000-8000-000000000001")
	for _, mode := range []string{"all", "read_only", "allowlist"} {
		t.Run(mode, func(t *testing.T) {
			var names []string
			if mode == "allowlist" {
				names = []string{"graph_list", "node_create"}
			}
			policy, _ := contract.NewPolicy(mode, names)
			remote := newToolRemote(t)
			raw := jsontext.Value(`{"content":[],"structuredContent":{"version":9007199254740993,"score":0.1234567890123456789,"text":"\u003c原文\u003e"},"isError":false,"_meta":{"extension":{"resultType":"nested","ttlMs":123}}}`)
			remote.result, _ = contract.NewResult(raw)
			running := startHost(t, NewTools(remote, policy, agent))
			initializeHost(t, running, "2025-11-25")
			listed := exchange(t, running, `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`+"\n")
			var list struct {
				Tools []contract.Tool `json:"tools"`
			}
			if json.Unmarshal(listed["result"], &list) != nil || len(list.Tools) != len(remote.catalog.Tools(policy)) || bytes.Contains(listed["result"], []byte(`"resultType"`)) || bytes.Contains(listed["result"], []byte(`"ttlMs"`)) {
				t.Fatalf("호환 목록: %s", listed["result"])
			}
			all, _ := contract.NewPolicy("all", nil)
			for _, tool := range remote.catalog.Tools(all) {
				args := hostArguments(t, tool, agent.String())
				before := remote.calls
				response := exchange(t, running, legacyToolRequest(t, tool.Name, "tools/call", tool.Name, args))
				if !policy.Allows(tool.Name) {
					assertClientError(t, response["result"], "client_protocol", false)
					if remote.calls != before {
						t.Fatal("정책 밖 도구를 전송했습니다")
					}
					continue
				}
				if !bytes.Equal(response["result"], raw) || string(response["id"]) != `"`+tool.Name+`"` {
					t.Fatalf("호환 결과 원문: %s", response["result"])
				}
				if contract.InjectsAgent(tool.Name) {
					if !bytes.Contains(remote.arguments, []byte(`"created_by_agent":"`+agent.String()+`"`)) {
						t.Fatal("행위 에이전트 주입 누락")
					}
					bypass, _ := setMember(args, "created_by_agent", jsontext.Value(`"`+agent.String()+`"`))
					response := exchange(t, running, legacyToolRequest(t, "bypass", "tools/call", tool.Name, bypass))
					assertClientError(t, response["result"], "client_protocol", false)
					if remote.calls != before+1 {
						t.Fatal("우회 입력을 전송했습니다")
					}
				}
			}
		})
	}
}

func TestLegacyHandshakeValidationAndPing(t *testing.T) {
	remote := newToolRemote(t)
	policy, _ := contract.NewPolicy("all", nil)
	running := startHost(t, NewTools(remote, policy, uuid.UUID{}))
	response := exchange(t, running, legacyToolRequest(t, 1, "tools/list", "", nil))
	if response["error"] == nil {
		t.Fatal("초기 연결 전 목록이 성공했습니다")
	}
	response = exchange(t, running, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":null,"clientInfo":{"name":"host","version":"1"}}}`+"\n")
	if !bytes.Contains(response["error"], []byte(`"code":-32602`)) {
		t.Fatal("잘못된 초기 연결을 허용했습니다")
	}
	response = exchange(t, running, legacyToolRequest(t, 3, "tools/list", "", nil))
	if response["error"] == nil {
		t.Fatal("실패한 초기 연결 뒤 목록이 성공했습니다")
	}
	initializeHost(t, running, "2025-11-25")
	response = exchange(t, running, `{"jsonrpc":"2.0","id":"ping","method":"ping"}`+"\n")
	if string(response["result"]) != `{}` {
		t.Fatalf("ping 결과: %s", response["result"])
	}
	response = exchange(t, running, `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"wrong"}}}`+"\n")
	if response["error"] == nil {
		t.Fatal("명시한 잘못된 revision을 호환 연결로 우회했습니다")
	}
	response = exchange(t, running, legacyToolRequest(t, "init", "tools/list", "", nil))
	if response["error"] != nil {
		t.Fatalf("초기 연결 ID 재사용: %s", response["error"])
	}
	response = exchange(t, running, toolRequest(t, 5, "tools/list", "", nil))
	if !bytes.Contains(response["result"], []byte(`"resultType":"complete"`)) {
		t.Fatal("무상태 요청의 응답 외피가 바뀌었습니다")
	}
	remote.err = protocolFixtureError{jsontext.Value(`{"code":-32603,"message":"remote","data":{"version":9007199254740993}}`)}
	response = exchange(t, running, legacyToolRequest(t, 6, "tools/call", "graph_list", jsontext.Value(`{}`)))
	if !bytes.Equal(response["error"], remote.err.(protocolFixtureError).raw) {
		t.Fatal("원격 JSON-RPC 오류가 바뀌었습니다")
	}
}

func TestLegacyCancellationAndEOF(t *testing.T) {
	for _, eof := range []bool{false, true} {
		t.Run(map[bool]string{true: "EOF", false: "cancel"}[eof], func(t *testing.T) {
			remote := &cancellationRemote{newToolRemote(t).catalog, make(chan struct{}), make(chan struct{})}
			policy, _ := contract.NewPolicy("all", nil)
			running := startHost(t, NewTools(remote, policy, uuid.UUID{}))
			initializeHost(t, running, "2025-11-25")
			if _, err := io.WriteString(running.input, legacyToolRequest(t, "cancel", "tools/call", "graph_list", jsontext.Value(`{}`))); err != nil {
				t.Fatal(err)
			}
			select {
			case <-remote.started:
			case <-time.After(3 * time.Second):
				t.Fatal("호환 호출 시작 시간 초과")
			}
			if eof {
				running.input.Close()
			} else {
				io.WriteString(running.input, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel"}}`+"\n")
			}
			select {
			case <-remote.cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("호환 호출 취소 미전파")
			}
			if eof {
				var response map[string]jsontext.Value
				json.Unmarshal([]byte(running.readLine(t)), &response)
				assertClientError(t, response["result"], "client_timeout", true)
				if bytes.Contains(response["result"], []byte(`"resultType"`)) {
					t.Fatal("호환 오류에 새 외피가 포함됐습니다")
				}
			} else {
				response := exchange(t, running, `{"jsonrpc":"2.0","id":"ping","method":"ping"}`+"\n")
				if string(response["id"]) != `"ping"` {
					t.Fatal("명시적 취소 응답이 전송됐습니다")
				}
				running.input.Close()
			}
			if extra, err := running.output.ReadString('\n'); !errors.Is(err, io.EOF) || extra != "" {
				t.Fatalf("종료 뒤 추가 응답: %q, %v", extra, err)
			}
		})
	}
}

func TestLegacyResultRemovesOnlyEnvelope(t *testing.T) {
	for _, raw := range []jsontext.Value{
		jsontext.Value(`{"resultType":"complete","ttlMs":0,"cacheScope":"private","_meta":{"io.modelcontextprotocol/serverInfo":{}},"content":[]}`),
		jsontext.Value(`{"content":[],"_meta":{"io.modelcontextprotocol/serverInfo":{}},"ttlMs":0,"cacheScope":"private","resultType":"complete"}`),
	} {
		result, err := legacyResult(raw)
		if err != nil || string(result) != `{"content":[]}` {
			t.Fatalf("호환 외피 제거: %s, %v", result, err)
		}
	}
}
