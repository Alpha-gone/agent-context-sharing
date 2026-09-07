package store

import (
	json "encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
)

// agVertex는 AGE가 정점 agtype에 넣는 외곽 구조다.
type agVertex struct {
	// Properties 필드는 AGE 정점의 사용자 정의 property 묶음이다.
	Properties agContextProperties `json:"properties"`
}

// agEdge는 AGE가 간선 agtype에 넣는 외곽 구조다.
type agEdge struct {
	// Label 필드는 AGE가 저장한 간선 label이다.
	Label string `json:"label"`
	// Properties 필드는 AGE 간선의 사용자 정의 property 묶음이다.
	Properties agRelationProperties `json:"properties"`
}

// agContextProperties는 Context label property를 모델과 분리해 표현한다.
type agContextProperties struct {
	// ContextID 필드는 도메인 컨텍스트 UUIDv7 문자열이다.
	ContextID string `json:"context_id"`
	// GraphID 필드는 논리 격리 단위 UUIDv7 문자열이다.
	GraphID string `json:"graph_id"`
	// Layer 필드는 source, derived 또는 event 계층 값이다.
	Layer string `json:"layer"`
	// Body 필드는 컨텍스트 본문이다.
	Body string `json:"body"`
	// RecordedAt 필드는 기록한 UTC 시각 문자열이다.
	RecordedAt string `json:"recorded_at"`
	// CreatedBy 필드는 기여 계정 UUIDv7 문자열이다.
	CreatedBy string `json:"created_by"`
	// CreatedByAgent 필드는 실제 기록 에이전트 UUIDv7 문자열이다.
	CreatedByAgent string `json:"created_by_agent"`
	// Version 필드는 낙관적 잠금에 쓰는 판 번호다.
	Version int64 `json:"version"`
	// DeletedAt 필드는 소프트 삭제 UTC 시각이며 활성 정점에서는 비어 있다.
	DeletedAt *string `json:"deleted_at"`
	// SourceRefChannel 필드는 원천 수집 경로다.
	SourceRefChannel string `json:"source_ref_channel"`
	// SourceRefLocator 필드는 원천 URI다.
	SourceRefLocator string `json:"source_ref_locator"`
	// OccurredAt 필드는 원천이 실제 발생한 UTC 시각이다.
	OccurredAt string `json:"occurred_at"`
	// OriginKind 필드는 원천 내용을 만든 주체 구분이다.
	OriginKind string `json:"origin_kind"`
	// DerivationKind 필드는 파생의 생성 목적이다.
	DerivationKind string `json:"derivation_kind"`
	// SummaryScope 필드는 요약 파생의 적용 범위다.
	SummaryScope string `json:"summary_scope"`
	// EvidenceState 필드는 파생의 근거 성격이다.
	EvidenceState string `json:"evidence_state"`
	// ConfidenceState 필드는 의견 파생의 신뢰 상태다.
	ConfidenceState string `json:"confidence_state"`
	// ValidFrom 필드는 파생의 유효 시작 UTC 시각이다.
	ValidFrom *string `json:"valid_from"`
	// ValidTo 필드는 파생의 유효 종료 UTC 시각이다.
	ValidTo *string `json:"valid_to"`
	// EvidenceInvalidated 필드는 근거 무효 표시다.
	EvidenceInvalidated bool `json:"evidence_invalidated"`
	// EventStart 필드는 사건의 시작 UTC 시각이다.
	EventStart string `json:"event_start"`
	// EventEnd 필드는 진행 중인 사건에서는 비어 있는 종료 UTC 시각이다.
	EventEnd *string `json:"event_end"`
}

// agRelationProperties는 사건 관계 간선의 12개 property를 표현한다.
type agRelationProperties struct {
	// RelationID 필드는 관계 UUIDv7 문자열이다.
	RelationID string `json:"relation_id"`
	// GraphID 필드는 관계의 논리 격리 단위 UUIDv7 문자열이다.
	GraphID string `json:"graph_id"`
	// RelationType 필드는 edge label과 일치해야 하는 관계 유형이다.
	RelationType string `json:"relation_type"`
	// FromContextID 필드는 관계 시작 사건 UUIDv7 문자열이다.
	FromContextID string `json:"from_context_id"`
	// ToContextID 필드는 관계 대상 사건 UUIDv7 문자열이다.
	ToContextID string `json:"to_context_id"`
	// State 필드는 제안, 확정 또는 폐기 상태다.
	State string `json:"state"`
	// ProposedBy 필드는 관계 후보를 만든 주체 구분이다.
	ProposedBy string `json:"proposed_by"`
	// ProposedAt 필드는 관계 후보 기록 UTC 시각이다.
	ProposedAt string `json:"proposed_at"`
	// ConfirmedBy 필드는 관계를 확정한 계정 UUIDv7 문자열이다.
	ConfirmedBy string `json:"confirmed_by"`
	// ConfirmedByAgent 필드는 관계를 확정한 에이전트 UUIDv7 문자열이다.
	ConfirmedByAgent string `json:"confirmed_by_agent"`
	// ConfirmedAt 필드는 확정 상태에서만 채우는 UTC 시각이다.
	ConfirmedAt *string `json:"confirmed_at"`
	// DeletedAt 필드는 폐기 상태에서만 채우는 UTC 시각이다.
	DeletedAt *string `json:"deleted_at"`
}

// parseContext는 AGE 정점 값을 요청한 그래프의 model.Context로 조립한다.
func parseContext(raw string, graphID model.ID) (model.Context, error) {
	var vertex agVertex
	if err := json.Unmarshal(trimAGType(raw, "vertex"), &vertex); err != nil {
		return model.Context{}, fmt.Errorf("AGE 정점 agtype 해석: %w", err)
	}
	properties := vertex.Properties
	contextID, err := model.ParseID(properties.ContextID)
	if err != nil {
		return model.Context{}, fmt.Errorf("context_id: %w", err)
	}
	storedGraphID, err := model.ParseID(properties.GraphID)
	if err != nil {
		return model.Context{}, fmt.Errorf("graph_id: %w", err)
	}
	if storedGraphID != graphID {
		return model.Context{}, fmt.Errorf("그래프 격리 위반: 요청 그래프와 정점 graph_id가 다르다")
	}
	createdBy, err := model.ParseID(properties.CreatedBy)
	if err != nil {
		return model.Context{}, fmt.Errorf("created_by: %w", err)
	}
	createdByAgent, err := model.ParseID(properties.CreatedByAgent)
	if err != nil {
		return model.Context{}, fmt.Errorf("created_by_agent: %w", err)
	}
	recordedAt, err := parseUTC(properties.RecordedAt)
	if err != nil {
		return model.Context{}, fmt.Errorf("recorded_at: %w", err)
	}
	context := model.Context{
		ID:             contextID,
		GraphID:        storedGraphID,
		Layer:          model.Layer(properties.Layer),
		Body:           properties.Body,
		RecordedAt:     recordedAt,
		CreatedBy:      createdBy,
		CreatedByAgent: createdByAgent,
		Version:        properties.Version,
	}
	if context.DeletedAt, err = parseOptionalUTC(properties.DeletedAt); err != nil {
		return model.Context{}, fmt.Errorf("deleted_at: %w", err)
	}
	switch context.Layer {
	case model.LayerSource:
		occurredAt, err := parseUTC(properties.OccurredAt)
		if err != nil {
			return model.Context{}, fmt.Errorf("occurred_at: %w", err)
		}
		context.Source = &model.SourceAttributes{
			Reference: model.SourceReference{
				Channel: model.SourceChannel(properties.SourceRefChannel),
				Locator: properties.SourceRefLocator,
			},
			OccurredAt: occurredAt,
			OriginKind: model.OriginKind(properties.OriginKind),
		}
	case model.LayerDerived:
		validFrom, err := parseOptionalUTC(properties.ValidFrom)
		if err != nil {
			return model.Context{}, fmt.Errorf("valid_from: %w", err)
		}
		validTo, err := parseOptionalUTC(properties.ValidTo)
		if err != nil {
			return model.Context{}, fmt.Errorf("valid_to: %w", err)
		}
		context.Derived = &model.DerivedAttributes{
			Kind:                model.DerivationKind(properties.DerivationKind),
			SummaryScope:        model.SummaryScope(properties.SummaryScope),
			EvidenceState:       model.EvidenceState(properties.EvidenceState),
			ConfidenceState:     model.ConfidenceState(properties.ConfidenceState),
			ValidFrom:           validFrom,
			ValidTo:             validTo,
			EvidenceInvalidated: properties.EvidenceInvalidated,
		}
	case model.LayerEvent:
		start, err := parseUTC(properties.EventStart)
		if err != nil {
			return model.Context{}, fmt.Errorf("event_start: %w", err)
		}
		end, err := parseOptionalUTC(properties.EventEnd)
		if err != nil {
			return model.Context{}, fmt.Errorf("event_end: %w", err)
		}
		context.Event = &model.EventAttributes{Start: start, End: end}
	default:
		return model.Context{}, fmt.Errorf("저장된 컨텍스트 계층 %q가 올바르지 않다", context.Layer)
	}
	// 사건의 member_refs는 property가 아니라 HAS_MEMBER 간선에 있으므로 호출자가 간선을
	// 조립한 뒤 검증한다.
	if context.Layer == model.LayerEvent {
		return context, nil
	}
	if err := context.Validate(); err != nil {
		return model.Context{}, fmt.Errorf("저장된 컨텍스트 검증: %w", err)
	}
	return context, nil
}

// parseRelation은 AGE 간선 값을 요청한 그래프의 model.Relation으로 조립한다.
func parseRelation(raw string, graphID model.ID) (model.Relation, error) {
	var edge agEdge
	if err := json.Unmarshal(trimAGType(raw, "edge"), &edge); err != nil {
		return model.Relation{}, fmt.Errorf("AGE 간선 agtype 해석: %w", err)
	}
	properties := edge.Properties
	storedGraphID, err := model.ParseID(properties.GraphID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("graph_id: %w", err)
	}
	if storedGraphID != graphID {
		return model.Relation{}, fmt.Errorf("그래프 격리 위반: 요청 그래프와 간선 graph_id가 다르다")
	}
	relationID, err := model.ParseID(properties.RelationID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("relation_id: %w", err)
	}
	fromContextID, err := model.ParseID(properties.FromContextID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("from_context_id: %w", err)
	}
	toContextID, err := model.ParseID(properties.ToContextID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("to_context_id: %w", err)
	}
	proposedAt, err := parseUTC(properties.ProposedAt)
	if err != nil {
		return model.Relation{}, fmt.Errorf("proposed_at: %w", err)
	}
	relation := model.Relation{
		ID:            relationID,
		GraphID:       storedGraphID,
		Type:          model.RelationType(properties.RelationType),
		FromContextID: fromContextID,
		ToContextID:   toContextID,
		State:         model.RelationState(properties.State),
		ProposedBy:    model.ProposalSource(properties.ProposedBy),
		ProposedAt:    proposedAt,
	}
	expectedLabel, err := relationLabel(relation.Type)
	if err != nil {
		return model.Relation{}, err
	}
	if edge.Label != expectedLabel {
		return model.Relation{}, fmt.Errorf("그래프 격리 위반: 관계 유형과 간선 label이 다르다")
	}
	if relation.ConfirmedAt, err = parseOptionalUTC(properties.ConfirmedAt); err != nil {
		return model.Relation{}, fmt.Errorf("confirmed_at: %w", err)
	}
	if relation.DeletedAt, err = parseOptionalUTC(properties.DeletedAt); err != nil {
		return model.Relation{}, fmt.Errorf("deleted_at: %w", err)
	}
	if properties.ConfirmedBy != "" {
		relation.ConfirmedBy, err = model.ParseID(properties.ConfirmedBy)
		if err != nil {
			return model.Relation{}, fmt.Errorf("confirmed_by: %w", err)
		}
	}
	if properties.ConfirmedByAgent != "" {
		relation.ConfirmedByAgent, err = model.ParseID(properties.ConfirmedByAgent)
		if err != nil {
			return model.Relation{}, fmt.Errorf("confirmed_by_agent: %w", err)
		}
	}
	return relation, nil
}

// contextProperties는 도메인 컨텍스트를 AGE property map으로 평탄화한다.
func contextProperties(context model.Context) map[string]any {
	properties := map[string]any{
		"context_id":       context.ID.String(),
		"graph_id":         context.GraphID.String(),
		"layer":            string(context.Layer),
		"body":             context.Body,
		"recorded_at":      context.RecordedAt.UTC().Format(time.RFC3339Nano),
		"created_by":       context.CreatedBy.String(),
		"created_by_agent": context.CreatedByAgent.String(),
		"version":          context.Version,
	}
	if context.DeletedAt != nil {
		properties["deleted_at"] = context.DeletedAt.UTC().Format(time.RFC3339Nano)
	}
	switch context.Layer {
	case model.LayerSource:
		properties["source_ref_channel"] = string(context.Source.Reference.Channel)
		properties["source_ref_locator"] = context.Source.Reference.Locator
		properties["occurred_at"] = context.Source.OccurredAt.UTC().Format(time.RFC3339Nano)
		properties["origin_kind"] = string(context.Source.OriginKind)
	case model.LayerDerived:
		properties["derivation_kind"] = string(context.Derived.Kind)
		properties["summary_scope"] = string(context.Derived.SummaryScope)
		properties["evidence_state"] = string(context.Derived.EvidenceState)
		properties["confidence_state"] = string(context.Derived.ConfidenceState)
		properties["evidence_invalidated"] = context.Derived.EvidenceInvalidated
		if context.Derived.ValidFrom != nil {
			properties["valid_from"] = context.Derived.ValidFrom.UTC().Format(time.RFC3339Nano)
		}
		if context.Derived.ValidTo != nil {
			properties["valid_to"] = context.Derived.ValidTo.UTC().Format(time.RFC3339Nano)
		}
	case model.LayerEvent:
		properties["event_start"] = context.Event.Start.UTC().Format(time.RFC3339Nano)
		if context.Event.End != nil {
			properties["event_end"] = context.Event.End.UTC().Format(time.RFC3339Nano)
		}
	}
	return properties
}

// relationProperties는 도메인 관계를 AGE 간선 property map으로 평탄화한다.
func relationProperties(relation model.Relation) map[string]any {
	properties := map[string]any{
		"relation_id":     relation.ID.String(),
		"graph_id":        relation.GraphID.String(),
		"relation_type":   string(relation.Type),
		"from_context_id": relation.FromContextID.String(),
		"to_context_id":   relation.ToContextID.String(),
		"state":           string(relation.State),
		"proposed_by":     string(relation.ProposedBy),
		"proposed_at":     relation.ProposedAt.UTC().Format(time.RFC3339Nano),
	}
	if !relation.ConfirmedBy.IsZero() {
		properties["confirmed_by"] = relation.ConfirmedBy.String()
	}
	if !relation.ConfirmedByAgent.IsZero() {
		properties["confirmed_by_agent"] = relation.ConfirmedByAgent.String()
	}
	if relation.ConfirmedAt != nil {
		properties["confirmed_at"] = relation.ConfirmedAt.UTC().Format(time.RFC3339Nano)
	}
	if relation.DeletedAt != nil {
		properties["deleted_at"] = relation.DeletedAt.UTC().Format(time.RFC3339Nano)
	}
	return properties
}

// trimAGType는 AGE가 JSON 값 뒤에 붙이는 타입 접미사를 제거한다.
func trimAGType(raw, kind string) []byte {
	trimmed := strings.TrimSpace(raw)
	trimmed, _ = strings.CutSuffix(trimmed, "::"+kind)
	return []byte(trimmed)
}

// parseUTC는 AGE property의 RFC3339 UTC 시각을 해석한다.
func parseUTC(raw string) (time.Time, error) {
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || value.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("UTC RFC3339 시각이 아니다")
	}
	return value.UTC(), nil
}

// parseOptionalUTC는 비어 있을 수 있는 AGE property 시각을 해석한다.
func parseOptionalUTC(raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	value, err := parseUTC(*raw)
	if err != nil {
		return nil, err
	}
	normalized := new(time.Time)
	*normalized = value
	return normalized, nil
}
