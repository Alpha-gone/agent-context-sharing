package host

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHostDuplicateLifecycleDrainsAfterEOF는 첫 SDK 핸들러를 멈춰 중복 ID가
// 진행 중인 요청과 확실히 겹쳐도 EOF 뒤 응답을 기록하고 종료하는지 확인한다.
func TestHostDuplicateLifecycleDrainsAfterEOF(t *testing.T) {
	for _, request := range []string{discoverRequest, initializeRequest, `{"jsonrpc":"2.0","id":1,"method":"ping"}`} {
		var decoded struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(request), &decoded); err != nil {
			t.Fatal(err)
		}
		t.Run(decoded.Method, func(t *testing.T) { testHostDuplicateLifecycleDrainsAfterEOF(t, request, decoded.Method) })
	}
}

func testHostDuplicateLifecycleDrainsAfterEOF(t *testing.T, frame, trackedMethod string) {
	t.Helper()
	for _, id := range []string{`1`, `9007199254740993`, `9223372036854775808`, `-0`, `"duplicate"`, `"\u0000number:9007199254740993"`} {
		t.Run(id, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{
				SupportedProtocolVersions: hostProtocolVersions,
				Logger:                    slog.New(slog.NewJSONHandler(io.Discard, nil)),
			})
			server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
					if method == trackedMethod {
						select {
						case <-entered:
						default:
							close(entered)
							<-release
						}
					}
					return next(ctx, method, request)
				}
			})
			request := strings.Replace(frame, `"id":1`, `"id":`+id, 1)
			// marker의 로컬 오류는 reader가 중복 요청을 넘어서 읽었음을 보장한다.
			input := request + "\n" + request + "\n" + `{"jsonrpc":"2.0","id":"marker","method":"unsupported"}` + "\n"
			output := &discoverMarkerWriter{marker: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				done <- server.Run(ctx, &stdioTransport{in: io.NopCloser(strings.NewReader(input)), out: output})
			}()
			select {
			case <-output.marker:
			case <-ctx.Done():
				close(release)
				<-done
				t.Fatal("중복 요청 뒤 marker를 처리하지 못했습니다")
			}
			close(release)
			err := <-done
			if ctx.Err() != nil || err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("중복 discover 뒤 EOF 종료 = %v, 제한 시간 = %v", err, ctx.Err())
			}
			var successes, duplicates, lines int
			for line := range bytes.SplitSeq(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
				lines++
				var response struct {
					ID     jsontext.Value `json:"id"`
					Result map[string]any `json:"result"`
					Error  struct {
						Code int64 `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(line, &response); err != nil {
					t.Fatal(err)
				}
				if response.Result != nil {
					if string(response.ID) != id {
						t.Fatalf("완료 응답 ID = %s", response.ID)
					}
					successes++
				}
				if response.Error.Code == jsonrpc.CodeInvalidRequest {
					if string(response.ID) != id {
						t.Fatalf("중복 응답 ID = %s", response.ID)
					}
					duplicates++
				}
			}
			if successes != 1 || duplicates != 1 || lines != 3 {
				t.Fatalf("완료·중복 응답 수 = %d, %d: %s", successes, duplicates, output.String())
			}
		})
	}
}

type discoverMarkerWriter struct {
	bytes.Buffer
	marker chan struct{}
}

func (w *discoverMarkerWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, []byte(`"id":"marker"`)) {
		close(w.marker)
	}
	return n, err
}

// TestHostDiscoverWriteFailureStopsAfterEOF는 SDK가 응답 쓰기를 중단해도
// 발견 대기 수 때문에 EOF 종료가 호출 제한 시간까지 멈추지 않는지 확인한다.
func TestHostDiscoverWriteFailureStopsAfterEOF(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(strconv.FormatBool(short), func(t *testing.T) {
			var input strings.Builder
			for id := range 128 {
				input.WriteString(strings.Replace(discoverRequest, `"id":1`, `"id":`+strconv.Itoa(id+1), 1))
				input.WriteByte('\n')
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_ = Run(ctx, io.NopCloser(strings.NewReader(input.String())), discoverFailedWriter{short: short},
				slog.New(slog.NewJSONHandler(io.Discard, nil)), "test-version")
			if ctx.Err() != nil {
				t.Fatal("출력 실패 뒤 EOF 종료가 제한 시간까지 멈췄습니다")
			}
		})
	}
}

type discoverFailedWriter struct{ short bool }

func (w discoverFailedWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

func TestDiscoverWriteFailureStopsPendingRead(t *testing.T) {
	input := discoverRequest + "\n" + strings.Replace(discoverRequest, `"id":1`, `"id":2`, 1) + "\n"
	connection := newTestConnection(t, input, discoverFailedWriter{})
	first, err := connection.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(t.Context(), &jsonrpc.Response{ID: first.(*jsonrpc.Request).ID, Result: []byte(`{}`)}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("출력 실패 = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := connection.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("응답 없는 발견의 정리 = %v, 제한 시간 = %v", err, ctx.Err())
	}
}

func TestUntrackedSDKResponseDoesNotDrainDiscover(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, discoverRequest+"\n", &output)
	if _, err := connection.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	id, _ := jsonrpc.MakeID("untracked")
	if err := connection.Write(t.Context(), &jsonrpc.Response{ID: id, Result: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	connection.pendingMu.Lock()
	pending := connection.pending
	connection.pendingMu.Unlock()
	if pending != 1 {
		t.Fatalf("다른 ID의 응답이 발견 대기를 해제했습니다: %d", pending)
	}
}

func TestDiscoverAndToolIDsShareAdmission(t *testing.T) {
	for _, toolFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(toolFirst), func(t *testing.T) {
			tool := toolRequest(t, 1, "tools/call", "graph_list", jsontext.Value(`{}`))
			sentinel := strings.Replace(discoverRequest, `"id":1`, `"id":2`, 1) + "\n"
			input := discoverRequest + "\n" + sentinel
			if !toolFirst {
				input = discoverRequest + "\n" + tool + sentinel
			}
			var output bytes.Buffer
			connection := newTestConnection(t, input, &output)
			remote := &cancellationRemote{catalog: newToolRemote(t).catalog, started: make(chan struct{}), cancelled: make(chan struct{})}
			policy, _ := contract.NewPolicy("all", nil)
			connection.tools = NewTools(remote, policy, uuid.UUID{})
			if toolFirst {
				message, err := jsonrpc.DecodeMessage([]byte(tool))
				if err != nil {
					t.Fatal(err)
				}
				if err := connection.dispatch(t.Context(), message.(*jsonrpc.Request)); err != nil {
					t.Fatal(err)
				}
				<-remote.started
			} else {
				first, err := connection.Read(t.Context())
				if err != nil || first.(*jsonrpc.Request).ID.Raw() != int64(1) {
					t.Fatalf("첫 발견 = %v, %v", first, err)
				}
			}
			message, err := connection.Read(t.Context())
			if err != nil || message.(*jsonrpc.Request).ID.Raw() != int64(2) {
				t.Fatalf("충돌 뒤 발견 = %v, %v", message, err)
			}
			if !bytes.Contains(output.Bytes(), []byte(`"code":-32600`)) {
				t.Fatalf("혼합 ID 충돌을 거절하지 않았습니다: %s", output.String())
			}
			if !toolFirst {
				id, err := jsonrpc.MakeID(float64(1))
				if err != nil {
					t.Fatal(err)
				}
				if err := connection.Write(t.Context(), &jsonrpc.Response{ID: id, Result: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := connection.Write(t.Context(), &jsonrpc.Response{ID: message.(*jsonrpc.Request).ID, Result: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			connection.cancelCalls()
			connection.workers.Wait()
			if _, err := connection.Read(t.Context()); !errors.Is(err, io.EOF) {
				t.Fatalf("혼합 ID 뒤 EOF = %v", err)
			}
		})
	}
}

func TestDiscoverIDReuseAfterResponse(t *testing.T) {
	var output bytes.Buffer
	connection := newTestConnection(t, discoverRequest+"\n"+discoverRequest+"\n", &output)
	for range 2 {
		message, err := connection.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(t.Context(), &jsonrpc.Response{ID: message.(*jsonrpc.Request).ID, Result: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.Read(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("재사용 뒤 EOF = %v", err)
	}
	if bytes.Count(output.Bytes(), []byte{'\n'}) != 2 || bytes.Contains(output.Bytes(), []byte(`"error"`)) {
		t.Fatalf("완료 ID 재사용 = %s", output.String())
	}
}
