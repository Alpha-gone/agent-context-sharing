package model

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestNewIDCreatesUUIDv7은 생성한 식별자가 UUIDv7 형식과 UTC 시각을 보존하는지 확인한다.
func TestNewIDCreatesUUIDv7(t *testing.T) {
	at := time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC)
	id, err := newIDAt(at, bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatalf("식별자 생성: %v", err)
	}
	if !id.IsV7() {
		t.Fatalf("UUIDv7이 아니다: %s", id)
	}
	got, err := id.Time()
	if err != nil {
		t.Fatalf("식별자 시각: %v", err)
	}
	if !got.Equal(at) {
		t.Fatalf("식별자 시각 = %s, want %s", got, at)
	}
	if _, err := ParseID(id.String()); err != nil {
		t.Fatalf("식별자 문자열 해석: %v", err)
	}
}

// TestContextValidate은 계층별 필수 속성과 상태 조합이 강제되는지 확인한다.
func TestContextValidate(t *testing.T) {
	now := time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC)
	context := sourceContext(t, now)
	if err := context.Validate(); err != nil {
		t.Fatalf("유효한 원천 검증: %v", err)
	}
	context.Version = 2
	if err := context.Validate(); err == nil {
		t.Fatal("원천의 변경 판 번호가 허용됐다")
	}

	derived := baseContext(t, now)
	derived.Layer = LayerDerived
	derived.Derived = new(DerivedAttributes{
		Kind:            DerivationKindSummary,
		SummaryScope:    SummaryScopeLocal,
		EvidenceState:   EvidenceStateOpinion,
		ConfidenceState: ConfidenceStateSupported,
	})
	if err := derived.Validate(); err != nil {
		t.Fatalf("유효한 파생 검증: %v", err)
	}
	derived.Derived.ConfidenceState = ""
	if err := derived.Validate(); err == nil {
		t.Fatal("의견 파생의 신뢰 상태 누락이 허용됐다")
	}
	if err := ValidateDerivedReferences([]ID{newTestID(t, now)}); err != nil {
		t.Fatalf("파생 근거 검증: %v", err)
	}
}

// TestValidateEventMembers은 사건이 원천·파생만 구성원으로 받고 원천 시각 범위를 확인하는지 검증한다.
func TestValidateEventMembers(t *testing.T) {
	now := time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC)
	source := sourceContext(t, now)
	event := eventContext(t, now, now.Add(-time.Minute))
	event.GraphID = source.GraphID
	event.Event.MemberIDs = []ID{source.ID}
	event.Event.End = new(now)
	if err := ValidateEventMembers(event, []Context{source}); err != nil {
		t.Fatalf("유효한 사건 구성원 검증: %v", err)
	}
	event.Event.Start = now.Add(time.Minute)
	if err := ValidateEventMembers(event, []Context{source}); err == nil {
		t.Fatal("시간 범위 밖 원천 구성원이 허용됐다")
	}
	event.Event.Start = now.Add(-time.Minute)
	otherGraph := source
	otherGraph.GraphID = newTestID(t, now)
	if err := ValidateEventMembers(event, []Context{otherGraph}); err == nil {
		t.Fatal("다른 그래프 사건 구성원이 허용됐다")
	}
}

// TestValidateUpdate은 원천 불변성과 파생 판 번호 증가 규칙을 확인한다.
func TestValidateUpdate(t *testing.T) {
	now := time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC)
	source := sourceContext(t, now)
	updatedSource := source
	updatedSource.Body = "바뀐 본문"
	if err := ValidateUpdate(source, updatedSource); err == nil {
		t.Fatal("원천 본문 변경이 허용됐다")
	}

	derived := baseContext(t, now)
	derived.Layer = LayerDerived
	derived.Derived = new(DerivedAttributes{Kind: DerivationKindProposition, EvidenceState: EvidenceStateObservation})
	updatedDerived := derived
	updatedDerived.Body = "새 본문"
	updatedDerived.Version = 2
	if err := ValidateUpdate(derived, updatedDerived); err != nil {
		t.Fatalf("유효한 파생 갱신: %v", err)
	}
	updatedDerived = derived
	updatedDerived.Version = 2
	updatedDerived.Derived = new(*derived.Derived)
	updatedDerived.Derived.EvidenceInvalidated = true
	if err := ValidateUpdate(derived, updatedDerived); err == nil {
		t.Fatal("파생 갱신으로 근거 무효 표시 변경이 허용됐다")
	}
	if err := ValidateEvidenceInvalidation(derived, updatedDerived); err != nil {
		t.Fatalf("유효한 근거 무효 표시: %v", err)
	}
}

// TestGraphValidate는 그래프 속성의 필수 값과 저장 문자 수 제약을 확인한다.
func TestGraphValidate(t *testing.T) {
	now := time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC)
	graph := Graph{
		ID:             newTestID(t, now),
		Name:           "프로젝트",
		CreatedBy:      newTestID(t, now),
		CreatedAt:      now,
		LastActivityAt: now,
		StoredChars:    0,
		Version:        1,
	}
	if err := graph.Validate(); err != nil {
		t.Fatalf("유효한 그래프 검증: %v", err)
	}
	graph.StoredChars = -1
	if err := graph.Validate(); err == nil {
		t.Fatal("음수 저장 문자 수가 허용됐다")
	}
}

// TestValidateDerivedReferencesRequiresAtLeastOne은 근거 없는 파생이 거부되고 상한은 이
// 계층이 보지 않는지 확인한다. 「입력 검증」이 개수 상한을 mcp에 맡겼으므로 상한을 넘는
// 목록도 여기에서는 통과해야 하며, 그래야 같은 값이 두 곳에 박히지 않는다.
func TestValidateDerivedReferencesRequiresAtLeastOne(t *testing.T) {
	now := time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC)
	if err := ValidateDerivedReferences(nil); err == nil {
		t.Fatal("근거 없는 파생이 허용됐다")
	}
	references := make([]ID, 101)
	for index := range references {
		references[index] = newTestID(t, now)
	}
	if err := ValidateDerivedReferences(references); err != nil {
		t.Fatalf("상한을 model이 보고 있다: %v", err)
	}
}

// TestEventMembersRequireAtLeastOne은 구성원 없는 사건이 거부되는지 확인한다. 구성원 수의
// 상한은 「입력 검증」이 mcp에 맡겼으므로 이 계층이 보지 않는다.
func TestEventMembersRequireAtLeastOne(t *testing.T) {
	now := time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC)
	event := eventContext(t, now, now.Add(-time.Minute))
	event.Event.MemberIDs = nil
	if err := event.Validate(); err == nil {
		t.Fatal("구성원 없는 사건이 허용됐다")
	}
}

// TestValidateRelation은 사건 관계의 시간 제약을 확인한다.
func TestValidateRelation(t *testing.T) {
	now := time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC)
	from := eventContext(t, now, now)
	to := eventContext(t, now.Add(time.Hour), now.Add(time.Hour))
	to.GraphID = from.GraphID
	confirmedAt := now.Add(2 * time.Hour)
	relation := Relation{
		ID:               newTestID(t, now),
		GraphID:          from.GraphID,
		Type:             RelationTypePrecedes,
		FromContextID:    from.ID,
		ToContextID:      to.ID,
		State:            RelationStateConfirmed,
		ProposedBy:       ProposalSourceAgent,
		ProposedAt:       now,
		ConfirmedBy:      newTestID(t, now),
		ConfirmedByAgent: newTestID(t, now),
		ConfirmedAt:      new(confirmedAt),
	}
	if err := ValidateRelation(relation, from, to); err != nil {
		t.Fatalf("유효한 관계 검증: %v", err)
	}
	relation.Type = RelationTypePrecedes
	relation.FromContextID, relation.ToContextID = relation.ToContextID, relation.FromContextID
	if err := ValidateRelation(relation, to, from); err == nil {
		t.Fatal("시간 순서가 뒤집힌 precedes가 허용됐다")
	}
	if err := ValidateRelationCycle(relation, func(ID, ID) bool { return true }); err == nil {
		t.Fatal("순환 관계가 허용됐다")
	}
}

// baseContext는 계층 속성만 덧붙이면 검증할 수 있는 공통 컨텍스트를 만든다.
func baseContext(t *testing.T, now time.Time) Context {
	t.Helper()
	return Context{
		ID:             newTestID(t, now),
		GraphID:        newTestID(t, now),
		Body:           "본문",
		RecordedAt:     now,
		CreatedBy:      newTestID(t, now),
		CreatedByAgent: newTestID(t, now),
		Version:        1,
	}
}

// sourceContext는 유효한 원천 컨텍스트를 만든다.
func sourceContext(t *testing.T, now time.Time) Context {
	t.Helper()
	context := baseContext(t, now)
	context.Layer = LayerSource
	context.Source = new(SourceAttributes{
		Reference:  SourceReference{Channel: SourceChannelConversation, Locator: "conversation://example/1"},
		OccurredAt: now,
		OriginKind: OriginKindUserUtterance,
	})
	return context
}

// eventContext는 유효한 사건 컨텍스트를 만든다.
func eventContext(t *testing.T, now, start time.Time) Context {
	t.Helper()
	context := baseContext(t, now)
	context.Layer = LayerEvent
	context.Event = new(EventAttributes{MemberIDs: []ID{newTestID(t, now)}, Start: start})
	return context
}

// newTestID는 테스트 실패를 즉시 보고하는 UUIDv7 생성 도우미다.
func newTestID(t *testing.T, at time.Time) ID {
	t.Helper()
	_ = at
	id, err := NewID()
	if err != nil {
		t.Fatalf("시험 식별자 생성: %v", err)
	}
	return id
}

// TestParseIDRejectsNonStandardHyphens는 하이픈이 표준 배치가 아닌 UUID 표기를 거부하는지
// 확인한다. 위치와 무관하게 지우면 같은 식별자가 여러 문자열로 들어와 기록과 응답의
// 표기가 어긋난다.
func TestParseIDRejectsNonStandardHyphens(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	canonical := id.String()
	compact := strings.ReplaceAll(canonical, "-", "")
	// 표준 표기와 하이픈 없는 표기는 받는다.
	for _, raw := range []string{canonical, compact} {
		if parsed, err := ParseID(raw); err != nil || parsed != id {
			t.Fatalf("정상 표기 %q 해석 = %v, %v", raw, parsed, err)
		}
	}
	// 자리를 옮긴 하이픈은 거부한다.
	for _, raw := range []string{
		compact[:1] + "-" + compact[1:],
		compact[:4] + "-" + compact[4:12] + "-" + compact[12:16] + "-" + compact[16:20] + "-" + compact[20:],
		"-" + compact,
	} {
		if _, err := ParseID(raw); err == nil {
			t.Fatalf("비정규 표기 %q가 통과했다", raw)
		}
	}
}
