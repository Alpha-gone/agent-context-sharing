package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

type agentRegistryStub struct {
	register func(context.Context, model.ID, string, string) (model.ID, error)
	validate func(context.Context, model.ID, string, model.ID) error
}

func (s agentRegistryStub) RegisterAgent(ctx context.Context, account model.ID, client, key string) (model.ID, error) {
	return s.register(ctx, account, client, key)
}
func (s agentRegistryStub) ValidateAgent(ctx context.Context, account model.ID, client string, agent model.ID) error {
	return s.validate(ctx, account, client, agent)
}

func TestAgentRegistrationBoundary(t *testing.T) {
	for _, mode := range []string{"success", "missing account", "missing client", "short key", "nil registry", "invalid id", "invalid key", "limit", "storage failure", "cancel before", "cancel after"} {
		t.Run(mode, func(t *testing.T) {
			account, _ := model.NewID()
			agent, _ := model.NewID()
			principal := Principal{AccountID: account, ClientID: "verified-client"}
			key := "registration_key_1"
			calls := 0
			failure := errors.New("synthetic storage secret")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var registry AgentRegistry = agentRegistryStub{register: func(ctx context.Context, actual model.ID, client, key string) (model.ID, error) {
				calls++
				if actual != account || client != "verified-client" || key != "registration_key_1" {
					t.Fatal("등록이 인증 결합을 변경")
				}
				switch mode {
				case "invalid id":
					return model.ID{}, nil
				case "invalid key":
					return model.ID{}, model.FieldError{Field: "registration_key", Message: "등록 키를 확인해 주십시오."}
				case "limit":
					return model.ID{}, plan.LimitError{Name: "agent_registrations_per_account", Current: 128, Allowed: 128}
				case "storage failure":
					return model.ID{}, failure
				case "cancel after":
					cancel()
				}
				return agent, nil
			}}
			switch mode {
			case "missing account":
				principal.AccountID = model.ID{}
			case "missing client":
				principal.ClientID = ""
			case "short key":
				key = "short"
			case "nil registry":
				registry = nil
			case "cancel before":
				cancel()
			}
			id, err := RegisterAgent(ctx, registry, principal, key)
			if mode == "success" {
				if err != nil || id != agent {
					t.Fatal("정상 등록 실패")
				}
				return
			}
			if !id.IsZero() || err == nil {
				t.Fatal("거부·장애가 등록 ID 반환")
			}
			switch mode {
			case "missing account", "missing client", "short key", "invalid key":
				if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != "registration_key" {
					t.Fatal("등록 오류 사상 실패")
				}
			case "limit":
				if _, ok := errors.AsType[plan.LimitError](err); !ok {
					t.Fatal("한도 오류 손실")
				}
			case "cancel before", "cancel after":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("취소 원인 손실")
				}
			default:
				if _, ok := errors.AsType[*agentFailure](err); !ok || strings.Contains(err.Error(), "synthetic storage secret") {
					t.Fatal("내부 장애 비노출 실패")
				}
			}
			if mode == "storage failure" && !errors.Is(err, failure) {
				t.Fatal("저장 원인 손실")
			}
			if (mode == "missing account" || mode == "missing client" || mode == "short key" || mode == "nil registry" || mode == "cancel before") && calls != 0 {
				t.Fatal("입력 거부 전 DB 진입")
			}
		})
	}
}

func TestAgentBindingBoundary(t *testing.T) {
	for _, mode := range []string{"success", "missing account", "missing client", "missing agent", "unknown agent", "other account", "other client", "revoked", "nil registry", "storage failure", "cancel before", "cancel after"} {
		t.Run(mode, func(t *testing.T) {
			account, _ := model.NewID()
			agent, _ := model.NewID()
			principal := Principal{AccountID: account, ClientID: "verified-client"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			failure := errors.New("synthetic storage secret")
			var registry AgentRegistry = agentRegistryStub{validate: func(ctx context.Context, actual model.ID, client string, actualAgent model.ID) error {
				calls++
				if actual != account || client != "verified-client" || actualAgent != agent {
					t.Fatal("행위자 검증 결합 변경")
				}
				switch mode {
				case "unknown agent", "other account", "other client", "revoked":
					return model.FieldError{Field: "agent_id", Message: "등록된 행위자를 확인해 주십시오."}
				case "storage failure":
					return failure
				case "cancel after":
					cancel()
				}
				return nil
			}}
			switch mode {
			case "missing account":
				principal.AccountID = model.ID{}
			case "missing client":
				principal.ClientID = ""
			case "missing agent":
				agent = model.ID{}
			case "nil registry":
				registry = nil
			case "cancel before":
				cancel()
			}
			actor, err := BindActor(ctx, registry, principal, agent)
			if mode == "success" {
				if err != nil || actor.AccountID != account || actor.AgentID != agent || actor.JudgmentInput != "" {
					t.Fatal("행위자 바인딩 실패")
				}
				return
			}
			if err == nil || actor != (Actor{}) {
				t.Fatal("거부가 행위자 전달")
			}
			switch mode {
			case "storage failure", "nil registry":
				if _, ok := errors.AsType[*agentFailure](err); !ok || strings.Contains(err.Error(), "synthetic storage secret") {
					t.Fatal("내부 장애를 자격 오류로 처리")
				}
			case "cancel before", "cancel after":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("취소 원인 손실")
				}
			default:
				if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != "agent_id" || field.Message != "등록된 행위자를 확인해 주십시오." {
					t.Fatal("존재·결합·폐기 안내 차이")
				}
			}
			if mode == "storage failure" && !errors.Is(err, failure) {
				t.Fatal("저장 원인 손실")
			}
			if (mode == "missing account" || mode == "missing client" || mode == "missing agent" || mode == "nil registry" || mode == "cancel before") && calls != 0 {
				t.Fatal("입력 거부 전 검증 호출")
			}
		})
	}
}

func TestAgentWrappedStorageFailure(t *testing.T) {
	account, _ := model.NewID()
	agent, _ := model.NewID()
	principal := Principal{AccountID: account, ClientID: "verified-client"}
	// 손상된 저장 식별자의 해석 실패도 model.FieldError를 cause로 보존할 수 있다.
	failure := errors.Join(errors.New("synthetic storage secret"), model.FieldError{Field: "id", Message: "synthetic decoded secret"})
	registry := agentRegistryStub{
		register: func(context.Context, model.ID, string, string) (model.ID, error) { return model.ID{}, failure },
		validate: func(context.Context, model.ID, string, model.ID) error { return failure },
	}
	for _, mode := range []string{"register", "actor"} {
		t.Run(mode, func(t *testing.T) {
			var err error
			if mode == "register" {
				_, err = RegisterAgent(t.Context(), registry, principal, "registration_key_1")
			} else {
				_, err = BindActor(t.Context(), registry, principal, agent)
			}
			if _, ok := errors.AsType[*agentFailure](err); !ok || !errors.Is(err, failure) || strings.Contains(err.Error(), "secret") {
				t.Fatal("저장 해석 실패를 사용자 입력 오류로 분류하거나 원문 노출")
			}
		})
	}
}
