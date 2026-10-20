package mcp

import (
	"cmp"
	"slices"

	"agent_context_sharing/internal/model"
)

// readNodes는 응답 정점의 범위·중복·삭제 상태를 경계 밖으로 내보내기 전에 검사한다.
func readNodes(graph model.ID, nodes []model.Context) (map[model.ID]bool, error) {
	ids := make(map[model.ID]bool, len(nodes))
	for _, node := range nodes {
		if node.GraphID != graph || !node.ID.IsV7() || ids[node.ID] || node.DeletedAt != nil {
			return nil, &DependencyFailure{}
		}
		ids[node.ID] = true
	}
	return ids, nil
}

func edgeObjects(ids map[model.ID]bool, edges []model.HopEdge) ([]map[string]any, []map[string]any, error) {
	references, relations := []map[string]any{}, []map[string]any{}
	edges = slices.Clone(edges)
	slices.SortFunc(edges, func(a, b model.HopEdge) int {
		if order := a.FromID.Compare(b.FromID); order != 0 {
			return order
		}
		if order := a.ToID.Compare(b.ToID); order != 0 {
			return order
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	for _, edge := range edges {
		if !ids[edge.FromID] || !ids[edge.ToID] {
			return nil, nil, &DependencyFailure{}
		}
		value := map[string]any{"from_context_id": edge.FromID.String(), "to_context_id": edge.ToID.String()}
		switch edge.Kind {
		case "derived_from", "supersedes", "member_refs":
			value["reference_type"] = edge.Kind
			references = append(references, value)
		case "has_member":
			// AGE의 저장 label을 공개 참조 속성으로 사상한다.
			value["reference_type"] = "member_refs"
			references = append(references, value)
		case "precedes", "causes", "part_of", "relates_to":
			value["relation_type"] = edge.Kind
			relations = append(relations, value)
		default:
			return nil, nil, &DependencyFailure{}
		}
	}
	return references, relations, nil
}

func hopObject(graph, start model.ID, hops, maximum int, result model.HopResult) (map[string]any, error) {
	ids, err := readNodes(graph, result.Contexts)
	if err != nil {
		return nil, err
	}
	if !ids[start] || (maximum > 0 && len(ids) > maximum) {
		return nil, &DependencyFailure{}
	}
	nodes := slices.Clone(result.Contexts)
	slices.SortFunc(nodes, func(a, b model.Context) int {
		if order := cmp.Compare(result.Distances[a.ID], result.Distances[b.ID]); order != 0 {
			return order
		}
		if order := b.RecordedAt.Compare(a.RecordedAt); order != 0 {
			return order
		}
		return b.ID.Compare(a.ID)
	})
	contexts := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		distance, ok := result.Distances[node.ID]
		if !ok || distance < 0 || distance > hops || (node.ID == start) != (distance == 0) {
			return nil, &DependencyFailure{}
		}
		value := nodeObject(node)
		value["distance"] = distance
		contexts = append(contexts, value)
	}
	references, relations, err := edgeObjects(ids, result.Edges)
	if err != nil {
		return nil, err
	}
	truncated := map[string]any{"result_truncated": result.Truncated}
	if result.Truncated {
		truncated["hop_boundary"] = result.Boundary
	}
	return map[string]any{"contexts": contexts, "references": references, "relations": relations, "truncated": truncated}, nil
}

func flowObject(graph model.ID, flow model.ContextFlow) (map[string]any, error) {
	nodes := make([]model.Context, len(flow.Contexts))
	for i, item := range flow.Contexts {
		nodes[i] = item.Value
	}
	ids, err := readNodes(graph, nodes)
	if err != nil {
		return nil, err
	}
	items := slices.Clone(flow.Contexts)
	slices.SortFunc(items, func(a, b model.FlowContext) int {
		if order := cmp.Compare(a.Rank, b.Rank); order != 0 {
			return order
		}
		if order := b.Value.RecordedAt.Compare(a.Value.RecordedAt); order != 0 {
			return order
		}
		return b.Value.ID.Compare(a.Value.ID)
	})
	contexts := make([]map[string]any, 0, len(items))
	for _, item := range items {
		value := nodeObject(item.Value)
		value["entry_distance"], value["matched_channels"], value["rank"], value["origin_kinds"] = item.EntryDistance, item.MatchedChannels, item.Rank, item.OriginKinds
		if item.FoldedCount > 0 {
			value["folded_count"] = item.FoldedCount
		}
		contexts = append(contexts, value)
	}
	references, relations, err := edgeObjects(ids, flow.Edges)
	if err != nil {
		return nil, err
	}
	channels := make(map[string]any, len(flow.Channels))
	for name, channel := range flow.Channels {
		channels[name] = map[string]any{"candidates": channel.Candidates, "latency_ms": channel.Latency.Milliseconds(), "contribution": channel.Contribution, "failure": channel.Failure}
	}
	truncated := map[string]any{"result_truncated": flow.Truncation != nil || flow.HopBoundary != nil}
	if flow.Truncation != nil {
		truncated["reason"], truncated["excluded"] = flow.Truncation.Reason, flow.Truncation.Excluded
	}
	if flow.HopBoundary != nil {
		truncated["hop_boundary"] = *flow.HopBoundary
	}
	value := map[string]any{"contexts": contexts, "references": references, "relations": relations, "entry_points": idStrings(flow.EntryPoints), "budget": map[string]any{"used": flow.BudgetUsed, "limit": flow.Budget}, "channels": channels, "truncated": truncated}
	if flow.Route != "" {
		value["route"], value["route_reason"] = flow.Route, flow.RouteReason
	}
	return value, nil
}
