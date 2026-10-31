package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWriteFailureMapping(t *testing.T) {
	for _, sample := range []struct {
		name          string
		err           error
		code, field   string
		deterministic bool
	}{
		{"field", fmt.Errorf("validation: %w", model.FieldError{Field: "body", Message: "private input"}), "invalid_argument", "body", true},
		{"supersedes field", model.FieldError{Field: "supersedes"}, "invalid_argument", "supersedes_context_id", true},
		{"locator field", model.FieldError{Field: "locator"}, "invalid_argument", "source_ref.locator", true},
		{"channel field", model.FieldError{Field: "source_channel"}, "invalid_argument", "source_ref.channel", true},
		{"grade", perm.DeniedError{Required: model.GraphGradeEditor}, "permission_denied", "", true},
		{"hidden", perm.NotFoundError{}, "not_found", "", true},
		{"absent", model.ErrNotFound, "not_found", "", true},
		{"conflict", model.VersionConflictError{Current: 9007199254740993}, "version_conflict", "", true},
		{"limit", plan.LimitError{Name: "stored_characters_per_graph", Current: 12, Allowed: 10}, "limit_exceeded", "", true},
		{"state", model.ErrInvalidState, "invalid_argument", "state", true},
		{"web", RestoreChannelError{}, "not_supported", "", true},
		{"internal", errors.New("private SQL"), "internal", "", false},
		{"wrapped storage field", &DependencyFailure{Cause: model.FieldError{Field: "body", Message: "private SQL"}}, "internal", "", false},
		{"agent storage field", &agentFailure{cause: model.FieldError{Field: "agent_id", Message: "private input"}}, "internal", "", false},
		{"cleanup failure", errors.Join(model.ErrNotFound, &DependencyFailure{Cause: errors.New("private cleanup")}), "internal", "", false},
		{"cancel", errors.Join(model.ErrNotFound, context.Canceled), "internal", "", false},
		{"timeout", context.DeadlineExceeded, "internal", "", false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			value, deterministic := writeFailureObject(sample.err)
			if value["code"] != sample.code || deterministic != sample.deterministic || sample.field != "" && value["field"] != sample.field {
				t.Fatalf("도메인 사상: %v %v", value, deterministic)
			}
			data, _ := json.Marshal(value)
			if strings.Contains(string(data), "private") {
				t.Fatal("내부 원문 노출")
			}
			result, err := writeErrorResult(sample.err)
			if err != nil || !result.IsError {
				t.Fatal("도구 오류 외피", err)
			}
			if sample.name == "conflict" && !strings.Contains(result.Content[0].(*sdk.TextContent).Text, "9007199254740993") {
				t.Fatal("판 정밀도")
			}
		})
	}
}

// TestRelationEndpointFailureGuidance는 관계 끝점 거부가 기존 코드·필드를 유지하면서 원인별
// 안내를 돌려주고, 다른 필드 거부는 기존 고정 문구를 유지하는지 확인한다.
func TestRelationEndpointFailureGuidance(t *testing.T) {
	for _, sample := range []struct {
		name, field, want string
		cause             error
	}{
		{"not_event", "from_context_id", "node_create의 derived_from", model.ErrRelationEndpointNotEvent},
		{"not_event_to", "to_context_id", "앞서 만든 노드는 그대로 남습니다", model.ErrRelationEndpointNotEvent},
		{"discarded", "to_context_id", "폐기된 사건", model.ErrRelationEndpointDiscarded},
		{"time_order", "from_context_id", "precedes·causes", model.ErrRelationTimeOrder},
		{"other", "body", "도구 입력을 확인해 주십시오.", nil},
	} {
		t.Run(sample.name, func(t *testing.T) {
			err := fmt.Errorf("관계 검증: %w", model.FieldError{Field: sample.field, Message: "private detail", Cause: sample.cause})
			value, deterministic := writeFailureObject(err)
			message, _ := value["message"].(string)
			if !deterministic || value["code"] != "invalid_argument" || value["field"] != sample.field || !strings.Contains(message, sample.want) || strings.Contains(message, "private") {
				t.Fatalf("끝점 안내 = %v", value)
			}
		})
	}
}

func TestPatchNodeOptionalValuesAndLayers(t *testing.T) {
	at := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	previous := model.Context{Layer: model.LayerDerived, Body: "원문", Version: 3, Derived: &model.DerivedAttributes{Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateOpinion, ConfidenceState: model.ConfidenceStateSupported, ValidFrom: &at, ValidTo: &at}}
	next, err := patchNode(previous, map[string]any{"body": "새 본문", "valid_to": nil})
	if err != nil || next.Version != 4 || next.Derived.ValidFrom != previous.Derived.ValidFrom || next.Derived.ValidTo != nil || previous.Derived.ValidTo == nil || previous.Body != "원문" {
		t.Fatal("생략/null/불변 값 병합", err)
	}
	for _, field := range []string{"member_refs", "event_start", "event_end"} {
		t.Run(field, func(t *testing.T) {
			if _, err := patchNode(previous, map[string]any{field: nil}); err == nil {
				t.Fatal("다른 계층 입력 허용")
			}
		})
	}
	previous.Layer = model.LayerSource
	if _, err := patchNode(previous, map[string]any{"body": "변경"}); err == nil {
		t.Fatal("원천 변경 허용")
	}
	previous = model.Context{Layer: model.LayerEvent, Version: 4, Event: &model.EventAttributes{Start: at, End: &at}}
	next, err = patchNode(previous, map[string]any{"event_end": nil})
	if err != nil || next.Event.End != nil || previous.Event.End == nil {
		t.Fatal("사건 종료 null", err)
	}
	for _, field := range []string{"confidence_state", "valid_from", "valid_to"} {
		t.Run(field, func(t *testing.T) {
			if _, err := patchNode(previous, map[string]any{field: nil}); err == nil {
				t.Fatal("다른 계층 입력 허용")
			}
		})
	}
}
