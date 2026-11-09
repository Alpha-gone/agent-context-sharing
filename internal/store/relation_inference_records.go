package store

import (
	"context"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 이 저장 함수는 호출자가 소유한 결과 트랜잭션에서만 사용한다. 관계 반영과 판정 기록을
// 따로 커밋하면 추론 출처가 없는 관계 또는 관계 없는 적용 기록이 남을 수 있다.
type inferenceRunRecord struct {
	ID, GraphID, ContextID                                  model.ID
	TargetVersion                                           int64
	ProfileFingerprint, ProfileVersions, Candidates, Result string
	Attempts                                                int
	InputTokens, OutputTokens, LatencyMillis                *int64
	ProviderStatus                                          *string
	CreatedAt                                               time.Time
}

type inferenceDecisionRecord struct {
	ID, RunID, CandidateID, RelationID              model.ID
	CandidateVersion                                int64
	RelationType                                    model.RelationType
	Direction, Decision, Evidence, ValidationResult string
	Reason, ValidationReason                        *string
}

func insertInferenceRun(ctx context.Context, tx pgx.Tx, run inferenceRunRecord) error {
	if !run.ID.IsV7() || !run.GraphID.IsV7() || !run.ContextID.IsV7() || run.TargetVersion < 1 ||
		run.ProfileFingerprint == "" || run.ProfileVersions == "" || run.Candidates == "" || run.Result == "" ||
		run.Attempts < 0 || run.CreatedAt.IsZero() || run.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("추론 회차 기록이 올바르지 않다")
	}
	_, err := tx.Exec(ctx, `INSERT INTO public.relation_inference_run
		(run_id,graph_id,context_id,target_version,profile_fingerprint,profile_versions,candidates,result,attempts,input_tokens,output_tokens,latency_ms,provider_status,created_at)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8,$9,$10,$11,$12,$13,$14)`,
		run.ID.String(), run.GraphID.String(), run.ContextID.String(), run.TargetVersion, run.ProfileFingerprint,
		run.ProfileVersions, run.Candidates, run.Result, run.Attempts, run.InputTokens, run.OutputTokens,
		run.LatencyMillis, run.ProviderStatus, run.CreatedAt)
	if err != nil {
		return storageError("relation_inference_run_insert", err)
	}
	return nil
}

func insertInferenceDecision(ctx context.Context, tx pgx.Tx, decision inferenceDecisionRecord) error {
	if !decision.ID.IsV7() || !decision.RunID.IsV7() || !decision.CandidateID.IsV7() ||
		(!decision.RelationID.IsZero() && !decision.RelationID.IsV7()) || decision.CandidateVersion < 1 ||
		decision.Decision == "" || decision.Evidence == "" || decision.ValidationResult == "" {
		return fmt.Errorf("추론 판정 기록이 올바르지 않다")
	}
	if _, err := relationLabel(decision.RelationType); err != nil {
		return err
	}
	var relationID any
	if !decision.RelationID.IsZero() {
		relationID = decision.RelationID.String()
	}
	_, err := tx.Exec(ctx, `INSERT INTO public.relation_inference_decision
		(decision_id,run_id,candidate_context_id,candidate_version,relation_type,direction,decision,evidence,reason,validation_result,validation_reason,relation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12)`, decision.ID.String(), decision.RunID.String(),
		decision.CandidateID.String(), decision.CandidateVersion, string(decision.RelationType), decision.Direction,
		decision.Decision, decision.Evidence, decision.Reason, decision.ValidationResult, decision.ValidationReason, relationID)
	if err != nil {
		return storageError("relation_inference_decision_insert", err)
	}
	return nil
}
