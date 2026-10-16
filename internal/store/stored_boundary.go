package store

import (
	json "encoding/json/v2"
	"fmt"
	"strings"

	"agent_context_sharing/internal/model"
)

// parseStoredEndpoints는 간선과 실제 양 끝의 그래프·식별자·계층을 함께 검사한다.
// 과거 참조의 삭제 상태는 읽을 수 있으며 새 참조의 활성 상태는 쓰기에서 검사한다.
func parseStoredEndpoints(rawEdge, rawSource, rawTarget string, graphID model.ID) (source, target model.Context, err error) {
	defer func() {
		if err != nil {
			source, target = model.Context{}, model.Context{}
			err = storageError("edge_endpoints", err)
		}
	}()
	source, err = parseStoredIdentity(rawSource, graphID)
	if err != nil {
		return
	}
	target, err = parseStoredIdentity(rawTarget, graphID)
	if err != nil {
		return
	}
	var edge struct {
		StartID    int64  `json:"start_id"`
		EndID      int64  `json:"end_id"`
		Label      string `json:"label"`
		Properties struct {
			GraphID string `json:"graph_id"`
		} `json:"properties"`
	}
	var from, to struct {
		ID int64 `json:"id"`
	}
	if err = json.Unmarshal(trimAGType(rawEdge, "edge"), &edge); err != nil {
		return
	}
	if err = json.Unmarshal(trimAGType(rawSource, "vertex"), &from); err != nil {
		return
	}
	if err = json.Unmarshal(trimAGType(rawTarget, "vertex"), &to); err != nil {
		return
	}
	if from.ID == 0 || to.ID == 0 || edge.Properties.GraphID != graphID.String() || edge.StartID != from.ID || edge.EndID != to.ID {
		err = fmt.Errorf("%w: 간선과 실제 끝점의 그래프·식별자가 다르다", ErrInvariantViolation)
		return
	}
	valid := false
	switch edge.Label {
	case "DERIVED_FROM":
		valid = source.Layer == model.LayerDerived && target.Layer != model.LayerEvent
	case "SUPERSEDES":
		valid = source.Layer == model.LayerDerived && target.Layer == model.LayerDerived
	case "HAS_MEMBER":
		valid = source.Layer == model.LayerEvent && target.Layer != model.LayerEvent
	default:
		_, known := relationLabels[edge.Label]
		valid = known && source.Layer == model.LayerEvent && target.Layer == model.LayerEvent
	}
	if !valid {
		err = fmt.Errorf("%w: 간선 끝점 계층이 올바르지 않다", ErrInvariantViolation)
	}
	return
}

func parseStoredRelation(rawEdge, rawSource, rawTarget string, graphID model.ID) (model.Relation, error) {
	relation, err := parseRelation(rawEdge, graphID)
	if err != nil {
		return model.Relation{}, err
	}
	source, target, err := parseStoredEndpoints(rawEdge, rawSource, rawTarget, graphID)
	if err != nil {
		return model.Relation{}, err
	}
	if relation.FromContextID != source.ID || relation.ToContextID != target.ID {
		return model.Relation{}, storageError("relation_identity", ErrInvariantViolation)
	}
	return relation, nil
}

// validateStoredProperties는 계층 밖 속성이 모델 변환에서 조용히 사라지지 않게 한다.
func validateStoredProperties(raw, kind, layer string) error {
	var value struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(trimAGType(raw, kind), &value); err != nil {
		return err
	}
	if layer == "derived" && value.Properties["evidence_invalidated"] == nil {
		return fmt.Errorf("필수 저장 속성이 없다")
	}
	common := "context_id graph_id layer body recorded_at created_by created_by_agent version deleted_at"
	fields := map[string]string{
		"source":   "source_ref_channel source_ref_locator occurred_at origin_kind",
		"derived":  "derivation_kind summary_scope evidence_state confidence_state valid_from valid_to evidence_invalidated",
		"event":    "event_start event_end",
		"relation": "relation_id graph_id relation_type from_context_id to_context_id state proposed_by proposed_at confirmed_by confirmed_by_agent confirmed_at deleted_at",
	}
	allowed := fields[layer]
	if layer != "relation" {
		allowed = common + " " + allowed
	}
	keys := make(map[string]struct{})
	for key := range strings.FieldsSeq(allowed) {
		keys[key] = struct{}{}
	}
	for key := range value.Properties {
		if _, found := keys[key]; !found {
			return fmt.Errorf("허용되지 않은 저장 속성")
		}
	}
	return nil
}

// endpointProjection은 범위·계층·실제 끝점 대조에 필요한 값만 받아 본문 적재를 늘리지 않는다.
func endpointProjection(name string) string {
	return "{id: id(" + name + "), label: label(" + name + "), properties: {context_id: " + name + ".context_id, graph_id: " + name + ".graph_id, layer: " + name + ".layer}}"
}

func parseStoredIdentity(raw string, graphID model.ID) (model.Context, error) {
	var vertex agVertex
	if err := json.Unmarshal(trimAGType(raw, "vertex"), &vertex); err != nil {
		return model.Context{}, err
	}
	id, err := model.ParseID(vertex.Properties.ContextID)
	if err != nil {
		return model.Context{}, err
	}
	if vertex.Label != "Context" || vertex.Properties.GraphID != graphID.String() {
		return model.Context{}, ErrInvariantViolation
	}
	layer := model.Layer(vertex.Properties.Layer)
	if layer != model.LayerSource && layer != model.LayerDerived && layer != model.LayerEvent {
		return model.Context{}, ErrInvariantViolation
	}
	return model.Context{ID: id, GraphID: graphID, Layer: layer}, nil
}
