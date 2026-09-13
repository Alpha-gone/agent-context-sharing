package mcp

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// TestInvalidArgumentCarriesViolatedField는 SDD의 「오류 부가 정보」대로 invalid_argument가
// 위반한 필드를 함께 싣는지 확인한다. 코드만 돌려주면 호출자가 무엇을 고칠지 알 수 없다.
func TestInvalidArgumentCarriesViolatedField(t *testing.T) {
	tests := map[string]struct {
		err   error
		field string
	}{
		"mcp 검증":      {err: &argumentError{Field: "occurred_at"}, field: "occurred_at"},
		"model 검증":    {err: model.FieldError{Field: "evidence_state", Message: "근거 상태가 올바르지 않다"}, field: "evidence_state"},
		"감싼 model 검증": {err: fmt.Errorf("다음 컨텍스트: %w", model.FieldError{Field: "summary_scope", Message: "요약 범위"}), field: "summary_scope"},
		"필드 없음":       {err: errors.New("이름 없는 검증 실패"), field: ""},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			domain, ok := errors.AsType[*Error](invalidArgument(test.err))
			if !ok || domain.Code != "invalid_argument" {
				t.Fatalf("도메인 오류 = %#v, want invalid_argument", domain)
			}
			field, _ := domain.Data["field"].(string)
			if field != test.field {
				t.Fatalf("field = %q, want %q", field, test.field)
			}
		})
	}
}

// TestModelValidationNamesViolatedField는 계층별 검증이 필드 이름을 담는지 확인한다.
// 이름이 없으면 위 사상이 코드만 돌려주므로 두 곳을 함께 지켜야 한다.
func TestModelValidationNamesViolatedField(t *testing.T) {
	value := testDerivedContext(t)
	value.Derived.EvidenceState = "잘못된 근거 상태"
	field, ok := errors.AsType[model.FieldError](value.Validate())
	if !ok || field.Field != "evidence_state" {
		t.Fatalf("파생 검증 필드 = %#v, want evidence_state", field)
	}

	event := testEventContext(t)
	event.Event.MemberIDs = nil
	field, ok = errors.AsType[model.FieldError](event.Validate())
	if !ok || field.Field != "member_refs" {
		t.Fatalf("사건 검증 필드 = %#v, want member_refs", field)
	}
}

// TestUpdatedContextDoesNotMutatePrevious는 갱신본을 만들 때 기존 컨텍스트가 함께 바뀌지
// 않는지 확인한다. 계층 속성이 포인터라 얕은 복사로 두면 model.ValidateUpdate의 불변
// 검사가 같은 객체를 양쪽에서 읽어 항상 통과한다.
func TestUpdatedContextDoesNotMutatePrevious(t *testing.T) {
	previous := testDerivedContext(t)
	previous.Derived.EvidenceState = model.EvidenceStateOpinion
	previous.Derived.ConfidenceState = model.ConfidenceStateSupported

	next, err := updatedContext(previous, map[string]any{"confidence_state": "disputed"})
	if err != nil {
		t.Fatalf("파생 갱신본 생성: %v", err)
	}
	if next.Derived == previous.Derived {
		t.Fatal("갱신본이 기존 파생 속성과 같은 객체를 가리킨다")
	}
	if previous.Derived.ConfidenceState != model.ConfidenceStateSupported {
		t.Fatalf("기존 신뢰 상태가 바뀌었다: %q", previous.Derived.ConfidenceState)
	}
	if next.Derived.ConfidenceState != model.ConfidenceStateDisputed {
		t.Fatalf("갱신본 신뢰 상태 = %q, want disputed", next.Derived.ConfidenceState)
	}
	// 불변 속성을 실제로 바꾸면 검사가 걸려야 한다. 포인터를 공유하면 여기가 통과한다.
	next.Derived.Kind = model.DerivationKindSummary
	next.Derived.SummaryScope = model.SummaryScopeLocal
	if err := model.ValidateUpdate(previous, next); err == nil {
		t.Fatal("파생 종류 변경이 불변 검사를 통과했다")
	}

	event := testEventContext(t)
	original := append([]model.ID(nil), event.Event.MemberIDs...)
	updated, err := updatedContext(event, map[string]any{"member_refs": []any{newTestMemberID(t).String()}})
	if err != nil {
		t.Fatalf("사건 갱신본 생성: %v", err)
	}
	if updated.Event == event.Event {
		t.Fatal("갱신본이 기존 사건 속성과 같은 객체를 가리킨다")
	}
	if len(event.Event.MemberIDs) != len(original) || event.Event.MemberIDs[0] != original[0] {
		t.Fatal("기존 사건 구성원이 바뀌었다")
	}
}

func newTestMemberID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("식별자 생성: %v", err)
	}
	return id
}

func testDerivedContext(t *testing.T) model.Context {
	t.Helper()
	now := time.Now().UTC()
	return model.Context{
		ID: newTestMemberID(t), GraphID: newTestMemberID(t), Layer: model.LayerDerived, Body: "파생 본문",
		RecordedAt: now, CreatedBy: newTestMemberID(t), CreatedByAgent: newTestMemberID(t), Version: 1,
		Derived: &model.DerivedAttributes{Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateObservation},
	}
}

func testEventContext(t *testing.T) model.Context {
	t.Helper()
	now := time.Now().UTC()
	return model.Context{
		ID: newTestMemberID(t), GraphID: newTestMemberID(t), Layer: model.LayerEvent, Body: "사건 본문",
		RecordedAt: now, CreatedBy: newTestMemberID(t), CreatedByAgent: newTestMemberID(t), Version: 1,
		Event: &model.EventAttributes{MemberIDs: []model.ID{newTestMemberID(t)}, Start: now},
	}
}

// TestRejectReasonKeepsDomainCode는 거부 기록의 사유가 실제 거부 코드를 유지하는지
// 확인한다. 채널 경계 거부까지 invalid_argument로 뭉개면 감사 기록에서 사유를 가릴 수 없다.
func TestRejectReasonKeepsDomainCode(t *testing.T) {
	tests := map[string]struct {
		cause  error
		reason string
	}{
		"채널 경계":   {cause: &Error{Code: "not_supported"}, reason: "not_supported"},
		"대상 없음":   {cause: store.ErrNotFound, reason: "not_found"},
		"판 충돌":    {cause: store.VersionConflictError{Current: 3}, reason: "version_conflict"},
		"확정 코드 밖": {cause: &Error{Code: "made_up"}, reason: "invalid_argument"},
		"그 밖의 거부": {cause: errors.New("컨텍스트가 이미 폐기됐다"), reason: "invalid_argument"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if reason := rejectReason(test.cause); reason != test.reason {
				t.Fatalf("거부 사유 = %q, want %q", reason, test.reason)
			}
		})
	}
}

// TestRelationErrorsCarryViolatedField는 관계 계층의 거부도 위반한 필드를 싣는지 확인한다.
// store가 감싸고 model이 이름을 담으므로 두 겹을 지나서도 필드가 살아남아야 한다.
func TestRelationErrorsCarryViolatedField(t *testing.T) {
	tests := map[string]struct {
		err   error
		field string
	}{
		"관계 제약 위반": {
			err: fmt.Errorf("관계 검증: %w", errors.Join(store.ErrInvalidRelation,
				model.FieldError{Field: "to_context_id", Message: "관계의 양 끝은 같을 수 없다"})),
			field: "to_context_id",
		},
		"상태 전이 거부": {
			err: fmt.Errorf("관계가 이미 폐기됐다: %w", errors.Join(store.ErrInvalidState,
				model.FieldError{Field: "relation_id", Message: "관계가 이미 폐기됐다"})),
			field: "relation_id",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			domain, ok := errors.AsType[*Error](mapError(test.err))
			if !ok || domain.Code != "invalid_argument" {
				t.Fatalf("도메인 오류 = %#v, want invalid_argument", domain)
			}
			if field, _ := domain.Data["field"].(string); field != test.field {
				t.Fatalf("field = %q, want %q", field, test.field)
			}
		})
	}
}
