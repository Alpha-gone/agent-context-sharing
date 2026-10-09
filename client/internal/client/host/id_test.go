package host

import (
	"bytes"
	"encoding/json/jsontext"
	"io"
	"strings"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func TestHostPreservesExactIDsAndRejectsInvalidIDs(t *testing.T) {
	policy, _ := contract.NewPolicy("all", nil)
	for _, method := range []string{"initialize", "server/discover", "ping", "tools/list", "tools/call"} {
		for _, id := range []string{`0`, `-0`, `9007199254740993`, `-9007199254740993`, `9223372036854775808`, `"9007199254740993"`, `"\u0000number:9007199254740993"`, `1.5`, `1.0`, `1e3`, `null`, `true`, `{}`, `[]`} {
			t.Run(method+"/"+id, func(t *testing.T) {
				running := startHost(t, NewTools(newToolRemote(t), policy, uuid.UUID{}))
				request := toolRequest(t, jsontext.Value(id), method, "graph_list", jsontext.Value(`{}`))
				if method == "initialize" {
					request = strings.Replace(initializeRequest, `"id":1`, `"id":`+id, 1) + "\n"
				} else if method == "server/discover" {
					request = strings.Replace(discoverRequest, `"id":1`, `"id":`+id, 1) + "\n"
				}
				response := exchange(t, running, request)
				valid := id[0] == '"' || !strings.ContainsAny(id, ".e") && (id[0] == '9' || id[0] == '-' || id[0] == '0')
				if valid {
					if string(response["id"]) != id {
						t.Fatalf("호스트 ID 손실: got=%s want=%s", response["id"], id)
					}
				} else if string(response["id"]) != "null" || !strings.Contains(string(response["error"]), `"code":-32600`) {
					t.Fatalf("잘못된 ID를 거부하지 않았다: %v", response)
				}
			})
		}
	}
}

func TestHostLargeIDsRemainDistinctWhilePending(t *testing.T) {
	ids := []string{`9007199254740992`, `9007199254740993`, `"9007199254740993"`, `"\u0000number:9007199254740993"`}
	var input strings.Builder
	for _, id := range ids {
		input.WriteString(strings.Replace(discoverRequest, `"id":1`, `"id":`+id, 1) + "\n")
	}
	var output bytes.Buffer
	connection := newTestConnection(t, input.String(), &output)
	var requests []*jsonrpc.Request
	for range ids {
		message, err := connection.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, message.(*jsonrpc.Request))
	}
	if output.Len() != 0 {
		t.Fatalf("서로 다른 ID의 충돌: %s", output.Bytes())
	}
	for _, request := range requests {
		if err := connection.Write(t.Context(), &jsonrpc.Response{ID: request.ID, Result: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for index, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		if !bytes.Contains(line, []byte(`"id":`+ids[index]+`,`)) {
			t.Fatalf("동시 요청 ID 손실: %s", line)
		}
	}
}

func TestHostLargeCancellationTargetsExactID(t *testing.T) {
	remote := &cancellationRemote{newToolRemote(t).catalog, make(chan struct{}), make(chan struct{})}
	policy, _ := contract.NewPolicy("all", nil)
	connection := newTestConnection(t, "", io.Discard)
	connection.tools = NewTools(remote, policy, uuid.UUID{})
	message, err := decodeHostMessage([]byte(toolRequest(t, jsontext.Value(`9007199254740993`), "tools/call", "graph_list", jsontext.Value(`{}`))))
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.dispatch(t.Context(), message.(*jsonrpc.Request)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-remote.started:
	case <-time.After(time.Second):
		t.Fatal("호출 미시작")
	}
	for _, id := range []string{`9007199254740992`, `"9007199254740993"`, `"\u0000number:9007199254740993"`} {
		connection.cancelRequest([]byte(`{"requestId":` + id + `}`))
		select {
		case <-remote.cancelled:
			t.Fatalf("다른 ID가 큰 정수 요청을 취소했다: %s", id)
		default:
		}
	}
	connection.cancelRequest([]byte(`{"requestId":9007199254740993}`))
	select {
	case <-remote.cancelled:
	case <-time.After(time.Second):
		t.Fatal("큰 정수의 일치한 취소 ID 미전파")
	}
}

func TestHostOversizedFramePreservesLargeID(t *testing.T) {
	for _, id := range []string{`9007199254740993`, `9223372036854775808`, `1.5`, `1e3`, `null`} {
		frame := `{"id":` + id + `,"padding":"` + strings.Repeat("x", maxInputBytes) + `"}`
		var output bytes.Buffer
		connection := newTestConnection(t, frame+"\n"+discoverRequest+"\n", &output)
		if _, err := connection.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := "null"
		if id[0] == '9' {
			want = id
		}
		if !bytes.Contains(output.Bytes(), []byte(`"id":`+want+`,`)) {
			t.Fatalf("초과 프레임 ID = %s, want %s", output.Bytes(), want)
		}
	}
}

func TestHostInvalidCancellationDoesNotCancelIntegerRequest(t *testing.T) {
	for _, id := range []string{`1.2`, `null`, `true`, `"1"`} {
		t.Run(id, func(t *testing.T) {
			remote := &cancellationRemote{newToolRemote(t).catalog, make(chan struct{}), make(chan struct{})}
			policy, _ := contract.NewPolicy("all", nil)
			running := startHost(t, NewTools(remote, policy, uuid.UUID{}))
			io.WriteString(running.input, toolRequest(t, 1, "tools/call", "graph_list", jsontext.Value(`{}`)))
			select {
			case <-remote.started:
			case <-time.After(time.Second):
				t.Fatal("호출 미시작")
			}
			io.WriteString(running.input, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":`+id+`}}`+"\n")
			response := exchange(t, running, strings.Replace(discoverRequest, `"id":1`, `"id":2`, 1)+"\n")
			if string(response["id"]) != "2" {
				t.Fatal("취소 통지가 다른 요청에 전달되었다")
			}
			select {
			case <-remote.cancelled:
				t.Fatal("잘못된 ID가 정수 요청을 취소했다")
			default:
			}
			io.WriteString(running.input, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`+"\n")
			select {
			case <-remote.cancelled:
			case <-time.After(time.Second):
				t.Fatal("일치한 취소 ID 미전파")
			}
		})
	}
}
