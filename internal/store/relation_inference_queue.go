package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// inferenceTask는 확보 당시의 세대와 리스를 함께 보존한다.
type inferenceTask = model.RelationInferenceTask

// RelationInferenceTask는 판정 작업의 확보 세대와 리스를 보존한다.
type RelationInferenceTask = model.RelationInferenceTask

// ClaimRelationInferenceTask는 대기 작업을 확보하고 리스를 커밋한다.
func (s *Store) ClaimRelationInferenceTask(ctx context.Context, lease time.Duration) (RelationInferenceTask, bool, error) {
	return s.claimInferenceTask(ctx, lease)
}

// FailRelationInferenceTask는 확보한 세대에만 실패를 반영한다.
func (s *Store) FailRelationInferenceTask(ctx context.Context, task RelationInferenceTask, reason string, retryable bool, outputFailures int) (bool, error) {
	return s.failInferenceTask(ctx, task, reason, retryable, outputFailures)
}

// DeferRelationInferenceTask는 색인 대기 중인 확보분을 시도 횟수 소모 없이 연기한다.
func (s *Store) DeferRelationInferenceTask(ctx context.Context, task RelationInferenceTask, until time.Time) (bool, error) {
	if !task.ID.IsV7() || task.Generation.IsZero() || task.LeasedUntil.IsZero() || until.IsZero() {
		return false, fmt.Errorf("추론 작업 대기 인자가 올바르지 않다")
	}
	result, err := s.pool.Exec(ctx, `UPDATE public.relation_inference_task
		SET next_attempt_at=GREATEST($4::timestamptz,statement_timestamp())
		WHERE task_id=$1 AND enqueued_at=$2 AND next_attempt_at=$3 AND state='pending'`,
		task.ID.String(), task.Generation, task.LeasedUntil, until)
	if err != nil {
		return false, fmt.Errorf("추론 작업 색인 대기: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

// registerInferenceTask는 사건 쓰기 트랜잭션과 같은 커밋 경계를 사용한다.
func (s *Store) registerInferenceTask(ctx context.Context, tx pgx.Tx, graphID, contextID, accountID model.ID, version int64) error {
	if !graphID.IsV7() || !contextID.IsV7() || !accountID.IsV7() || version < 1 {
		return fmt.Errorf("추론 작업 등록 인자가 올바르지 않다")
	}
	id, err := model.NewID()
	if err != nil {
		return fmt.Errorf("추론 작업 식별자 생성: %w", err)
	}
	correlation := ""
	if id := model.CorrelationID(ctx); !id.IsZero() {
		correlation = id.String()
	}
	var stored string
	var generation time.Time
	err = tx.QueryRow(ctx, `INSERT INTO public.relation_inference_task
		(task_id,graph_id,context_id,target_version,trigger_account_id,attempts,output_attempts,state,enqueued_at,next_attempt_at,correlation_id)
		VALUES ($1,$2,$3,$4,$5,0,0,'pending',statement_timestamp(),statement_timestamp(),$6)
		ON CONFLICT (context_id) DO UPDATE SET
		  target_version=EXCLUDED.target_version, trigger_account_id=EXCLUDED.trigger_account_id,
		  attempts=0, output_attempts=0, state='pending', last_error=NULL,
		  enqueued_at=GREATEST(EXCLUDED.enqueued_at,relation_inference_task.enqueued_at + interval '1 microsecond'),
		  next_attempt_at=statement_timestamp(), correlation_id=EXCLUDED.correlation_id
		WHERE relation_inference_task.graph_id=EXCLUDED.graph_id RETURNING task_id::text,enqueued_at`,
		id.String(), graphID.String(), contextID.String(), version, accountID.String(), correlation).Scan(&stored, &generation)
	if err != nil {
		return fmt.Errorf("추론 작업 등록: %w", err)
	}
	storedID, err := model.ParseID(stored)
	if err != nil {
		return storageError("relation_inference_task_id", err)
	}
	fields := diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: graphID.String(), ContextID: contextID.String()}, Index: diagnostic.IndexFields{TaskID: storedID.String(), Generation: uint64(generation.UnixMicro())}}
	if observed, ok := tx.(*observedWriteTx); ok {
		observed.callbacks = append(observed.callbacks, func(state string) {
			result, reason := "pending", "completed"
			if state != "committed" {
				result, reason = "skipped", "commit_unknown"
			}
			diagnostic.Observe(s.indexDiagnostics, ctx, "INFO", "relation_inference_enqueue", result, reason, true, fields)
		})
	} else {
		diagnostic.Observe(s.indexDiagnostics, ctx, "INFO", "relation_inference_enqueue", "pending", "completed", true, fields)
	}
	return nil
}

// RelationInferenceQueueCounts는 추론 대기·실패 작업 수를 제공한다.
func (s *Store) RelationInferenceQueueCounts(ctx context.Context) (int64, int64, error) {
	var pending, failed int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='pending'), count(*) FILTER (WHERE state='failed') FROM public.relation_inference_task`).Scan(&pending, &failed)
	if err != nil {
		return 0, 0, storageError("relation_inference_progress", err)
	}
	return pending, failed, nil
}

// claimInferenceTask는 작업 행만 잠그고 리스를 커밋한 뒤 연결을 돌려준다.
func (s *Store) claimInferenceTask(ctx context.Context, lease time.Duration) (task inferenceTask, found bool, err error) {
	if lease <= 0 {
		return inferenceTask{}, false, fmt.Errorf("추론 작업 리스가 올바르지 않다")
	}
	leaseMicros := lease.Microseconds()
	if lease%time.Microsecond != 0 {
		leaseMicros++
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return inferenceTask{}, false, fmt.Errorf("추론 작업 확보 시작: %w", err)
	}
	defer finishWrite(ctx, tx, "claimInferenceTask", &err)
	var rawID, rawGraph, rawContext, rawAccount string
	err = tx.QueryRow(ctx, `SELECT task_id::text,graph_id::text,context_id::text,trigger_account_id::text,
		target_version,attempts,output_attempts,correlation_id,enqueued_at
		FROM public.relation_inference_task WHERE state='pending' AND next_attempt_at <= statement_timestamp()
		ORDER BY enqueued_at,task_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&rawID, &rawGraph, &rawContext, &rawAccount,
		&task.TargetVersion, &task.Attempts, &task.OutputAttempts, &task.CorrelationID, &task.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.Commit(ctx)
		return inferenceTask{}, false, err
	}
	if err != nil {
		return inferenceTask{}, false, fmt.Errorf("추론 작업 확보: %w", err)
	}
	for _, field := range []struct {
		raw string
		id  *model.ID
	}{{rawID, &task.ID}, {rawGraph, &task.GraphID}, {rawContext, &task.ContextID}, {rawAccount, &task.TriggerAccountID}} {
		*field.id, err = model.ParseID(field.raw)
		if err != nil {
			return inferenceTask{}, false, storageError("relation_inference_task_id", err)
		}
	}
	if err = tx.QueryRow(ctx, `UPDATE public.relation_inference_task SET next_attempt_at=statement_timestamp()+$2::bigint * interval '1 microsecond'
		WHERE task_id=$1 RETURNING next_attempt_at`, rawID, leaseMicros).Scan(&task.LeasedUntil); err != nil {
		return inferenceTask{}, false, fmt.Errorf("추론 작업 리스 설정: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return inferenceTask{}, false, fmt.Errorf("추론 작업 확보 커밋: %w", err)
	}
	return task, true, nil
}

// failInferenceTask는 동일 세대·리스에만 실패를 기록한다. 출력 위반은 별도 예산이다.
func (s *Store) failInferenceTask(ctx context.Context, task inferenceTask, reason string, retryable bool, outputFailures int) (bool, error) {
	switch reason {
	case "provider_unavailable", "provider_rejected", "output_invalid", "candidate_failed", "result_failed":
	default:
		return false, fmt.Errorf("허용되지 않은 추론 실패 사유다")
	}
	if outputFailures < 0 || outputFailures > 2 || (reason == "output_invalid" && outputFailures != 2) {
		return false, fmt.Errorf("추론 출력 위반 횟수가 올바르지 않다")
	}
	attempts, outputAttempts := task.Attempts, task.OutputAttempts
	outputAttempts += outputFailures
	state, delay := "pending", time.Duration(0)
	if reason == "output_invalid" {
		state = "failed"
	} else {
		attempts++
		if !retryable || attempts >= maxIndexAttempts {
			state = "failed"
		} else {
			delay = indexRetryDelay(attempts)
		}
	}
	result, err := s.pool.Exec(ctx, `UPDATE public.relation_inference_task SET attempts=$2,output_attempts=$3,state=$4,last_error=$5,
		next_attempt_at=statement_timestamp()+$6::bigint * interval '1 microsecond'
		WHERE task_id=$1 AND enqueued_at=$7 AND next_attempt_at=$8 AND state='pending'`,
		task.ID.String(), attempts, outputAttempts, state, reason, delay.Microseconds(), task.Generation, task.LeasedUntil)
	if err != nil {
		return false, fmt.Errorf("추론 작업 실패 기록: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

// takeInferenceTask는 결과 반영 트랜잭션의 그래프 잠금 뒤 동일 확보분만 소비한다.
// 호출자는 판정 기록과 관계 반영을 같은 트랜잭션에서 수행하고 커밋해야 한다.
func takeInferenceTask(ctx context.Context, tx pgx.Tx, task inferenceTask) (bool, error) {
	if !task.ID.IsV7() || !task.GraphID.IsV7() || !task.ContextID.IsV7() || task.TargetVersion < 1 || task.Generation.IsZero() || task.LeasedUntil.IsZero() {
		return false, fmt.Errorf("추론 작업 확보분이 올바르지 않다")
	}
	if _, err := lockGraphs(ctx, tx, []model.ID{task.GraphID}); err != nil {
		return false, err
	}
	result, err := tx.Exec(ctx, `DELETE FROM public.relation_inference_task
		WHERE task_id=$1 AND graph_id=$2 AND context_id=$3 AND target_version=$4
		AND enqueued_at=$5 AND next_attempt_at=$6 AND trigger_account_id=$7 AND state='pending'`,
		task.ID.String(), task.GraphID.String(), task.ContextID.String(), task.TargetVersion, task.Generation, task.LeasedUntil, task.TriggerAccountID.String())
	if err != nil {
		return false, fmt.Errorf("추론 결과 작업 세대 확인: %w", err)
	}
	return result.RowsAffected() == 1, nil
}
