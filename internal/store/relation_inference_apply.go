package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	json "encoding/json/v2"
	"github.com/jackc/pgx/v5"
)

// ApplyInferenceResult는 확보분 검증·판정 기록·관계 반영을 한 커밋으로 묶는다.
// 모델 출력은 호출 전에 relinfer의 계약 검증을 거친 값이어야 한다.
func (s *Store) ApplyInferenceResult(ctx context.Context, request model.InferenceApplication) (stale bool, err error) {
	task := request.Task
	if !request.RunID.IsV7() || !task.TriggerAccountID.IsV7() || !validInferenceFingerprint(request.ProfileFingerprint) || request.ProfileVersions == "" || request.Candidates == "" || !slices.Contains([]string{"shadow", "propose", "apply"}, request.Mode) || request.Attempts < 0 || request.InvalidUnbound < 0 {
		return false, fmt.Errorf("추론 반영 인자가 올바르지 않다")
	}
	if request.AcceptedProfile != "" && !validInferenceFingerprint(request.AcceptedProfile) {
		return false, fmt.Errorf("승인된 추론 프로필 지문이 올바르지 않다")
	}
	for _, kind := range request.AutoConfirmTypes {
		if kind != model.RelationTypeRelatesTo && kind != model.RelationTypePartOf {
			return false, fmt.Errorf("자동 확정 유형이 올바르지 않다")
		}
	}
	tx, err := s.writeTransaction(ctx)
	if err != nil {
		return false, err
	}
	defer finishWrite(ctx, tx, "ApplyInferenceResult", &err)
	matched, err := takeInferenceTask(ctx, tx, task)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	state := "completed"
	if request.InvalidUnbound > 0 {
		state = "partial_invalid"
	}
	if !matched {
		state = "stale"
	}
	run := inferenceRunRecord{ID: request.RunID, GraphID: task.GraphID, ContextID: task.ContextID, TargetVersion: task.TargetVersion, ProfileFingerprint: request.ProfileFingerprint, ProfileVersions: request.ProfileVersions, Candidates: request.Candidates, Result: state, Attempts: request.Attempts, InputTokens: request.InputTokens, OutputTokens: request.OutputTokens, LatencyMillis: request.LatencyMillis, ProviderStatus: request.ProviderStatus, CreatedAt: now}
	if err := insertInferenceRun(ctx, tx, run); err != nil {
		return false, err
	}
	if !matched {
		for _, decision := range request.Decisions {
			if !decision.CandidateID.IsV7() || decision.CandidateVersion < 1 || !slices.Contains([]model.RelationType{model.RelationTypeRelatesTo, model.RelationTypePartOf, model.RelationTypeCauses}, decision.RelationType) {
				continue
			}
			id, err := model.NewID()
			if err != nil {
				return false, err
			}
			evidence, err := json.Marshal(map[string]string{"target": decision.TargetEvidence, "candidate": decision.CandidateEvidence})
			if err != nil {
				return false, err
			}
			choice := decision.Decision
			if choice == "" {
				choice = "invalid"
			}
			reason := "stale"
			if err := insertInferenceDecision(ctx, tx, inferenceDecisionRecord{ID: id, RunID: request.RunID, CandidateID: decision.CandidateID, CandidateVersion: decision.CandidateVersion, RelationType: decision.RelationType, Direction: decision.Direction, Decision: choice, Evidence: string(evidence), ValidationResult: "rejected", ValidationReason: &reason}); err != nil {
				return false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		s.observeInferenceResult(ctx, task, request.RunID)
		return true, nil
	}
	graphActive := true
	if err := requireActiveGraph(ctx, tx, task.GraphID); err != nil {
		if errors.Is(err, ErrNotFound) {
			graphActive = false
		} else {
			return false, err
		}
	}
	grade, found, err := effectiveGrade(ctx, tx, task.GraphID, task.TriggerAccountID)
	if err != nil {
		return false, err
	}
	delegationOK := graphActive && found && (grade == model.GraphGradeOwner || grade == model.GraphGradeEditor)
	target, targetErr := s.context(ctx, tx, task.GraphID, task.ContextID)
	if targetErr != nil && !errors.Is(targetErr, ErrNotFound) {
		return false, targetErr
	}
	targetReason := ""
	switch {
	case errors.Is(targetErr, ErrNotFound) || target.DeletedAt != nil || target.Layer != model.LayerEvent || target.Event == nil:
		targetReason = "endpoint_inactive"
	case target.Version != task.TargetVersion:
		targetReason = "target_changed"
	case !delegationOK:
		targetReason = "delegation_invalid"
	}
	for _, decision := range request.Decisions {
		if !decision.CandidateID.IsV7() || decision.CandidateVersion < 1 || !slices.Contains([]model.RelationType{model.RelationTypeRelatesTo, model.RelationTypePartOf, model.RelationTypeCauses}, decision.RelationType) {
			continue
		}
		if err := s.applyInferenceDecision(ctx, tx, request, target, targetReason, decision, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("추론 결과 커밋: %w", err)
	}
	s.observeInferenceResult(ctx, task, request.RunID)
	return false, nil
}

func (s *Store) observeInferenceResult(ctx context.Context, task model.RelationInferenceTask, runID model.ID) {
	if s.managementDiagnostics == nil {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT decision_id::text,relation_type,decision,validation_result,COALESCE(validation_reason,''),relation_id::text FROM public.relation_inference_decision WHERE run_id=$1 ORDER BY decision_id`, runID.String())
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var decisionID, kind, decision, resultCode, reasonCode string
		var relationID *string
		if err := rows.Scan(&decisionID, &kind, &decision, &resultCode, &reasonCode, &relationID); err != nil {
			return
		}
		if !slices.Contains([]string{"relation", "none", "abstain"}, decision) {
			decision = "invalid"
		}
		stage, result, reason := "relation_inference_apply", "completed", "completed"
		if resultCode != "valid" {
			stage, result, reason = "relation_inference_reject", "rejected", inferenceDiagnosticReason(reasonCode)
		} else if reasonCode == "policy_downgraded" {
			reason = "policy_downgraded"
		}
		fields := diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: task.GraphID.String(), ContextID: task.ContextID.String(), InferenceDecisionID: decisionID}, Management: diagnostic.ManagementFields{Operation: "relation_inference", RelationType: kind, InferenceDecision: decision, CommitState: "committed"}}
		if relationID != nil {
			fields.Identity.RelationID = *relationID
		}
		diagnostic.Observe(s.managementDiagnostics, ctx, "INFO", stage, result, reason, true, fields)
	}
}

func inferenceDiagnosticReason(reason string) string {
	if reason == "stale" {
		return "stale_generation"
	}
	if slices.Contains([]string{"output_invalid", "evidence_unverified", "target_changed", "candidate_changed", "endpoint_inactive", "delegation_invalid", "identity_settled", "identity_proposed", "relation_constraint", "policy_downgraded"}, reason) {
		return reason
	}
	return "constraint_violation"
}

func validInferenceFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func (s *Store) applyInferenceDecision(ctx context.Context, tx pgx.Tx, request model.InferenceApplication, target model.Context, targetReason string, value model.InferenceDecision, now time.Time) error {
	task := request.Task
	id, err := model.NewID()
	if err != nil {
		return err
	}
	reason := value.InvalidReason
	if reason != "" && reason != "output_invalid" && reason != "evidence_unverified" {
		return fmt.Errorf("추론 판정 오류 사유가 올바르지 않다")
	}
	if reason == "" && !slices.Contains([]string{"relation", "none", "abstain"}, value.Decision) {
		reason = "output_invalid"
	}
	if value.Decision == "" {
		value.Decision = "invalid"
	}
	if reason == "" {
		reason = targetReason
	}
	candidate, candidateErr := s.context(ctx, tx, task.GraphID, value.CandidateID)
	if candidateErr != nil && !errors.Is(candidateErr, ErrNotFound) {
		return candidateErr
	}
	if reason == "" {
		switch {
		case errors.Is(candidateErr, ErrNotFound) || candidate.DeletedAt != nil || candidate.Layer != model.LayerEvent || candidate.Event == nil:
			reason = "endpoint_inactive"
		case candidate.Version != value.CandidateVersion:
			reason = "candidate_changed"
		case candidate.ID == target.ID:
			reason = "output_invalid"
		}
	}
	evidence, err := json.Marshal(map[string]string{"target": value.TargetEvidence, "candidate": value.CandidateEvidence})
	if err != nil {
		return err
	}
	status := "valid"
	if reason != "" {
		status = "rejected"
	}
	record := inferenceDecisionRecord{ID: id, RunID: request.RunID, CandidateID: value.CandidateID, CandidateVersion: value.CandidateVersion, RelationType: value.RelationType, Direction: value.Direction, Decision: value.Decision, Evidence: string(evidence), ValidationResult: status}
	if value.Reason != "" {
		record.Reason = &value.Reason
	}
	if reason != "" {
		record.ValidationReason = &reason
	} else if request.Mode == "apply" && request.ProfileFingerprint != request.AcceptedProfile && value.Decision == "relation" {
		policyReason := "policy_downgraded"
		record.ValidationReason = &policyReason
	}
	if err := insertInferenceDecision(ctx, tx, record); err != nil {
		return err
	}
	if reason != "" {
		return s.recordInferenceRejection(ctx, tx, task, id, request.ProfileFingerprint, reason)
	}
	if value.Decision != "relation" || request.Mode == "shadow" {
		return nil
	}
	from, to := target, candidate
	if value.RelationType == model.RelationTypeRelatesTo {
		if value.Direction != "" {
			return s.rejectInferenceDecision(ctx, tx, task, id, request.ProfileFingerprint, "output_invalid")
		}
	} else {
		switch value.Direction {
		case "T_to_C":
		case "C_to_T":
			from, to = to, from
		default:
			return s.rejectInferenceDecision(ctx, tx, task, id, request.ProfileFingerprint, "output_invalid")
		}
	}
	if value.RelationType == model.RelationTypePartOf {
		part, _, ok := memberSubsetCandidate(task.GraphID, from, to)
		if !ok || part.FromContextID != from.ID || part.ToContextID != to.ID {
			return s.rejectInferenceDecision(ctx, tx, task, id, request.ProfileFingerprint, "relation_constraint")
		}
	}
	relationID, err := model.NewID()
	if err != nil {
		return err
	}
	relation := normalizeRelation(model.Relation{ID: relationID, GraphID: task.GraphID, Type: value.RelationType, FromContextID: from.ID, ToContextID: to.ID, State: model.RelationStateProposed, ProposedBy: model.ProposalSourceInference, InferenceDecisionID: id, ProposedAt: now})
	automatic := request.Mode == "apply" && request.ProfileFingerprint == request.AcceptedProfile && slices.Contains(request.AutoConfirmTypes, value.RelationType) && value.RelationType != model.RelationTypeCauses
	if automatic {
		relation.State = model.RelationStateConfirmed
		relation.ConfirmedBy = task.TriggerAccountID
		relation.ConfirmedSource = model.ConfirmationSourceInference
		relation.ConfirmedAt = &now
	}
	if relation.FromContextID != from.ID {
		from, to = to, from
	}
	if err := model.ValidateRelation(relation, from, to); err != nil {
		return s.rejectInferenceDecision(ctx, tx, task, id, request.ProfileFingerprint, "relation_constraint")
	}
	existing, lookupErr := s.relationByIdentity(ctx, tx, task.GraphID, relation.Type, relation.FromContextID, relation.ToContextID)
	if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
		return lookupErr
	}
	if lookupErr == nil && existing.State != model.RelationStateProposed {
		return s.rejectInferenceDecision(ctx, tx, task, id, request.ProfileFingerprint, "identity_settled")
	}
	if lookupErr == nil && !automatic {
		return s.rejectInferenceDecision(ctx, tx, task, id, request.ProfileFingerprint, "identity_proposed")
	}
	if _, err := s.validateRelationCycle(ctx, tx, task.GraphID, relation); err != nil {
		if errors.Is(err, ErrInvalidRelation) {
			return s.rejectInferenceDecision(ctx, tx, task, id, request.ProfileFingerprint, "relation_constraint")
		}
		return err
	}
	if lookupErr == nil {
		existing.State = model.RelationStateConfirmed
		existing.ConfirmedBy = task.TriggerAccountID
		existing.ConfirmedByAgent = model.ID{}
		existing.ConfirmedSource = model.ConfirmationSourceInference
		existing.ConfirmedAt = &now
		existing.InferenceDecisionID = id
		relation, err = s.updateRelation(ctx, tx, task.GraphID, existing)
	} else {
		relation, err = s.createRelation(ctx, tx, task.GraphID, relation)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE public.relation_inference_decision SET relation_id=$2 WHERE decision_id=$1`, id.String(), relation.ID.String()); err != nil {
		return storageError("inference_decision_relation", err)
	}
	if err := bumpContentRevision(ctx, tx, task.GraphID); err != nil {
		return err
	}
	op := &OperationRecord{Kind: OperationAdd, GraphID: task.GraphID, RelationID: relation.ID, JudgmentInput: id.String() + " " + request.ProfileFingerprint, AccountID: task.TriggerAccountID, JudgmentSource: model.ConfirmationSourceInference}
	if err := s.recordAppliedRelationOperation(ctx, tx, op, relation.ID); err != nil {
		return err
	}
	return s.checkWriteInvariants(ctx, tx, task.GraphID, []model.ID{relation.FromContextID, relation.ToContextID}, EmbeddingExpectation{})
}

func (s *Store) rejectInferenceDecision(ctx context.Context, tx pgx.Tx, task model.RelationInferenceTask, id model.ID, fingerprint, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE public.relation_inference_decision SET validation_result='rejected',validation_reason=$2 WHERE decision_id=$1`, id.String(), reason); err != nil {
		return storageError("inference_decision_reject", err)
	}
	return s.recordInferenceRejection(ctx, tx, task, id, fingerprint, reason)
}

func (s *Store) recordInferenceRejection(ctx context.Context, tx pgx.Tx, task model.RelationInferenceTask, id model.ID, fingerprint, reason string) error {
	op := OperationRecord{Kind: OperationAdd, GraphID: task.GraphID, JudgmentInput: id.String() + " " + fingerprint, AccountID: task.TriggerAccountID, JudgmentSource: model.ConfirmationSourceInference}
	if err := op.valid("rejected"); err != nil {
		return err
	}
	return s.insertOperation(ctx, tx, op, "rejected", reason)
}
