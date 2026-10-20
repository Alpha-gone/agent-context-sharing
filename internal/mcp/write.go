package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"agent_context_sharing/internal/model"
	"github.com/google/jsonschema-go/jsonschema"
)

// WriteRunner는 스키마·지문과 매 요청의 권한/행위자 재검사를 재생 경계에 결합한다.
type WriteRunner struct {
	executor WriteExecutor
	agents   AgentRegistry
	catalog  *toolCatalog
}

func NewWriteRunner(executor WriteExecutor, agents AgentRegistry) (*WriteRunner, error) {
	if executor == nil || agents == nil {
		return nil, fmt.Errorf("쓰기 실행기와 등록 저장소가 필요하다")
	}
	catalog, err := newToolCatalog()
	if err != nil {
		return nil, err
	}
	return &WriteRunner{executor: executor, agents: agents, catalog: catalog}, nil
}

// Execute의 check는 현재 Bearer·등급을 확인하며 apply는 도메인 완료 객체만
// 직렬화한다. 업무·거부 감사의 변환은 도구 어댑터가 담당한다.
func (runner *WriteRunner) Execute(ctx context.Context, principal Principal, tool string, arguments []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !principal.AccountID.IsV7() || principal.ClientID == "" || check == nil || apply == nil {
		return nil, fmt.Errorf("검증된 쓰기 주체와 처리기가 필요하다")
	}
	switch tool {
	case "graph_create", "graph_update", "node_create", "node_update", "node_discard", "node_restore", "relation_confirm", "relation_discard":
	default:
		return nil, inputFieldError("name")
	}
	values, err := runner.catalog.validate(tool, arguments)
	if err != nil {
		return nil, err
	}
	key, _ := values["request_id"].(string)
	delete(values, "request_id")
	runner.normalizeIDs(tool, values)
	normalized, err := json.Marshal(map[string]any{"tool": tool, "arguments": values}, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("쓰기 인자 정규화: %w", err)
	}
	var agent model.ID
	if value, ok := values["agent_id"].(string); ok {
		agent, _ = model.ParseID(value)
	}
	return runner.executor.ExecuteWrite(ctx, principal.AccountID, key, tool, normalized, func(checkCtx context.Context) error {
		if err := checkCtx.Err(); err != nil {
			return err
		}
		if err := check(checkCtx); err != nil {
			return err
		}
		if agent.IsV7() {
			_, err := BindActor(checkCtx, runner.agents, principal, agent)
			return err
		}
		return checkCtx.Err()
	}, apply)
}

func (runner *WriteRunner) normalizeIDs(tool string, values map[string]any) {
	// UUID 허용 표기만 통일한다. 본문·판단 입력·URI의 문자열은 변경하지 않는다.
	for _, definition := range runner.catalog.definitions {
		if definition.tool.Name != tool {
			continue
		}
		for name, schema := range definition.tool.InputSchema.(*jsonschema.Schema).Properties {
			value, present := values[name]
			if !present {
				continue
			}
			if schema.Format == "uuid-v7" {
				id, _ := model.ParseID(value.(string))
				values[name] = id.String()
			} else if schema.Type == "array" && schema.Items != nil && schema.Items.Format == "uuid-v7" {
				for i, item := range value.([]any) {
					id, _ := model.ParseID(item.(string))
					value.([]any)[i] = id.String()
				}
			}
		}
	}
}
