package remote

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
	"agent_context_sharing/client/internal/client/host"
)

// TestCallToolPreservesCompletedResponse는 전송이 결과를 반환할 때 취소·예산 만료가
// 발생해도 완료 결과를 재시도 가능한 오류로 대체하지 않는지 확인한다.
func TestCallToolPreservesCompletedResponse(t *testing.T) {
	for _, tool := range []struct{ name, arguments string }{
		{"graph_list", `{}`},
		{"graph_create", `{"name":"완료된 쓰기"}`},
	} {
		for _, action := range []string{"cancel", "request_timeout"} {
			for _, response := range []struct {
				name string
				raw  jsontext.Value
				want error
			}{
				{"success", jsontext.Value(`{"content":[],"structuredContent":{"version":9007199254740993,"cursor":"opaque+/=","truncated":true},"isError":false}`), nil},
				{"domain_error", jsontext.Value(`{"isError":true,"structuredContent":{"error":{"code":"version_conflict","version":9007199254740993}},"content":[]}`), nil},
				{"invalid_result", jsontext.Value(`[]`), contract.ErrProtocol},
			} {
				t.Run(tool.name+"/"+action+"/"+response.name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					source := &completedCallSource{catalogSource: newCatalogSource(t)}
					source.result = response.raw
					client := New(source, nil)
					// 목록 준비를 먼저 끝내 호출 예산 만료를 도구 결과 반환 경계에 둔다.
					if _, err := client.ListTools(t.Context()); err != nil {
						t.Fatal(err)
					}
					if action == "request_timeout" {
						client.timeout = 30 * time.Millisecond
					}
					source.complete = func(ctx context.Context) {
						want := error(context.Canceled)
						if action == "cancel" {
							cancel()
						} else {
							<-ctx.Done()
							want = context.DeadlineExceeded
						}
						if !errors.Is(ctx.Err(), want) {
							t.Fatalf("결과 반환 경계의 context = %v, want %v", ctx.Err(), want)
						}
					}
					policy, err := contract.NewPolicy("all", nil)
					if err != nil {
						t.Fatal(err)
					}
					relay := host.NewTools(client, policy, uuid.NewV7())
					result, err := relay.CallTool(ctx, tool.name, jsontext.Value(tool.arguments))
					if response.want != nil {
						if result != nil || !errors.Is(err, response.want) || contract.ClassifyError(err).Retryable {
							t.Fatalf("잘못된 완료 결과의 분류 = %v, %v", result, err)
						}
					} else if err != nil || result == nil || !bytes.Equal(result.Raw(), response.raw) {
						t.Fatalf("완료 결과를 변경하거나 폐기했다: %v, %v", result, err)
					}
					if source.calls.Load() != 1 {
						t.Fatalf("완료된 호출을 재전송했다: %d회", source.calls.Load())
					}
				})
			}
		}
	}
}

// TestCallToolCancellationBeforeSend는 완료 결과가 없고 전송 전 취소된 쓰기는 여전히
// timeout으로 분류하며 원격 도구를 호출하지 않는지 확인한다.
func TestCallToolCancellationBeforeSend(t *testing.T) {
	source := newCatalogSource(t)
	client := New(source, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := client.CallTool(ctx, "graph_create", jsontext.Value(`{"name":"전달 전"}`))
	classified := contract.ClassifyError(err)
	if result != nil || !errors.Is(err, context.Canceled) || classified.Code != "client_timeout" || !classified.Retryable || source.calls.Load() != 0 {
		t.Fatalf("전달 전 취소 = %v, %v, %d회", result, err, source.calls.Load())
	}
}

// TestHostCompletedWriteSurvivesLateCancellation은 검증된 HTTP 결과가 전송 반환 직전
// 취소되어도 stdio 호스트에 원래 결과로 전달되고 새 HTTP 쓰기를 만들지 않는지 확인한다.
func TestHostCompletedWriteSurvivesLateCancellation(t *testing.T) {
	fixture := newCatalogSource(t)
	auth := readyAuthentication()
	var calls int
	client := newHTTPTestClient(t, auth, transportFunc(func(request *http.Request) (*http.Response, error) {
		id := wireID(t, request)
		switch request.Header.Get("Mcp-Method") {
		case "server/discover":
			return rpcResponse(id, fixture.discovery), nil
		case "tools/list":
			return rpcResponse(id, fixture.tools), nil
		default:
			calls++
			return rpcResponse(id, fixture.result), nil
		}
	}), HTTPOptions{})
	// 갱신 훅은 도구 응답에서만 발생시킨다. 목록 준비의 취소 정책은 바꾸지 않는다.
	if _, err := client.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	auth.onRefresh = func(ctx context.Context) error {
		// 외피와 취소 검사가 끝난 전송의 마지막 동작에서 호출 예산을 취소한다.
		ctx.Value(stateKey{}).(*callState).cancel()
		return nil
	}
	policy, err := contract.NewPolicy("all", nil)
	if err != nil {
		t.Fatal(err)
	}
	line := stdioExchange(t, host.NewTools(client, policy, uuid.NewV7()), `{"jsonrpc":"2.0","id":"completed-write","method":"tools/call","params":{"name":"graph_create","arguments":{"name":"완료된 쓰기"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if calls != 1 || !strings.Contains(line, `"id":"completed-write"`) || !strings.Contains(line, `"version":9007199254740993`) || strings.Contains(line, `"client_error"`) || strings.Contains(line, `"error":`) {
		t.Fatalf("완료된 쓰기의 호스트 결과 = %s, %d회", line, calls)
	}
}

// completedCallSource는 전송이 확보한 결과를 준비한 뒤 반환 직전에 취소를 발생시킨다.
// 완료 여부가 이미 결정된 전송과 계약 계층 사이의 경쟁 창을 고정하는 대역이다.
type completedCallSource struct {
	*catalogSource
	complete func(context.Context)
}

func (source *completedCallSource) CallTool(ctx context.Context, _ string, _ jsontext.Value) (jsontext.Value, error) {
	source.calls.Add(1)
	raw := bytes.Clone(source.result)
	source.complete(ctx)
	return raw, source.callError
}
