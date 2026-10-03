package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

type runningHost struct {
	input  *io.PipeWriter
	output *bufio.Reader
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

func startHost(t *testing.T, tools ...*Tools) *runningHost {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	running := &runningHost{
		input:  inWriter,
		output: bufio.NewReader(outReader),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	go func() {
		running.err = Run(ctx, inReader, outWriter, slog.New(slog.NewJSONHandler(io.Discard, nil)), "test-version", tools...)
		_ = outWriter.Close()
		close(running.done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = inWriter.Close()
		_ = outReader.Close()
		select {
		case <-running.done:
		case <-time.After(3 * time.Second):
			t.Error("호스트 세션이 종료되지 않았습니다")
		}
	})
	return running
}

func (r *runningHost) readLine(t *testing.T) string {
	t.Helper()
	result := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, err := r.output.ReadString('\n')
		result <- struct {
			line string
			err  error
		}{line, err}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.line
	case <-time.After(3 * time.Second):
		t.Fatal("호스트 응답 시간 초과")
		return ""
	}
}

func TestHostDiscoverAndLegacyHandshake(t *testing.T) {
	running := startHost(t)
	if _, err := io.WriteString(running.input, discoverRequest+"\n"); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID     int64 `json:"id"`
		Result struct {
			SupportedVersions []string `json:"supportedVersions"`
			Capabilities      struct {
				Tools     map[string]any `json:"tools"`
				Resources map[string]any `json:"resources"`
				Prompts   map[string]any `json:"prompts"`
			} `json:"capabilities"`
			Meta map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(running.readLine(t)), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 1 || len(response.Result.SupportedVersions) != 1 ||
		response.Result.SupportedVersions[0] != protocolVersion || response.Result.Capabilities.Tools == nil ||
		response.Result.Capabilities.Resources != nil || response.Result.Capabilities.Prompts != nil {
		t.Fatalf("discover 계약이 다릅니다: %+v", response)
	}
	serverInfo, ok := response.Result.Meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if !ok || serverInfo["name"] != "agent-context-client" || serverInfo["version"] != "test-version" {
		t.Fatalf("serverInfo가 다릅니다: %v", response.Result.Meta)
	}
	if _, err := io.WriteString(running.input, `{"jsonrpc":"2.0","id":2,"method":"initialize"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if line := running.readLine(t); !strings.Contains(line, `"code":-32601`) {
		t.Fatalf("구형 handshake가 거부되지 않았습니다: %s", line)
	}
	if err := running.input.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHostDiscoverImmediatelyBeforeEOF(t *testing.T) {
	var output bytes.Buffer
	err := Run(t.Context(), io.NopCloser(strings.NewReader(discoverRequest+"\n")), &output,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 {
		t.Fatal("EOF 직전 discover 응답이 기록되지 않았습니다")
	}
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("EOF 직전 요청의 응답 수 = %d: %q", len(lines), output.String())
	}
	var response struct {
		ID     int64 `json:"id"`
		Result struct {
			SupportedVersions []string `json:"supportedVersions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[0], &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 1 || len(response.Result.SupportedVersions) != 1 || response.Result.SupportedVersions[0] != protocolVersion {
		t.Fatalf("EOF 직전 discover 응답 = %+v", response)
	}
}

func TestHostConcurrentRequestsImmediatelyBeforeEOF(t *testing.T) {
	var input strings.Builder
	for id := 1; id <= 128; id++ {
		input.WriteString(strings.Replace(discoverRequest, `"id":1`, `"id":`+strconv.Itoa(id), 1))
		input.WriteByte('\n')
	}
	var output bytes.Buffer
	if err := Run(t.Context(), io.NopCloser(strings.NewReader(input.String())), &output,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), "test-version"); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 128 {
		t.Fatalf("EOF 직전 동시 요청의 응답 수 = %d", len(lines))
	}
	seen := make(map[int64]bool)
	for _, line := range lines {
		var response struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		if response.ID < 1 || response.ID > 128 || seen[response.ID] {
			t.Fatalf("EOF 직전 요청의 응답 ID 혼선: %d", response.ID)
		}
		seen[response.ID] = true
	}
}

func TestHostConcurrentRequestIDs(t *testing.T) {
	running := startHost(t)
	sent := make(chan error, 1)
	go func() {
		for id := 1; id <= 128; id++ {
			frame := strings.Replace(discoverRequest, `"id":1`, `"id":`+strconv.Itoa(id), 1)
			if _, err := io.WriteString(running.input, frame+"\n"); err != nil {
				sent <- err
				return
			}
		}
		sent <- nil
	}()
	seen := make(map[int64]bool)
	for range 128 {
		var response struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(running.readLine(t)), &response); err != nil {
			t.Fatal(err)
		}
		if response.ID < 1 || response.ID > 128 || seen[response.ID] {
			t.Fatalf("응답 ID 혼선: %d", response.ID)
		}
		seen[response.ID] = true
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

func TestHostCancellationClosesInput(t *testing.T) {
	running := startHost(t)
	running.cancel()
	select {
	case <-running.done:
		if running.err != nil {
			t.Fatal(running.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("취소 뒤 호스트가 종료되지 않았습니다")
	}
}
