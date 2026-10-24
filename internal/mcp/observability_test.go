package mcp

import (
	"agent_context_sharing/internal/diagnostic"
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type obs05Events struct{ events []diagnostic.Event }

func (s *obs05Events) Emit(_ context.Context, e diagnostic.Event) bool {
	s.events = append(s.events, e)
	return false
}
func TestOBS05MCPToolBoundary(t *testing.T) {
	var auth, calls atomic.Int64
	cfg := httpTestConfig(t, &auth, &calls)
	sink := &obs05Events{}
	source, _ := diagnostic.NewSource("obs05")
	reporter, _ := diagnostic.NewReporter(diagnostic.Request, sink, source)
	cfg.Diagnostics = reporter
	handler, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := httpTestRequest(t, "tools/call")
	request = request.WithContext(diagnostic.WithTrace(request.Context(), diagnostic.NewRequestTrace()))
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if calls.Load() != 1 || len(sink.events) != 2 {
		t.Fatalf("실제 도구 경계 누락: calls=%d events=%d", calls.Load(), len(sink.events))
	}
	for _, e := range sink.events {
		if e.Validate() != nil || e.Fields.Request.Tool != "graph_list" {
			t.Fatal("도구 계측 필드 위반")
		}
	}
}
