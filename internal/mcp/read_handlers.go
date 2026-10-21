package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewReadHandlers는 현재 계정의 등급·플랜을 적용하는 다섯 읽기 도구를 반환한다.
func NewReadHandlers(d Dependencies) (map[string]ToolHandler, error) {
	if d.Grades == nil || d.Plans == nil || d.Graphs == nil || d.Nodes == nil || d.Hops == nil || d.Relations == nil || d.Flow == nil {
		return nil, fmt.Errorf("읽기 도구의 권한·플랜·그래프·노드·홉·관계·검색이 필요하다")
	}
	catalog, err := newToolCatalog()
	if err != nil {
		return nil, err
	}
	handlers := make(map[string]ToolHandler, 5)
	for _, tool := range []string{"graph_list", "graph_get", "node_get", "relation_list", "context_flow_get"} {
		handlers[tool] = func(ctx context.Context, principal Principal, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !principal.AccountID.IsV7() || principal.ClientID == "" {
				return writeErrorResult(&DependencyFailure{})
			}
			if request == nil || request.Params == nil || request.Params.Name != tool {
				return writeErrorResult(inputFieldError("name"))
			}
			args, err := catalog.validate(tool, request.Params.Arguments)
			if err != nil {
				return writeErrorResult(err)
			}
			value, err := readTool(ctx, d, principal, tool, args)
			// 의존 처리가 취소를 놓쳐도 완료 결과를 반환하지 않는다.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				return writeErrorResult(err)
			}
			data, err := json.Marshal(value, json.Deterministic(true))
			if err != nil {
				return writeErrorResult(&DependencyFailure{Cause: err})
			}
			return writeResult(data)
		}
	}
	return handlers, nil
}

func optionalInteger(args map[string]any, name string) *int {
	if value, ok := args[name].(int64); ok {
		return new(int(value))
	}
	return nil
}

func argumentStrings(args map[string]any, name string) []string {
	values, _ := args[name].([]any)
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.(string)
	}
	return result
}

func readTool(ctx context.Context, d Dependencies, principal Principal, tool string, args map[string]any) (map[string]any, error) {
	limits := d.Plans.For(principal.AccountID)
	if tool == "graph_list" {
		return readGraphList(ctx, d, principal, args, limits)
	}
	graphID := argumentID(args, "graph_id")
	if err := perm.Require(ctx, d.Grades, graphID, principal.AccountID, model.GraphGradeViewer); err != nil {
		if d.Diagnostics != nil {
			d.Diagnostics.RecordDiagnostic(ctx, "WARN", "permission", "rejected", "permission_denied")
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	graph, err := d.Graphs.Graph(ctx, graphID)
	if err != nil {
		return nil, err
	}
	if graph.ID != graphID {
		return nil, &DependencyFailure{}
	}
	if graph.DeletedAt != nil {
		return nil, model.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch tool {
	case "graph_get":
		return map[string]any{"graph": graphObject(graph)}, nil
	case "context_flow_get":
		budget, err := limits.ResolveBudget(optionalInteger(args, "budget"))
		if err != nil {
			return nil, err
		}
		asOf := time.Now().UTC()
		if specified := argumentTime(args, "as_of"); specified != nil {
			asOf = *specified
		}
		input := model.FlowInput{GraphID: graphID, WorkContext: argumentString(args, "work_context"), AsOf: asOf, Scope: argumentString(args, "scope"), Budget: budget, MaxHops: limits.MaxHops, MaxHopNodes: limits.MaxHopNodes}
		if err := input.Validate(); err != nil {
			return nil, err
		}
		flow, err := d.Flow.Flow(ctx, input)
		if err != nil {
			return nil, err
		}
		return flowObject(graphID, flow)
	}
	contextID := argumentID(args, "context_id")
	node, err := d.Nodes.Context(ctx, graphID, contextID)
	if err != nil {
		return nil, err
	}
	if node.ID != contextID || node.GraphID != graphID {
		return nil, &DependencyFailure{}
	}
	if node.DeletedAt != nil {
		return nil, model.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tool == "relation_list" {
		if node.Layer != model.LayerEvent {
			return nil, inputFieldError("context_id")
		}
		return readRelationList(ctx, d, graphID, contextID, args, limits)
	}
	hops := 0
	if specified := optionalInteger(args, "hops"); specified != nil {
		hops = *specified
	}
	maximum, err := limits.CheckHop(hops)
	if err != nil {
		return nil, err
	}
	direction := argumentString(args, "direction")
	if direction == "" {
		direction = "both"
	}
	result, err := d.Hops.SearchHopContextsFrom(ctx, graphID, []model.Context{node}, hops, direction, argumentStrings(args, "traversal_filter"), maximum)
	if err != nil {
		return nil, err
	}
	return hopObject(graphID, contextID, hops, maximum, result)
}

func readGraphList(ctx context.Context, d Dependencies, principal Principal, args map[string]any, limits plan.Limits) (map[string]any, error) {
	size, err := limits.GraphPage.Resolve("graph_page", optionalInteger(args, "page_size"))
	if err != nil {
		return nil, err
	}
	filter := model.GraphListFilter{Name: argumentString(args, "name_filter")}
	for _, grade := range argumentStrings(args, "grade_filter") {
		filter.Grades = append(filter.Grades, model.GraphGrade(grade))
	}
	graphs, cursor, err := d.Graphs.ListGraphs(ctx, principal.AccountID, filter, argumentString(args, "cursor"), size)
	if err != nil {
		return nil, err
	}
	if len(graphs) > size {
		return nil, &DependencyFailure{}
	}
	items := make([]map[string]any, 0, len(graphs))
	for _, graph := range graphs {
		if !graph.ID.IsV7() || !graph.Grade.Valid() {
			return nil, &DependencyFailure{}
		}
		items = append(items, map[string]any{"graph_id": graph.ID.String(), "name": graph.Name, "description": graph.Description, "last_activity_at": graph.LastActivityAt, "grade": graph.Grade})
	}
	return map[string]any{"graphs": items, "next_cursor": cursor}, nil
}

func readRelationList(ctx context.Context, d Dependencies, graphID, contextID model.ID, args map[string]any, limits plan.Limits) (map[string]any, error) {
	size, err := limits.RelationPage.Resolve("relation_page", optionalInteger(args, "page_size"))
	if err != nil {
		return nil, err
	}
	var states []model.RelationState
	for _, state := range argumentStrings(args, "state_filter") {
		states = append(states, model.RelationState(state))
	}
	var types []model.RelationType
	for _, kind := range argumentStrings(args, "type_filter") {
		types = append(types, model.RelationType(kind))
	}
	values, cursor, err := d.Relations.ListContextRelations(ctx, graphID, contextID, states, types, argumentString(args, "cursor"), size)
	if err != nil {
		return nil, err
	}
	if len(values) > size {
		return nil, &DependencyFailure{}
	}
	items := make([]map[string]any, 0, len(values))
	for _, relation := range values {
		if relation.GraphID != graphID || (relation.FromContextID != contextID && relation.ToContextID != contextID) {
			return nil, &DependencyFailure{}
		}
		items = append(items, relationObject(relation))
	}
	return map[string]any{"relations": items, "next_cursor": cursor}, nil
}
