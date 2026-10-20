package main

import (
	"context"
	"errors"
	"slices"

	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type mcpReadStorage struct{ mcpWriteStorage }

func newMCPReadHandlers(database *store.Store, plans mcp.Plans, flow mcp.FlowSearch) (map[string]mcp.ToolHandler, error) {
	if database == nil || flow == nil {
		return nil, &mcp.DependencyFailure{}
	}
	adapter := mcpReadStorage{mcpWriteStorage{database}}
	return mcp.NewReadHandlers(mcp.Dependencies{Grades: adapter, Plans: plans, Graphs: adapter, Nodes: adapter, Hops: adapter, Relations: adapter, Flow: mcpReadFlow{flow}})
}

func mcpReadError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*store.StorageError](err); ok {
		return &mcp.DependencyFailure{Cause: err}
	}
	if errors.Is(err, store.ErrInvalidCursor) {
		return model.FieldError{Field: "cursor", Message: "목록 커서를 확인해 주십시오."}
	}
	if errors.Is(err, model.ErrNotFound) {
		return err
	}
	return &mcp.DependencyFailure{Cause: err}
}

func (s mcpReadStorage) Graph(ctx context.Context, graph model.ID) (model.Graph, error) {
	value, err := s.Store.Graph(ctx, graph)
	return value, mcpReadError(err)
}

func (s mcpReadStorage) Context(ctx context.Context, graph, node model.ID) (model.Context, error) {
	value, err := s.Store.Context(ctx, graph, node)
	return value, mcpReadError(err)
}

func (s mcpReadStorage) ListGraphs(ctx context.Context, account model.ID, filter model.GraphListFilter, cursor string, size int) ([]model.GraphListItem, string, error) {
	values, next, err := s.Store.ListGraphs(ctx, account, filter, cursor, size)
	return values, next, mcpReadError(err)
}

func (s mcpReadStorage) ListContextRelations(ctx context.Context, graph, node model.ID, states []model.RelationState, types []model.RelationType, cursor string, size int) ([]model.Relation, string, error) {
	values, next, err := s.Store.ListContextRelations(ctx, graph, node, states, types, cursor, size)
	return values, next, mcpReadError(err)
}

func (s mcpReadStorage) SearchHopContextsFrom(ctx context.Context, graph model.ID, starts []model.Context, hops int, direction string, filters []string, maximum int) (model.HopResult, error) {
	filters = slices.Clone(filters)
	for i, kind := range filters {
		if kind == "member_refs" {
			filters[i] = "has_member"
		}
	}
	value, err := s.Store.SearchHopContextsFrom(ctx, graph, starts, hops, direction, filters, maximum)
	return value, mcpReadError(err)
}

type mcpReadFlow struct{ mcp.FlowSearch }

func (f mcpReadFlow) Flow(ctx context.Context, input model.FlowInput) (model.ContextFlow, error) {
	value, err := f.FlowSearch.Flow(ctx, input)
	if err != nil {
		return model.ContextFlow{}, &mcp.DependencyFailure{Cause: err}
	}
	return value, nil
}
