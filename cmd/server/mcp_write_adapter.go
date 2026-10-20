package main

import (
	"context"
	"errors"

	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

// mcpWriteStorage는 조립 시 저장 전용 인자와 내부 장애 표식을 맞춘다.
// 업무 판단·입력 병합·MCP 코드 사상은 internal/mcp가 소유한다.
type mcpWriteStorage struct{ *store.Store }

func newMCPWriteHandlers(database *store.Store, plans mcp.Plans) (map[string]mcp.ToolHandler, error) {
	if database == nil {
		return nil, &mcp.DependencyFailure{}
	}
	adapter := mcpWriteStorage{database}
	return mcp.NewWriteHandlers(mcp.Dependencies{Grades: adapter, Plans: plans, Graphs: adapter, Nodes: adapter, Agents: adapter, GraphWrites: adapter, NodeWrites: adapter, RelationWrites: adapter, Writes: adapter, Rejections: adapter})
}

func mcpWriteError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*store.StorageError](err); ok {
		return &mcp.DependencyFailure{Cause: err}
	}
	if errors.Is(err, store.ErrRestoreChannel) {
		return mcp.RestoreChannelError{Cause: err}
	}
	return err
}

func (s mcpWriteStorage) EffectiveGrade(ctx context.Context, graph, account model.ID) (model.GraphGrade, bool, error) {
	grade, found, err := s.Store.EffectiveGrade(ctx, graph, account)
	// 조회 장애는 필드 원인이 있더라도 업무 입력 거부가 아니다.
	if err != nil {
		return "", false, &mcp.DependencyFailure{Cause: err}
	}
	return grade, found, nil
}

func (s mcpWriteStorage) Graph(ctx context.Context, id model.ID) (model.Graph, error) {
	value, err := s.Store.Graph(ctx, id)
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) Context(ctx context.Context, graph, id model.ID) (model.Context, error) {
	value, err := s.Store.Context(ctx, graph, id)
	return value, mcpWriteError(err)
}

func mcpWriteLimits(account model.ID, limits plan.Limits) store.WriteLimits {
	return store.WriteLimits{ActorID: account, GraphsPerAccount: int64(limits.GraphsPerAccount), StoredCharsPerGraph: limits.StoredCharactersPerGraph, WritesPerMinute: int64(limits.WritesPerMinute)}
}
func mcpOperation(graph, node model.ID, kind model.OperationKind, actor mcp.Actor) *store.OperationRecord {
	return &store.OperationRecord{Kind: kind, GraphID: graph, ContextID: node, AccountID: actor.AccountID, AgentID: actor.AgentID, JudgmentInput: actor.JudgmentInput}
}
func (s mcpWriteStorage) CreateGraph(ctx context.Context, graph model.Graph, limits plan.Limits) (model.Graph, error) {
	value, err := s.Store.CreateGraphWithOwner(ctx, graph, mcpWriteLimits(graph.CreatedBy, limits))
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) UpdateGraph(ctx context.Context, graph model.Graph, expected int64, account model.ID, limits plan.Limits) (model.Graph, error) {
	value, err := s.Store.UpdateGraph(ctx, graph.ID, expected, graph.Name, graph.Description)
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) CreateNode(ctx context.Context, graph model.ID, node model.Context, actor mcp.Actor, limits plan.Limits) (model.Context, error) {
	op := mcpOperation(graph, node.ID, model.OperationAdd, actor)
	var refs []model.ID
	if node.Derived != nil {
		refs = node.Derived.DerivedFrom
	}
	var value model.Context
	var err error
	if node.Derived != nil && node.Derived.Supersedes != nil {
		op.Kind = model.OperationSupersede
		value, err = s.Store.CreateSupersedingContextWithOperation(ctx, graph, node, refs, *node.Derived.Supersedes, op, mcpWriteLimits(actor.AccountID, limits))
	} else {
		value, err = s.Store.CreateContextWithOperation(ctx, graph, node, refs, op, mcpWriteLimits(actor.AccountID, limits))
	}
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) UpdateNode(ctx context.Context, graph model.ID, node model.Context, expected int64, kind model.OperationKind, actor mcp.Actor, limits plan.Limits) (model.Context, error) {
	op := mcpOperation(graph, node.ID, kind, actor)
	var value model.Context
	var err error
	if kind == model.OperationKeep {
		value, err = s.Store.KeepContext(ctx, graph, node.ID, expected, op)
	} else {
		value, err = s.Store.UpdateContextWithOperation(ctx, graph, expected, node, op, mcpWriteLimits(actor.AccountID, limits))
	}
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) DiscardNode(ctx context.Context, graph, node model.ID, actor mcp.Actor, limits plan.Limits) (model.Context, error) {
	value, err := s.Store.DiscardContext(ctx, graph, node, mcpOperation(graph, node, model.OperationDiscard, actor), mcpWriteLimits(actor.AccountID, limits))
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) RestoreNode(ctx context.Context, graph, node model.ID, actor mcp.Actor, limits plan.Limits) (model.Context, error) {
	value, err := s.Store.RestoreContext(ctx, graph, node, mcpOperation(graph, node, model.OperationUpdate, actor), mcpWriteLimits(actor.AccountID, limits))
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) ConfirmRelation(ctx context.Context, graph model.ID, relation model.Relation, actor mcp.Actor, limits plan.Limits) (model.Relation, error) {
	op := mcpOperation(graph, model.ID{}, model.OperationAdd, actor)
	op.RelationID = relation.ID
	value, err := s.Store.ConfirmRelation(ctx, graph, relation, op)
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) DiscardRelation(ctx context.Context, graph, relation model.ID, actor mcp.Actor, limits plan.Limits) (model.Relation, error) {
	op := mcpOperation(graph, model.ID{}, model.OperationDiscard, actor)
	op.RelationID = relation
	value, err := s.Store.DiscardRelation(ctx, graph, relation, op)
	return value, mcpWriteError(err)
}
func (s mcpWriteStorage) RecordRejectedOperation(ctx context.Context, op model.ManagementOperation) error {
	return mcpWriteError(s.Store.RecordRejectedOperation(ctx, store.OperationRecord{Kind: op.Kind, GraphID: op.GraphID, ContextID: op.ContextID, RelationID: op.RelationID, TargetVersion: op.TargetVersion, JudgmentInput: op.JudgmentInput, AccountID: op.AccountID, AgentID: op.AgentID}, op.RejectionReason))
}
func (s mcpWriteStorage) ExecuteWrite(ctx context.Context, account model.ID, key, tool string, normalized []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error) {
	value, err := s.Store.ExecuteWrite(ctx, account, key, tool, normalized, check, apply)
	return value, mcpWriteError(err)
}
