package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewAgentHandler는 등록을 일반 쓰기 재생과 구분하고 같은 입력·결과 외피 계약에 연결한다.
func NewAgentHandler(registry AgentRegistry) (ToolHandler, error) {
	if registry == nil {
		return nil, fmt.Errorf("행위자 등록 저장소가 필요하다")
	}
	catalog, err := newToolCatalog()
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, principal Principal, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !principal.AccountID.IsV7() || principal.ClientID == "" {
			return writeErrorResult(&DependencyFailure{})
		}
		if request == nil || request.Params == nil || request.Params.Name != "agent_register" {
			return writeErrorResult(inputFieldError("name"))
		}
		args, err := catalog.validate("agent_register", request.Params.Arguments)
		if err != nil {
			return writeErrorResult(err)
		}
		id, err := RegisterAgent(ctx, registry, principal, argumentString(args, "registration_key"))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return writeErrorResult(err)
		}
		data, err := json.Marshal(map[string]any{"agent_id": id.String()}, json.Deterministic(true))
		if err != nil {
			return writeErrorResult(&DependencyFailure{Cause: err})
		}
		return writeResult(data)
	}, nil
}
