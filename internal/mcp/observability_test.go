package mcp

import (
	"agent_context_sharing/internal/diagnostic"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type obs05Events struct{ events []diagnostic.Event }

func (s *obs05Events) Emit(_ context.Context, e diagnostic.Event) bool {
	s.events = append(s.events, e)
	return false
}

func TestMCPResponseFailureReasons(t *testing.T) {
	errorResult := func(value any) *sdk.CallToolResult {
		return &sdk.CallToolResult{IsError: true, StructuredContent: value}
	}
	for _, sample := range []struct {
		name, level, outcome, reason string
		result                       *sdk.CallToolResult
		err                          error
	}{
		{"success", "INFO", "completed", "completed", &sdk.CallToolResult{}, nil},
		{"normal truncation", "INFO", "completed", "completed", &sdk.CallToolResult{StructuredContent: map[string]any{"truncated": map[string]bool{"result_truncated": true}}}, nil},
		{"permission_denied", "WARN", "rejected", "permission_denied", errorResult(map[string]any{"code": "permission_denied"}), nil},
		{"not_found", "WARN", "rejected", "not_found", errorResult(map[string]string{"code": "not_found"}), nil},
		{"invalid_argument", "WARN", "rejected", "invalid_argument", errorResult(struct {
			Code string `json:"code"`
		}{"invalid_argument"}), nil},
		{"invalid_credential", "WARN", "rejected", "invalid_credential", errorResult(jsontext.Value(`{"code":"invalid_credential"}`)), nil},
		{"limit_exceeded", "WARN", "rejected", "capacity_exceeded", errorResult(map[string]any{"code": "limit_exceeded"}), nil},
		{"version_conflict", "WARN", "rejected", "constraint_violation", errorResult(map[string]any{"code": "version_conflict"}), nil},
		{"not_supported", "WARN", "rejected", "channel_unavailable", errorResult(map[string]any{"code": "not_supported"}), nil},
		{"internal", "ERROR", "failed", "store_unavailable", errorResult(map[string]any{"code": "internal"}), nil},
		{"unexpected error", "ERROR", "failed", "store_unavailable", nil, errors.New("private-error-marker")},
		{"nil result", "ERROR", "failed", "store_unavailable", nil, nil},
		{"wrapped canceled", "INFO", "failed", "canceled", nil, &DependencyFailure{Cause: fmt.Errorf("private-error-marker: %w", context.Canceled)}},
		{"wrapped deadline", "INFO", "failed", "deadline_exceeded", nil, &DependencyFailure{Cause: fmt.Errorf("private-error-marker: %w", context.DeadlineExceeded)}},
		{"domain error", "WARN", "rejected", "not_found", nil, fmt.Errorf("private-error-marker: %w", model.ErrNotFound)},
		{"unknown code", "WARN", "rejected", "constraint_violation", errorResult(map[string]any{"code": "private-error-marker"}), nil},
		{"wrong code type", "WARN", "rejected", "constraint_violation", errorResult(map[string]any{"code": 42}), nil},
		{"nonobject result", "WARN", "rejected", "constraint_violation", errorResult([]string{"private-error-marker"}), nil},
		{"text fallback", "WARN", "rejected", "not_found", &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: `{"code":"not_found"}`}}}, nil},
		{"malformed text", "WARN", "rejected", "constraint_violation", &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: `private-error-marker`}}}, nil},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var auth, calls atomic.Int64
			cfg := httpTestConfig(t, &auth, &calls)
			cfg.Handlers["graph_list"] = func(context.Context, Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return sample.result, sample.err
			}
			sink := &obs05Events{}
			source, _ := diagnostic.NewSource("failure-reasons")
			cfg.Diagnostics, _ = diagnostic.NewReporter(diagnostic.Request, sink, source)
			handler, err := NewHandler(cfg)
			if err != nil {
				t.Fatal(err)
			}
			request := httpTestRequest(t, "tools/call")
			request = request.WithContext(diagnostic.WithTrace(request.Context(), diagnostic.NewRequestTrace()))
			handler.ServeHTTP(httptest.NewRecorder(), request)
			if len(sink.events) != 2 {
				t.Fatalf("events = %d", len(sink.events))
			}
			event := sink.events[1]
			if err := event.Validate(); err != nil || event.Level != sample.level || event.Result != sample.outcome || event.ReasonCode != sample.reason {
				t.Fatalf("event = %+v, validation = %v", event, err)
			}
			data, err := json.Marshal(sink.events)
			if err != nil || strings.Contains(string(data), "private-error-marker") {
				t.Fatal("진단에 원문 오류 또는 알 수 없는 code 누출")
			}
		})
	}
}

func TestMCPResponseRequestCancellationPrecedesInternalResult(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx := t.Context()
		reason := "canceled"
		var cancel context.CancelFunc
		if deadline {
			ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			reason = "deadline_exceeded"
		} else {
			ctx, cancel = context.WithCancel(ctx)
		}
		cancel()
		level, outcome, gotReason := toolResponseObservation(ctx, &sdk.CallToolResult{IsError: true, StructuredContent: map[string]any{"code": "internal"}}, &DependencyFailure{})
		if level != "INFO" || outcome != "failed" || gotReason != reason {
			t.Fatalf("observation = %s %s %s", level, outcome, gotReason)
		}
	}
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
