package model

import (
	"fmt"
	"slices"
	"time"
)

// RelationType는 사건 컨텍스트 사이의 확정 가능한 관계 유형이다.
type RelationType string

const (
	// RelationTypePrecedes는 앞선 사건에서 뒤따르는 사건으로 가는 시간 관계다.
	RelationTypePrecedes RelationType = "precedes"
	// RelationTypeCauses는 원인 사건에서 결과 사건으로 가는 인과 관계다.
	RelationTypeCauses RelationType = "causes"
	// RelationTypePartOf는 부분 사건에서 전체 사건으로 가는 포함 관계다.
	RelationTypePartOf RelationType = "part_of"
	// RelationTypeRelatesTo는 정규화한 양 끝을 쓰는 대칭 의미 관계다.
	RelationTypeRelatesTo RelationType = "relates_to"
)

// RelationState는 관계 제안의 확정 상태를 나타낸다.
type RelationState string

const (
	// RelationStateProposed는 아직 에이전트가 확정하지 않은 후보 상태다.
	RelationStateProposed RelationState = "proposed"
	// RelationStateConfirmed는 에이전트가 확정한 관계 상태다.
	RelationStateConfirmed RelationState = "confirmed"
	// RelationStateDiscarded는 폐기돼 탐색하지 않는 관계 상태다.
	RelationStateDiscarded RelationState = "discarded"
)

// ProposalSource는 관계 후보를 만든 주체의 종류를 나타낸다.
type ProposalSource string

const (
	// ProposalSourceSystem은 시스템 신호가 만든 후보를 뜻한다.
	ProposalSourceSystem ProposalSource = "system"
	// ProposalSourceAgent는 에이전트가 직접 만든 후보를 뜻한다.
	ProposalSourceAgent ProposalSource = "agent"
)

// Relation은 같은 그래프 안의 사건 컨텍스트 사이 관계다.
type Relation struct {
	// ID 필드는 관계의 전역 UUIDv7 식별자다.
	ID ID
	// GraphID 필드는 양 끝 사건이 함께 속한 그래프 식별자다.
	GraphID ID
	// Type 필드는 관계의 방향과 시간 제약을 정한다.
	Type RelationType
	// FromContextID 필드는 관계 시작 사건 식별자다.
	FromContextID ID
	// ToContextID 필드는 관계 대상 사건 식별자다.
	ToContextID ID
	// State 필드는 제안, 확정 또는 폐기 상태다.
	State RelationState
	// ProposedBy 필드는 후보를 만든 시스템 또는 에이전트다.
	ProposedBy ProposalSource
	// ProposedAt 필드는 후보가 기록된 UTC 시각이다.
	ProposedAt time.Time
	// ConfirmedBy 필드는 확정한 계정 식별자이며 confirmed에서 필수다.
	ConfirmedBy ID
	// ConfirmedByAgent 필드는 계정을 대행해 확정한 에이전트 식별자다.
	ConfirmedByAgent ID
	// ConfirmedAt 필드는 관계를 확정한 UTC 시각이다.
	ConfirmedAt *time.Time
	// DeletedAt 필드는 관계를 폐기한 UTC 시각이다.
	DeletedAt *time.Time
}

// ValidateRelation은 사건 양 끝과 관계 속성의 공통·유형별 제약을 확인한다.
func ValidateRelation(relation Relation, from, to Context) error {
	if !relation.ID.IsV7() || !relation.GraphID.IsV7() || !relation.FromContextID.IsV7() || !relation.ToContextID.IsV7() {
		return fieldErrorf("relation_id", "관계 식별자는 UUIDv7이어야 한다")
	}
	if relation.FromContextID == relation.ToContextID {
		return fieldErrorf("to_context_id", "관계의 양 끝은 같을 수 없다")
	}
	if !isUTC(relation.ProposedAt) {
		return fieldErrorf("proposed_at", "관계 제안 시각은 UTC여야 한다")
	}
	if !slices.Contains([]RelationType{RelationTypePrecedes, RelationTypeCauses, RelationTypePartOf, RelationTypeRelatesTo}, relation.Type) {
		return fieldErrorf("relation_type", "관계 유형 %q가 허용된 값이 아니다", relation.Type)
	}
	if !slices.Contains([]RelationState{RelationStateProposed, RelationStateConfirmed, RelationStateDiscarded}, relation.State) {
		return fieldErrorf("state", "관계 상태 %q가 허용된 값이 아니다", relation.State)
	}
	if !slices.Contains([]ProposalSource{ProposalSourceSystem, ProposalSourceAgent}, relation.ProposedBy) {
		return fieldErrorf("proposed_by", "관계 후보 출처 %q가 허용된 값이 아니다", relation.ProposedBy)
	}
	if relation.State == RelationStateConfirmed {
		if !relation.ConfirmedBy.IsV7() || !relation.ConfirmedByAgent.IsV7() || relation.ConfirmedAt == nil || !isUTC(*relation.ConfirmedAt) {
			return fieldErrorf("created_by_agent", "확정 관계에는 계정, 에이전트와 UTC 확정 시각이 필요하다")
		}
	} else if !relation.ConfirmedBy.IsZero() || !relation.ConfirmedByAgent.IsZero() || relation.ConfirmedAt != nil {
		return fieldErrorf("state", "확정되지 않은 관계에는 확정 정보를 둘 수 없다")
	}
	if relation.State == RelationStateDiscarded && (relation.DeletedAt == nil || !isUTC(*relation.DeletedAt)) {
		return fieldErrorf("state", "폐기 관계에는 UTC 폐기 시각이 필요하다")
	}
	if relation.State != RelationStateDiscarded && relation.DeletedAt != nil {
		return fieldErrorf("state", "폐기되지 않은 관계에는 폐기 시각을 둘 수 없다")
	}

	if from.Layer != LayerEvent || to.Layer != LayerEvent || from.Event == nil || to.Event == nil {
		return fieldErrorf("from_context_id", "관계 양 끝은 사건 컨텍스트여야 한다")
	}
	// 폐기된 사건을 끝으로 갖는 관계는 남지 않아야 한다. `FR-AGENT_CONTEXT-076`이 사건을
	// 폐기하면 그 확정 관계도 함께 폐기하기로 했으므로, 이미 폐기된 사건으로 새 관계를
	// 만들면 그 규칙이 곧바로 깨진 상태가 된다.
	if from.DeletedAt != nil {
		return fieldErrorf("from_context_id", "폐기된 사건은 관계의 끝이 될 수 없다")
	}
	if to.DeletedAt != nil {
		return fieldErrorf("to_context_id", "폐기된 사건은 관계의 끝이 될 수 없다")
	}
	if from.ID != relation.FromContextID || to.ID != relation.ToContextID || from.GraphID != relation.GraphID || to.GraphID != relation.GraphID {
		return fieldErrorf("from_context_id", "관계와 사건의 식별자 또는 그래프가 맞지 않는다")
	}
	if relation.Type == RelationTypeRelatesTo && relation.FromContextID.String() > relation.ToContextID.String() {
		return fieldErrorf("from_context_id", "relates_to는 식별자 순서로 양 끝을 정규화해야 한다")
	}
	if err := ValidateRelationTime(relation.Type, *from.Event, *to.Event); err != nil {
		return err
	}
	return nil
}

// ValidateRelationCycle은 순환을 허용하지 않는 관계가 기존 경로를 닫는지 확인한다.
// pathExists는 관계 저장소가 대상 사건에서 시작 사건으로 이미 닿을 수 있는지 알려준다.
func ValidateRelationCycle(relation Relation, pathExists func(ID, ID) bool) error {
	if pathExists == nil {
		return fmt.Errorf("관계 경로 확인 함수가 없다")
	}
	if relation.Type == RelationTypeRelatesTo {
		return nil
	}
	if pathExists(relation.ToContextID, relation.FromContextID) {
		return fieldErrorf("relation_type", "%s 관계가 사건 관계 순환을 만든다", relation.Type)
	}
	return nil
}

// ValidateRelationTime은 사건 시간 범위로 관계별 순서와 포함 제약을 확인한다. 저장소의
// 그래프 불변식 감사가 저장된 관계에 같은 판정을 다시 적용하므로 공개한다.
func ValidateRelationTime(kind RelationType, from, to EventAttributes) error {
	switch kind {
	case RelationTypePrecedes, RelationTypeCauses:
		if from.Start.After(to.Start) {
			return fieldErrorf("from_context_id", "%s 관계의 시작 사건이 대상보다 늦다", kind)
		}
	case RelationTypePartOf:
		if from.Start.Before(to.Start) {
			return fieldErrorf("to_context_id", "part_of 관계의 전체 사건 시작 시각이 부분을 포함하지 않는다")
		}
		if to.End != nil && (from.End == nil || from.End.After(*to.End)) {
			return fieldErrorf("to_context_id", "part_of 관계의 전체 사건 종료 시각이 부분을 포함하지 않는다")
		}
	}
	return nil
}
