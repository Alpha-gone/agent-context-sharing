package relinfer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
	json "encoding/json/v2"
)

// ResultSink는 결과 반영과 기록의 원자적 저장 경계다.
type ResultSink interface {
	ApplyInferenceResult(context.Context, model.InferenceApplication) (bool, error)
}

// DecisionProcessor는 후보 준비·공통 판정·검증·원자적 반영을 연결한다.
type DecisionProcessor struct {
	Builder               InputBuilder
	Judge                 Judge
	Sink                  ResultSink
	Mode, AcceptedProfile string
	AutoConfirmTypes      []model.RelationType
}

// Prepare는 모델 전송 데이터와 서버 전용 대응을 작업자 내부 버퍼에 함께 보관한다.
func (p *DecisionProcessor) Prepare(ctx context.Context, task model.RelationInferenceTask) ([]byte, *Failure, error) {
	if p.Judge == nil || p.Sink == nil {
		return nil, nil, fmt.Errorf("판정 처리 의존성이 없다")
	}
	if validator, ok := p.Judge.(interface{ ValidateProfile(Profile) error }); ok {
		if err := validator.ValidateProfile(p.Builder.Profile); err != nil {
			return nil, nil, err
		}
	}
	prepared, wait, err := p.Builder.Prepare(ctx, task)
	if err != nil {
		return nil, nil, err
	}
	if wait != nil {
		return nil, &Failure{WaitUntil: wait}, nil
	}
	raw, err := json.Marshal(prepared)
	if err != nil {
		return nil, nil, fmt.Errorf("판정 입력 준비 실패")
	}
	return raw, nil, nil
}

// Process는 준비된 발췌만 제공자에 보내고 서버 내부 식별자로 결과를 반영한다.
func (p *DecisionProcessor) Process(ctx context.Context, task model.RelationInferenceTask, input []byte, attempt int) (*Failure, error) {
	var prepared PreparedInput
	if err := json.Unmarshal(input, &prepared, json.RejectUnknownMembers(true)); err != nil || prepared.TargetID != task.ContextID || prepared.TargetVersion != task.TargetVersion {
		return &Failure{Reason: "candidate_failed", Retryable: true}, nil
	}
	fingerprint, err := p.Builder.Profile.Fingerprint()
	if err != nil || fingerprint != prepared.ProfileFingerprint {
		return &Failure{Reason: "candidate_failed", Retryable: true}, nil
	}
	started := time.Now()
	result := JudgeResult{}
	if len(prepared.Candidates) > 0 {
		var err error
		result, err = p.Judge.Judge(ctx, prepared.Data)
		if err != nil {
			if errors.Is(err, ErrInvalidOutput) {
				return &Failure{OutputInvalid: true}, nil
			}
			if provider, ok := errors.AsType[ProviderError](err); ok {
				reason := "provider_rejected"
				if provider.Status == 408 || provider.Status == 429 || provider.Status >= 500 {
					reason = "provider_unavailable"
				}
				return &Failure{Reason: reason, ProviderStatus: provider.Status, RetryAfter: provider.RetryAfter}, nil
			}
			return nil, err
		}
	} else {
		var sent inputPayload
		if err := json.Unmarshal(prepared.Data, &sent); err != nil {
			return &Failure{Reason: "candidate_failed", Retryable: true}, nil
		}
		result.Output, _ = json.Marshal(struct {
			SchemaVersion string     `json:"schema_version"`
			Decisions     []Judgment `json:"decisions"`
		}{sent.SchemaVersion, []Judgment{}})
	}
	checked, err := ValidateOutput(prepared, result.Output)
	if err != nil {
		return &Failure{OutputInvalid: true}, nil
	}
	decisions := make([]model.InferenceDecision, 0, len(checked))
	invalidUnbound := 0
	for _, item := range checked {
		if !item.CandidateID.IsV7() || !slices.Contains([]model.RelationType{model.RelationTypeRelatesTo, model.RelationTypePartOf, model.RelationTypeCauses}, item.Judgment.RelationType) {
			invalidUnbound++
			continue
		}
		decisions = append(decisions, model.InferenceDecision{CandidateID: item.CandidateID, CandidateVersion: item.CandidateVersion, RelationType: item.Judgment.RelationType, Decision: item.Judgment.Decision, Direction: item.Judgment.Direction, TargetEvidence: item.Judgment.Evidence.Target, CandidateEvidence: item.Judgment.Evidence.Candidate, Reason: item.Judgment.Reason, InvalidReason: item.InvalidReason})
	}
	versions, err := json.Marshal(struct {
		Provider, Model, PromptVersion, SchemaVersion, EmbeddingModel string
		Channels                                                      []string
		CandidateLimit, TargetChars, CandidateChars, InputChars       int
		IndexWaitNS, AdjacencyWindowNS                                int64
		Parameters                                                    map[string]string
		SemanticUnavailable                                           bool `json:"semantic_unavailable"`
	}{p.Builder.Profile.Provider, p.Builder.Profile.Model, p.Builder.Profile.PromptVersion, p.Builder.Profile.SchemaVersion, p.Builder.Profile.EmbeddingModel,
		p.Builder.Profile.Channels, p.Builder.Profile.CandidateLimit, p.Builder.Profile.TargetChars, p.Builder.Profile.CandidateChars, p.Builder.Profile.InputChars,
		int64(p.Builder.Profile.IndexWait), int64(p.Builder.Profile.AdjacencyWindow), p.Builder.Profile.Parameters, prepared.SemanticUnavailable})
	if err != nil {
		return nil, err
	}
	candidates, err := json.Marshal(prepared.Candidates)
	if err != nil {
		return nil, err
	}
	runID, err := model.NewID()
	if err != nil {
		return nil, err
	}
	latency := time.Since(started).Milliseconds()
	_, err = p.Sink.ApplyInferenceResult(ctx, model.InferenceApplication{Task: task, RunID: runID, ProfileFingerprint: prepared.ProfileFingerprint, ProfileVersions: string(versions), Candidates: string(candidates), Mode: p.Mode, AcceptedProfile: p.AcceptedProfile, AutoConfirmTypes: p.AutoConfirmTypes, Decisions: decisions, InvalidUnbound: invalidUnbound, InputTokens: result.InputTokens, OutputTokens: result.OutputTokens, LatencyMillis: &latency, ProviderStatus: result.ProviderStatus, Attempts: attempt})
	if err != nil {
		return nil, err
	}
	return nil, nil
}

// Probe는 사건 본문이 없는 고정 입력으로 제공자 판정 경로를 확인한다.
func (p *DecisionProcessor) Probe(ctx context.Context) (time.Duration, error) {
	_, err := p.Judge.Judge(ctx, []byte(`{"schema_version":"probe","data":{"target":{"alias":"T","body":"probe"},"candidates":[]}}`))
	if provider, ok := errors.AsType[ProviderError](err); ok {
		return provider.RetryAfter, err
	}
	return 0, err
}
