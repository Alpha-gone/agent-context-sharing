package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

const discoverRequest = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"test-host","version":"1"}}}}`

func newTestConnection(t *testing.T, input string, output io.Writer) *stdioConn {
	t.Helper()
	transport := &stdioTransport{in: io.NopCloser(strings.NewReader(input)), out: output}
	connection, err := transport.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection.(*stdioConn)
}

func TestReadFrameExactLimitAndOneByteOver(t *testing.T) {
	for _, size := range []int{maxInputBytes, maxInputBytes + 1} {
		input := strings.Repeat("x", size) + "\n" + discoverRequest + "\n"
		reader := bufio.NewReader(strings.NewReader(input))
		frame, oversized, err := readFrame(reader)
		if err != nil || len(frame) != min(size, maxInputBytes+2) || oversized != (size > maxInputBytes) {
			t.Fatalf("size=%d: len=%d oversized=%t err=%v", size, len(frame), oversized, err)
		}
		frame, oversized, err = readFrame(reader)
		if err != nil || oversized || string(frame) != discoverRequest {
			t.Fatalf("후속 프레임을 복구하지 못했습니다: oversized=%t err=%v", oversized, err)
		}
	}
}

func TestOversizedFrameKeepsSafeIDAndRecoversNextMessage(t *testing.T) {
	oversized := `{"jsonrpc":"2.0","id":42,"method":"server/discover","params":{"pad":"` +
		strings.Repeat("x", maxInputBytes) + `"}}`
	var output bytes.Buffer
	connection := newTestConnection(t, oversized+"\n"+discoverRequest+"\n", &output)
	message, err := connection.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request, ok := message.(*jsonrpc.Request)
	if !ok || request.Method != "server/discover" || request.ID.Raw() != int64(1) {
		t.Fatalf("다음 요청이 손상됐습니다: %#v", message)
	}
	var response struct {
		ID    int64 `json:"id"`
		Error struct {
			Data struct {
				ClientError struct {
					Code string `json:"code"`
				} `json:"client_error"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 42 || response.Error.Data.ClientError.Code != "client_busy" {
		t.Fatalf("크기 오류의 식별자·코드가 다릅니다: %+v", response)
	}
}

func TestOversizedFrameWithoutSafeIDUsesNullID(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, strings.Repeat("x", maxInputBytes+1)+"\n"+discoverRequest+"\n", &output)
	if _, err := connection.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"id":null`)) ||
		!bytes.Contains(output.Bytes(), []byte(`"client_busy"`)) {
		t.Fatalf("식별자 없는 초과 입력의 오류 응답 = %s", output.String())
	}
}

func TestInvalidAndLegacyRequestsDoNotBreakFraming(t *testing.T) {
	var output bytes.Buffer
	input := "\n\r\n \t\r\nnot-json\n\n" + `{"jsonrpc":"2.0","id":false,"method":"server/discover"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"initialize"}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"server/discover"}` + "\n\n" + discoverRequest + "\n"
	connection := newTestConnection(t, input, &output)
	message, err := connection.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if request, ok := message.(*jsonrpc.Request); !ok || request.Method != "server/discover" {
		t.Fatalf("유효한 discover를 읽지 못했습니다: %#v", message)
	}
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 4 {
		t.Fatalf("오류 응답 수 = %d", len(lines))
	}
	var errors []struct {
		Error struct {
			Code int64 `json:"code"`
		} `json:"error"`
	}
	for _, line := range lines {
		var response struct {
			Error struct {
				Code int64 `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		errors = append(errors, response)
	}
	if errors[0].Error.Code != jsonrpc.CodeParseError || errors[1].Error.Code != jsonrpc.CodeInvalidRequest ||
		errors[2].Error.Code != jsonrpc.CodeMethodNotFound || errors[3].Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("오류 코드가 다릅니다: %+v", errors)
	}
}

func TestBlankLinesBeforeEOFProduceNoResponse(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, "\n\r\n \t\r\n \t", &output)
	if _, err := connection.Read(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("빈 줄 뒤 EOF = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("빈 줄에 응답했습니다: %q", output.String())
	}
}

func TestConcurrentWritesKeepCompleteLinesAndIDs(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, "", &output)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Go(func() {
			id, _ := jsonrpc.MakeID(float64(i + 1))
			if err := connection.Write(t.Context(), &jsonrpc.Response{ID: id, Result: []byte(`{"ok":true}`)}); err != nil {
				t.Errorf("응답 기록: %v", err)
			}
		})
	}
	wg.Wait()
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 128 {
		t.Fatalf("완전한 응답 줄 수 = %d", len(lines))
	}
	seen := make(map[int64]bool)
	for _, line := range lines {
		var response struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(line, &response); err != nil || seen[response.ID] {
			t.Fatalf("출력 줄 또는 ID가 손상됐습니다: %v", err)
		}
		seen[response.ID] = true
	}
}

func TestOversizedOutputIsReplacedBeforeWriting(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, "", &output)
	id, _ := jsonrpc.MakeID(float64(7))
	response := &jsonrpc.Response{ID: id, Result: []byte(`"` + strings.Repeat("x", maxOutputBytes) + `"`)}
	if err := connection.Write(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	if output.Len() >= maxOutputBytes || !bytes.Contains(output.Bytes(), []byte(`"client_protocol"`)) ||
		!bytes.Contains(output.Bytes(), []byte(`"id":7`)) {
		t.Fatal("출력 제한 위반이 부분 응답으로 기록됐습니다")
	}
	if err := connection.Write(t.Context(), &jsonrpc.Response{ID: id, Result: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(output.Bytes(), []byte{'\n'}) != 2 {
		t.Fatal("크기 오류 뒤의 정상 응답 프레이밍이 손상됐습니다")
	}
}

func TestClosedConnectionStopsReadAndWrite(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, "", &output)
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Read(context.Background()); err == nil {
		t.Fatal("닫힌 연결의 Read가 성공했습니다")
	}
	id, _ := jsonrpc.MakeID(float64(1))
	if err := connection.Write(context.Background(), &jsonrpc.Response{ID: id, Result: []byte(`{}`)}); err == nil {
		t.Fatal("닫힌 연결의 Write가 성공했습니다")
	}
}

func TestReadCancellationDoesNotWaitForStdinEOF(t *testing.T) {
	input, keepOpen := io.Pipe()
	defer keepOpen.Close()
	transport := &stdioTransport{in: input, out: io.Discard}
	connection, err := transport.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := connection.Read(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("취소 결과 = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stdin EOF 없이 취소가 끝나지 않았습니다")
	}
}

func TestHostAdmissionLimitReturnsBusyAndKeepsFraming(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, toolRequest(t, 137, "tools/call", "graph_list", []byte(`{}`))+discoverRequest+"\n", &output)
	connection.toolPending = 136
	if _, err := connection.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID     int            `json:"id"`
		Result jsontext.Value `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 137 {
		t.Fatal("상한 오류 ID 혼선")
	}
	assertClientError(t, response.Result, "client_busy", true)
	if connection.toolPending != 136 {
		t.Fatal("초과 요청을 접수했습니다")
	}
}
