package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agent_context_sharing/internal/diagnostic"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolPanicEvents struct {
	events        chan diagnostic.Event
	panicOnReport bool
}

func (events toolPanicEvents) Emit(_ context.Context, event diagnostic.Event) bool {
	events.events <- event
	if events.panicOnReport && event.ReasonCode == "handler_panic" {
		panic("private-diagnostic-panic")
	}
	return false // 진단 수락 실패도 호출 결과를 바꾸지 않아야 한다.
}

func TestMCPToolPanicHTTP(t *testing.T) {
	for _, mode := range []string{"diagnostics", "nil_panic_without_diagnostics", "diagnostic_panic"} {
		diagnostics := mode != "nil_panic_without_diagnostics"
		t.Run(mode, func(t *testing.T) {
			for _, definition := range toolDefinitions() {
				t.Run(definition.Name, func(t *testing.T) {
					var auth, calls, attempts atomic.Int64
					cfg := httpTestConfig(t, &auth, &calls)
					normal := cfg.Handlers[definition.Name]
					cfg.Handlers[definition.Name] = func(ctx context.Context, principal Principal, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
						if attempts.Add(1) == 1 {
							if !diagnostics {
								panic(nil)
							}
							panic("private-panic-body-token")
						}
						return normal(ctx, principal, request)
					}
					events := toolPanicEvents{events: make(chan diagnostic.Event, 8), panicOnReport: mode == "diagnostic_panic"}
					if diagnostics {
						source, err := diagnostic.NewSource("panic-test")
						if err != nil {
							t.Fatal(err)
						}
						cfg.Diagnostics, err = diagnostic.NewReporter(diagnostic.Request, events, source)
						if err != nil {
							t.Fatal(err)
						}
					}
					handler, err := NewHandler(cfg)
					if err != nil {
						t.Fatal(err)
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						ctx := diagnostic.WithTrace(request.Context(), diagnostic.NewRequestTrace())
						handler.ServeHTTP(w, request.WithContext(ctx))
					}))
					t.Cleanup(server.Close)
					body := sdkProbeRequest("tools/call")
					params := body["params"].(map[string]any)
					params["name"], params["arguments"] = definition.Name, catalogSample(definition.Name)
					for attempt := range 2 {
						response := exchangeSDKProbe(t, server, http.MethodPost, body, map[string]string{"Mcp-Name": definition.Name, "Authorization": "Bearer synthetic"})
						wire := decodeSDKProbe(t, response)
						result, ok := wire["result"].(map[string]any)
						if response.Status != 200 || !ok || wire["error"] != nil || result["resultType"] != "complete" || response.Header.Get("WWW-Authenticate") != "" || response.Header.Get("Cache-Control") != "no-store" {
							t.Fatalf("도구 완료 외피: %s", response.Body)
						}
						content := result["structuredContent"].(map[string]any)
						if attempt == 0 {
							if result["isError"] != true || content["code"] != "internal" {
								t.Fatalf("panic 결과: %s", response.Body)
							}
						} else if result["isError"] == true || content["account"] == nil || calls.Load() != 1 || auth.Load() != 2 {
							t.Fatalf("panic 뒤 같은 서버의 다음 호출: %s", response.Body)
						}
						if strings.Contains(string(response.Body), "private-") || strings.Contains(string(response.Body), "goroutine") || strings.Contains(string(response.Body), "tool_panic_test.go") {
							t.Fatal("panic 원문 또는 스택 노출")
						}
					}
					panics := 0
					for len(events.events) > 0 {
						event := <-events.events
						encoded, err := json.Marshal(event)
						if err != nil || event.Validate() != nil || strings.Contains(string(encoded), "private-") {
							t.Fatal("panic 진단 안전 필드 위반")
						}
						if event.ReasonCode == "handler_panic" && event.Level == "ERROR" && event.Stage == "response" && event.Result == "failed" && event.Fields.Request.Tool == definition.Name {
							panics++
						}
					}
					if diagnostics && panics != 1 {
						t.Fatalf("panic 진단 수=%d, want 1", panics)
					}
				})
			}
		})
	}
}
