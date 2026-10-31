package mcp

import (
	"slices"
	"time"

	"agent_context_sharing/internal/model"
)

func argumentString(args map[string]any, name string) string {
	value, _ := args[name].(string)
	return value
}
func argumentID(args map[string]any, name string) model.ID {
	id, _ := model.ParseID(argumentString(args, name))
	return id
}
func argumentIDs(args map[string]any, name string) []model.ID {
	values, _ := args[name].([]any)
	ids := make([]model.ID, len(values))
	for i, value := range values {
		ids[i], _ = model.ParseID(value.(string))
	}
	return ids
}
func argumentTime(args map[string]any, name string) *time.Time {
	value, ok := args[name].(string)
	if !ok {
		return nil
	}
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return new(parsed.UTC())
}

func newNode(args map[string]any, principal Principal) (model.Context, error) {
	id, err := model.NewID()
	if err != nil {
		return model.Context{}, err
	}
	node := model.Context{ID: id, GraphID: argumentID(args, "graph_id"), Layer: model.Layer(argumentString(args, "layer")), Body: argumentString(args, "body"), RecordedAt: time.Now().UTC(), CreatedBy: principal.AccountID, CreatedByAgent: argumentID(args, "agent_id"), Version: 1}
	switch node.Layer {
	case model.LayerSource:
		ref := args["source_ref"].(map[string]any)
		node.Source = &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannel(ref["channel"].(string)), Locator: ref["locator"].(string)}, OccurredAt: *argumentTime(args, "occurred_at"), OriginKind: model.OriginKind(argumentString(args, "origin_kind"))}
	case model.LayerDerived:
		node.Derived = &model.DerivedAttributes{Kind: model.DerivationKind(argumentString(args, "derivation_kind")), SummaryScope: model.SummaryScope(argumentString(args, "summary_scope")), DerivedFrom: argumentIDs(args, "derived_from"), EvidenceState: model.EvidenceState(argumentString(args, "evidence_state")), ConfidenceState: model.ConfidenceState(argumentString(args, "confidence_state")), ValidFrom: argumentTime(args, "valid_from"), ValidTo: argumentTime(args, "valid_to")}
		if _, ok := args["supersedes_context_id"]; ok {
			node.Derived.Supersedes = new(argumentID(args, "supersedes_context_id"))
		}
	case model.LayerEvent:
		node.Event = &model.EventAttributes{MemberIDs: argumentIDs(args, "member_refs"), Start: *argumentTime(args, "event_start"), End: argumentTime(args, "event_end")}
	}
	return node, nil
}

func patchNode(previous model.Context, args map[string]any) (model.Context, error) {
	if previous.Layer == model.LayerSource {
		return model.Context{}, inputFieldError("layer")
	}
	next := previous
	if next.Derived != nil {
		next.Derived = new(*next.Derived)
	}
	if next.Event != nil {
		next.Event = new(*next.Event)
	}
	for _, name := range []string{"body", "confidence_state", "valid_from", "valid_to", "member_refs", "event_start", "event_end"} {
		value, ok := args[name]
		if !ok {
			continue
		}
		if slices.Contains([]string{"confidence_state", "valid_from", "valid_to"}, name) && next.Derived == nil || slices.Contains([]string{"member_refs", "event_start", "event_end"}, name) && next.Event == nil {
			return model.Context{}, inputFieldError(name)
		}
		switch name {
		case "body":
			next.Body = value.(string)
		case "confidence_state":
			next.Derived.ConfidenceState = model.ConfidenceState(value.(string))
		case "valid_from":
			next.Derived.ValidFrom = argumentTime(args, name)
		case "valid_to":
			next.Derived.ValidTo = argumentTime(args, name)
		case "member_refs":
			next.Event.MemberIDs = argumentIDs(args, name)
		case "event_start":
			next.Event.Start = *argumentTime(args, name)
		case "event_end":
			next.Event.End = argumentTime(args, name)
		}
	}
	next.Version++
	return next, nil
}

func graphObject(graph model.Graph) map[string]any {
	return map[string]any{"graph_id": graph.ID.String(), "name": graph.Name, "description": graph.Description, "created_by": graph.CreatedBy.String(), "created_at": graph.CreatedAt, "last_activity_at": graph.LastActivityAt, "stored_chars": graph.StoredChars, "version": graph.Version, "deleted_at": graph.DeletedAt}
}

func idStrings(ids []model.ID) []string {
	result := make([]string, len(ids))
	for i, id := range ids {
		result[i] = id.String()
	}
	return result
}

func nodeObject(node model.Context) map[string]any {
	value := map[string]any{"context_id": node.ID.String(), "graph_id": node.GraphID.String(), "layer": node.Layer, "body": node.Body, "recorded_at": node.RecordedAt, "created_by": node.CreatedBy.String(), "created_by_agent": node.CreatedByAgent.String(), "version": node.Version, "deleted_at": node.DeletedAt}
	if source := node.Source; source != nil {
		value["source_ref"] = map[string]any{"channel": source.Reference.Channel, "locator": source.Reference.Locator}
		value["occurred_at"], value["origin_kind"] = source.OccurredAt, source.OriginKind
	}
	if derived := node.Derived; derived != nil {
		value["derivation_kind"], value["derived_from"] = derived.Kind, idStrings(derived.DerivedFrom)
		value["evidence_state"], value["evidence_invalidated"] = derived.EvidenceState, derived.EvidenceInvalidated
		if derived.SummaryScope != "" {
			value["summary_scope"] = derived.SummaryScope
		}
		if derived.ConfidenceState != "" {
			value["confidence_state"] = derived.ConfidenceState
		}
		value["valid_from"], value["valid_to"] = derived.ValidFrom, derived.ValidTo
		if derived.Supersedes != nil {
			value["supersedes_context_id"] = derived.Supersedes.String()
		}
	}
	if event := node.Event; event != nil {
		value["member_refs"], value["event_start"], value["event_end"] = idStrings(event.MemberIDs), event.Start, event.End
	}
	return value
}

func relationObject(relation model.Relation) map[string]any {
	value := map[string]any{"relation_id": relation.ID.String(), "graph_id": relation.GraphID.String(), "relation_type": relation.Type, "from_context_id": relation.FromContextID.String(), "to_context_id": relation.ToContextID.String(), "state": relation.State, "proposed_by": relation.ProposedBy, "proposed_at": relation.ProposedAt, "confirmed_at": relation.ConfirmedAt, "deleted_at": relation.DeletedAt}
	if !relation.ConfirmedBy.IsZero() {
		value["confirmed_by"] = relation.ConfirmedBy.String()
	}
	if !relation.ConfirmedByAgent.IsZero() {
		value["confirmed_by_agent"] = relation.ConfirmedByAgent.String()
	}
	return value
}

// nodeCreationObject는 기존 폐기 원천의 반환을 새 생성·복구와 구분한다.
func nodeCreationObject(node model.Context) map[string]any {
	value := map[string]any{"node": nodeObject(node)}
	if node.Layer == model.LayerSource && node.DeletedAt != nil {
		value["creation_status"] = "existing_deleted"
		value["warning"] = "기존의 폐기·삭제된 원천을 반환했습니다. 새 생성이나 자동 복구가 아니며 활성 근거로 사용할 수 없습니다. MCP 폐기분은 node_restore로, 웹 삭제분은 웹에서 복구해 주십시오. request_id 재생은 최초 결과이므로 복구 후 활성 원천 조회는 node_get을 사용해 주십시오."
	}
	return value
}
