package mcp

import (
	"context"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

// Principal은 HTTP 인증 경계가 확인한 계정·OAuth client다. 도구 인자/clientInfo에서 만들지 않는다.
type Principal struct {
	AccountID model.ID
	ClientID  string
}

type agentFailure struct{ cause error }

func (e *agentFailure) Error() string { return "행위자 등록 처리 실패" }
func (e *agentFailure) Unwrap() error { return e.cause }

func agentError(err error) error {
	if err == nil {
		return nil
	}
	// 저장 값 해석 오류의 cause에도 FieldError가 있을 수 있다. 저장소가 직접
	// 반환한 도메인 거부만 보존하고 감싼 내부 장애는 원문 없는 실패로 구분한다.
	switch err.(type) {
	case model.FieldError, plan.LimitError:
		return err
	}
	return &agentFailure{cause: err}
}

// RegisterAgent는 인증 경계의 결합만 전달하며 호스트 메타데이터·등급을 받지 않는다.
func RegisterAgent(ctx context.Context, registry AgentRegistry, principal Principal, key string) (model.ID, error) {
	if err := ctx.Err(); err != nil {
		return model.ID{}, err
	}
	if !principal.AccountID.IsV7() || principal.ClientID == "" {
		return model.ID{}, model.FieldError{Field: "registration_key", Message: "등록 키를 확인해 주십시오."}
	}
	if err := model.ValidateRegistrationKey(key); err != nil {
		return model.ID{}, err
	}
	if registry == nil {
		return model.ID{}, &agentFailure{}
	}
	id, err := registry.RegisterAgent(ctx, principal.AccountID, principal.ClientID, key)
	if err := ctx.Err(); err != nil {
		return model.ID{}, err
	}
	if err != nil {
		return model.ID{}, agentError(err)
	}
	if !id.IsV7() {
		return model.ID{}, &agentFailure{}
	}
	return id, nil
}

// BindActor는 여섯 노드·관계 쓰기가 재생·실행 전에 공유하는 현재 행위자 검사다.
// 감사 판단 입력은 검증된 도구 인자를 처리하는 업무 어댑터가 채운다.
func BindActor(ctx context.Context, registry AgentRegistry, principal Principal, agent model.ID) (Actor, error) {
	if err := ctx.Err(); err != nil {
		return Actor{}, err
	}
	if !principal.AccountID.IsV7() || principal.ClientID == "" || !agent.IsV7() {
		return Actor{}, model.FieldError{Field: "agent_id", Message: "등록된 행위자를 확인해 주십시오."}
	}
	if registry == nil {
		return Actor{}, &agentFailure{}
	}
	err := registry.ValidateAgent(ctx, principal.AccountID, principal.ClientID, agent)
	if err := ctx.Err(); err != nil {
		return Actor{}, err
	}
	if err != nil {
		return Actor{}, agentError(err)
	}
	return Actor{AccountID: principal.AccountID, AgentID: agent}, nil
}
