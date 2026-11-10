package relinfer

import (
	"context"
	"fmt"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
)

// CandidateSource는 동일 스냅숏에서 사건 후보와 판정 상태를 읽는 경계다.
type CandidateSource interface {
	InferenceCandidates(context.Context, model.InferenceCandidateRequest) (model.InferenceSnapshot, error)
}

// InputBuilder는 모델 호출 없이 후보 스냅숏과 전송 입력을 준비한다.
type InputBuilder struct {
	Source  CandidateSource
	Profile Profile
}

// Prepare는 색인 대기 중이면 대기 시각만 반환하고 입력을 만들지 않는다.
func (b InputBuilder) Prepare(ctx context.Context, task model.RelationInferenceTask) (PreparedInput, *time.Time, error) {
	if b.Source == nil {
		return PreparedInput{}, nil, fmt.Errorf("추론 입력 생성 구성이 올바르지 않다")
	}
	fingerprint, err := b.Profile.Fingerprint()
	if err != nil {
		return PreparedInput{}, nil, err
	}
	snapshot, err := b.Source.InferenceCandidates(ctx, model.InferenceCandidateRequest{Task: task, ProfileFingerprint: fingerprint, EmbeddingModel: b.Profile.EmbeddingModel, Channels: slices.Clone(b.Profile.Channels), Limit: b.Profile.CandidateLimit, IndexWait: b.Profile.IndexWait, AdjacencyWindow: b.Profile.AdjacencyWindow})
	if err != nil {
		return PreparedInput{}, nil, err
	}
	if snapshot.IndexWaitUntil != nil {
		return PreparedInput{}, snapshot.IndexWaitUntil, nil
	}
	if snapshot.Target.GraphID != task.GraphID || snapshot.Target.ID != task.ContextID || snapshot.Target.Version != task.TargetVersion {
		return PreparedInput{}, nil, fmt.Errorf("추론 대상이 확보한 작업과 다르다")
	}
	input, err := BuildInput(b.Profile, snapshot.Target, snapshot.Channels)
	if err != nil {
		return PreparedInput{}, nil, err
	}
	input.SemanticUnavailable = snapshot.SemanticUnavailable
	return input, nil, nil
}
