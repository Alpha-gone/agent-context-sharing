package host

import (
	"context"
	"encoding/json/jsontext"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
)

// Remote는 호스트가 사용하는 원격 도구 계약과 호출 경계다.
// 호스트 발견은 로컬 서버가 소유하므로 원격 인증을 시작하지 않는다.
type Remote interface {
	ListTools(context.Context) (*contract.Catalog, error)
	CallTool(context.Context, string, jsontext.Value) (*contract.Result, error)
}

// Tools는 도구 목록과 호출에 동일한 기동 정책과 행위 에이전트를 적용한다.
// stdio 중계와 실제 인증·전송 조립은 후속 단계에서 이 경계를 연결한다.
type Tools struct {
	remote  Remote
	policy  contract.Policy
	agentID uuid.UUID
}

// NewTools는 검증한 기동 정책·식별자와 원격 경계를 연결한다.
func NewTools(remote Remote, policy contract.Policy, agentID uuid.UUID) *Tools {
	return &Tools{remote: remote, policy: policy, agentID: agentID}
}

// ListTools는 검증된 전체 목록에서 정책이 허용한 호스트 스키마만 공개한다.
func (t *Tools) ListTools(ctx context.Context) ([]contract.Tool, error) {
	if t.remote == nil {
		return nil, contract.ErrProtocol
	}
	catalog, err := t.remote.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, contract.ErrProtocol
	}
	return catalog.Tools(t.policy), nil
}

// CallTool는 정책 밖 호출을 준비 요청 전에 거부하고 입력을 검증·주입한다.
func (t *Tools) CallTool(ctx context.Context, name string, arguments jsontext.Value) (*contract.Result, error) {
	if t.remote == nil || !t.policy.Allows(name) {
		return nil, contract.ErrProtocol
	}
	catalog, err := t.remote.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, contract.ErrProtocol
	}
	prepared, err := catalog.PrepareArguments(name, arguments, t.policy, t.agentID)
	if err != nil {
		return nil, err
	}
	return t.remote.CallTool(ctx, name, prepared)
}
