package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DependencyFailure는 조립 어댑터가 구분한 내부 장애다. 원인은 진단용으로만 보존한다.
type DependencyFailure struct{ Cause error }

func (e *DependencyFailure) Error() string { return "쓰기 의존 처리 실패" }
func (e *DependencyFailure) Unwrap() error { return e.Cause }

// RestoreChannelError는 MCP 폐기 이외의 삭제 복구를 웹으로 안내한다.
type RestoreChannelError struct{ Cause error }

func (e RestoreChannelError) Error() string { return "웹에서 복구해 주십시오." }
func (e RestoreChannelError) Unwrap() error { return e.Cause }

// NewWriteHandlers는 여덟 실제 쓰기만 반환한다. 전체 HTTP 조립은 읽기 도구도 요구한다.
func NewWriteHandlers(d Dependencies) (map[string]ToolHandler, error) {
	if d.Grades == nil || d.Plans == nil || d.Graphs == nil || d.Nodes == nil || d.GraphWrites == nil || d.NodeWrites == nil || d.RelationWrites == nil || d.Rejections == nil {
		return nil, fmt.Errorf("쓰기 도구의 조회·권한·플랜·쓰기·거부 기록이 필요하다")
	}
	runner, err := NewWriteRunner(d.Writes, d.Agents)
	if err != nil {
		return nil, err
	}
	runner.diagnostics = d.Diagnostics
	handlers := map[string]ToolHandler{}
	for _, tool := range []string{"graph_create", "graph_update", "node_create", "node_update", "node_discard", "node_restore", "relation_confirm", "relation_discard"} {
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
			args, err := runner.catalog.validate(tool, request.Params.Arguments)
			observeTool(d.Diagnostics, ctx, "argument_validation", tool, err)
			if err != nil {
				return writeErrorResult(err)
			}
			runner.normalizeIDs(tool, args)
			graphID := argumentID(args, "graph_id")
			var limits plan.Limits
			check := func(ctx context.Context) error {
				if tool != "graph_create" {
					if err := perm.Require(ctx, d.Grades, graphID, principal.AccountID, model.GraphGradeEditor); err != nil {
						observeTool(d.Diagnostics, ctx, "permission", tool, err)
						return err
					}
					graph, err := d.Graphs.Graph(ctx, graphID)
					if err != nil {
						return err
					}
					if graph.ID != graphID {
						return &DependencyFailure{}
					}
					if graph.DeletedAt != nil {
						return model.ErrNotFound
					}
				}
				observeTool(d.Diagnostics, ctx, "permission", tool, nil)
				limits = d.Plans.For(principal.AccountID)
				return ctx.Err()
			}
			result, err := runner.Execute(ctx, principal, tool, request.Params.Arguments, check, func(ctx context.Context) ([]byte, error) {
				return applyWrite(ctx, d, principal, tool, args, limits)
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return writeErrorResult(err)
			}
			return writeResult(result)
		}
	}
	return handlers, nil
}

func applyWrite(ctx context.Context, d Dependencies, principal Principal, tool string, args map[string]any, limits plan.Limits) ([]byte, error) {
	graphID, contextID := argumentID(args, "graph_id"), argumentID(args, "context_id")
	actor := Actor{AccountID: principal.AccountID, AgentID: argumentID(args, "agent_id"), JudgmentInput: argumentString(args, "judgment_input")}
	if actor.JudgmentInput == "" && actor.AgentID.IsV7() {
		// request_id는 업무 판단의 일부가 아니며 외피·자격은 닫힌 스키마가 거부한다.
		judgment := make(map[string]any, len(args))
		for name, value := range args {
			if name != "request_id" {
				judgment[name] = value
			}
		}
		data, err := json.Marshal(map[string]any{"tool": tool, "arguments": judgment}, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		actor.JudgmentInput = string(data)
	}
	op := model.ManagementOperation{GraphID: graphID, AccountID: actor.AccountID, AgentID: actor.AgentID, JudgmentInput: actor.JudgmentInput}
	var value any
	var err error
	switch tool {
	case "graph_create":
		var id model.ID
		id, err = model.NewID()
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		graph := model.Graph{ID: id, Name: argumentString(args, "name"), Description: argumentString(args, "description"), CreatedBy: principal.AccountID, CreatedAt: now, LastActivityAt: now, Version: 1}
		graph, err = d.GraphWrites.CreateGraph(ctx, graph, limits)
		if err == nil {
			if graph.ID != id || graph.CreatedBy != principal.AccountID {
				return nil, &DependencyFailure{}
			}
			value = map[string]any{"graph": graphObject(graph)}
		}
	case "graph_update":
		var graph model.Graph
		graph, err = d.Graphs.Graph(ctx, graphID)
		if err == nil {
			if graph.ID != graphID {
				return nil, &DependencyFailure{}
			}
			if v, ok := args["name"]; ok {
				graph.Name = v.(string)
			}
			if v, ok := args["description"]; ok {
				graph.Description = v.(string)
			}
			graph, err = d.GraphWrites.UpdateGraph(ctx, graph, args["expected_version"].(int64), principal.AccountID, limits)
			if err == nil {
				if graph.ID != graphID {
					return nil, &DependencyFailure{}
				}
				value = map[string]any{"graph": graphObject(graph)}
			}
		}
	case "node_create":
		op.Kind = model.OperationAdd
		if _, ok := args["supersedes_context_id"]; ok {
			op.Kind = model.OperationSupersede
		}
		var node model.Context
		node, err = newNode(args, principal)
		if err == nil {
			node, err = d.NodeWrites.CreateNode(ctx, graphID, node, actor, limits)
		}
		if err == nil {
			if node.GraphID != graphID {
				return nil, &DependencyFailure{}
			}
			value = nodeCreationObject(node)
		}
	case "node_update", "node_discard", "node_restore":
		op.ContextID, op.TargetVersion = contextID, 1
		op.Kind = model.OperationUpdate
		if tool == "node_discard" {
			op.Kind = model.OperationDiscard
		}
		if tool == "node_update" && argumentString(args, "management_action") == "keep" {
			op.Kind = model.OperationKeep
		}
		var node model.Context
		node, err = d.Nodes.Context(ctx, graphID, contextID)
		if err == nil {
			if node.ID != contextID || node.GraphID != graphID {
				return nil, &DependencyFailure{}
			}
			op.TargetVersion = node.Version
			switch tool {
			case "node_update":
				if op.Kind != model.OperationKeep {
					node, err = patchNode(node, args)
				}
				if err == nil {
					node, err = d.NodeWrites.UpdateNode(ctx, graphID, node, args["expected_version"].(int64), op.Kind, actor, limits)
				}
			case "node_discard":
				node, err = d.NodeWrites.DiscardNode(ctx, graphID, contextID, actor, limits)
			case "node_restore":
				node, err = d.NodeWrites.RestoreNode(ctx, graphID, contextID, actor, limits)
			}
			if err == nil {
				if node.ID != contextID || node.GraphID != graphID {
					return nil, &DependencyFailure{}
				}
				value = map[string]any{"node": nodeObject(node)}
			}
		}
	case "relation_confirm":
		op.Kind = model.OperationAdd
		id, idErr := model.NewID()
		if idErr != nil {
			return nil, idErr
		}
		now := time.Now().UTC()
		relation := model.Relation{ID: id, GraphID: graphID, Type: model.RelationType(argumentString(args, "relation_type")), FromContextID: argumentID(args, "from_context_id"), ToContextID: argumentID(args, "to_context_id"), State: model.RelationStateConfirmed, ProposedBy: model.ProposalSourceAgent, ProposedAt: now, ConfirmedBy: principal.AccountID, ConfirmedByAgent: actor.AgentID, ConfirmedAt: &now}
		// relates_to의 정체성은 양 끝의 UUID 순서에 독립적이다.
		if relation.Type == model.RelationTypeRelatesTo && relation.FromContextID.Compare(relation.ToContextID) > 0 {
			relation.FromContextID, relation.ToContextID = relation.ToContextID, relation.FromContextID
		}
		relation, err = d.RelationWrites.ConfirmRelation(ctx, graphID, relation, actor, limits)
		if err == nil {
			if relation.GraphID != graphID {
				return nil, &DependencyFailure{}
			}
			value = map[string]any{"relation": relationObject(relation)}
		}
	case "relation_discard":
		op.Kind, op.RelationID = model.OperationDiscard, argumentID(args, "relation_id")
		var relation model.Relation
		relation, err = d.RelationWrites.DiscardRelation(ctx, graphID, op.RelationID, actor, limits)
		if err == nil {
			if relation.GraphID != graphID || relation.ID != op.RelationID {
				return nil, &DependencyFailure{}
			}
			value = map[string]any{"relation": relationObject(relation)}
		}
	}
	if err != nil {
		failure, deterministic := writeFailureObject(err)
		if !deterministic {
			return nil, err
		}
		if actor.AgentID.IsV7() {
			if conflict, ok := errors.AsType[model.VersionConflictError](err); ok {
				op.TargetVersion = conflict.Current
			}
			op.Result, op.RejectionReason = model.OperationRejected, failure["code"].(string)
			if err := d.Rejections.RecordRejectedOperation(ctx, op); err != nil {
				return nil, &DependencyFailure{Cause: err}
			}
		}
		value = failure
	}
	return json.Marshal(value, json.Deterministic(true))
}

func writeFailureObject(err error) (map[string]any, bool) {
	value := map[string]any{"code": "internal", "message": "요청을 처리하지 못했습니다. 잠시 후 다시 시도해 주십시오."}
	if _, ok := errors.AsType[*DependencyFailure](err); ok {
		return value, false
	}
	if _, ok := errors.AsType[*agentFailure](err); ok {
		return value, false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return value, false
	}
	if denied, ok := errors.AsType[perm.DeniedError](err); ok {
		value["code"], value["message"], value["required_grade"] = "permission_denied", "그래프 권한이 부족합니다.", denied.Required
		return value, true
	}
	if _, ok := errors.AsType[perm.NotFoundError](err); ok || errors.Is(err, model.ErrNotFound) {
		value["code"], value["message"] = "not_found", "대상을 찾을 수 없습니다."
		return value, true
	}
	if conflict, ok := errors.AsType[model.VersionConflictError](err); ok {
		value["code"], value["message"], value["current_version"] = "version_conflict", "현재 판을 다시 조회해 주십시오.", conflict.Current
		return value, true
	}
	if field, ok := errors.AsType[model.FieldError](err); ok {
		name := field.Field
		switch name {
		case "supersedes":
			name = "supersedes_context_id"
		case "locator":
			name = "source_ref.locator"
		case "source_channel":
			name = "source_ref.channel"
		}
		value["code"], value["message"], value["field"] = "invalid_argument", "도구 입력을 확인해 주십시오.", name
		return value, true
	}
	if limit, ok := errors.AsType[plan.LimitError](err); ok {
		value["code"], value["message"], value["limit"], value["current"], value["allowed"] = "limit_exceeded", "요청 한도를 초과했습니다.", limit.Name, limit.Current, limit.Allowed
		return value, true
	}
	if _, ok := errors.AsType[RestoreChannelError](err); ok {
		value["code"], value["message"], value["alternative_channel"] = "not_supported", "웹에서 복구해 주십시오.", "web"
		return value, true
	}
	if errors.Is(err, model.ErrInvalidState) {
		value["code"], value["message"], value["field"] = "invalid_argument", "현재 상태에서 허용되지 않는 연산입니다.", "state"
		return value, true
	}
	return value, false
}

func writeErrorResult(err error) (*sdk.CallToolResult, error) {
	value, _ := writeFailureObject(err)
	data, marshalErr := json.Marshal(value, json.Deterministic(true))
	if marshalErr != nil {
		return nil, marshalErr
	}
	return writeResult(data)
}

func writeResult(data []byte) (*sdk.CallToolResult, error) {
	var value map[string]jsontext.Value
	if err := json.Unmarshal(data, &value); err != nil || value == nil {
		return writeErrorResult(&DependencyFailure{Cause: err})
	}
	return &sdk.CallToolResult{IsError: value["code"] != nil, StructuredContent: jsontext.Value(data), Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}}, nil
}

func observeTool(observer Diagnostics, ctx context.Context, stage, tool string, err error) {
	level, result, reason := "INFO", "completed", "completed"
	if err != nil {
		level, result, reason = "WARN", "rejected", "invalid_argument"
		if stage == "permission" {
			reason = "permission_denied"
		}
		if stage == "actor_binding" {
			reason = "binding_violation"
		}
		_, dependencyFailure := errors.AsType[*DependencyFailure](err)
		_, actorFailure := errors.AsType[*agentFailure](err)
		if dependencyFailure || actorFailure {
			level, result, reason = "ERROR", "failed", "store_unavailable"
		}
	}
	if typed, ok := observer.(diagnostic.Observer); ok {
		diagnostic.Observe(typed, ctx, level, stage, result, reason, false, diagnostic.Fields{Management: diagnostic.ManagementFields{Operation: tool, ValidationField: diagnosticField(err)}})
	} else if observer != nil {
		observer.RecordDiagnostic(ctx, level, stage, result, reason)
	}
}

func diagnosticField(err error) string {
	if e, ok := errors.AsType[model.FieldError](err); ok {
		return diagnostic.ValidationField(e.Field)
	}
	return ""
}
