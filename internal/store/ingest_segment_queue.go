package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// ErrSegmentPermissionLost는 위임 계정의 유효 편집 권한이 사라진 상태다.
var ErrSegmentPermissionLost = errors.New("구간화 위임 권한 상실")

// ErrSegmentFrozen은 에이전트가 변경해 서버 갱신을 중단한 구간이다.
var ErrSegmentFrozen = errors.New("구간이 고정되었다")

// ConversationSegmentTask는 확보한 대화 작업의 세대와 리스다.
type ConversationSegmentTask struct {
	ID, GraphID, AccountID, AgentID model.ID
	ConversationID, CorrelationID   string
	Attempts                        int
	Generation, LeasedUntil         time.Time
}

// ClaimConversationSegmentTask는 준비된 한 작업을 확보하고 리스를 커밋한다.
func (s *Store) ClaimConversationSegmentTask(ctx context.Context, lease time.Duration) (task ConversationSegmentTask, found bool, err error) {
	if lease < time.Microsecond || lease > 24*time.Hour {
		return task, false, fmt.Errorf("구간화 작업 리스가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return task, false, storageError("segment_claim_begin", err)
	}
	defer finishWrite(ctx, tx, "segment_claim", &err)
	var rawID, rawGraph, rawAccount, rawAgent string
	err = tx.QueryRow(ctx, `SELECT task_id::text,graph_id::text,trigger_account_id::text,trigger_agent_id::text,conversation_id,correlation_id,attempts,enqueued_at FROM public.conversation_segment_task WHERE state='pending' AND next_attempt_at<=statement_timestamp() ORDER BY enqueued_at,task_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&rawID, &rawGraph, &rawAccount, &rawAgent, &task.ConversationID, &task.CorrelationID, &task.Attempts, &task.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.Commit(ctx)
		return ConversationSegmentTask{}, false, err
	}
	if err != nil {
		return task, false, storageError("segment_claim_query", err)
	}
	for _, field := range []struct {
		raw    string
		target *model.ID
	}{{rawID, &task.ID}, {rawGraph, &task.GraphID}, {rawAccount, &task.AccountID}, {rawAgent, &task.AgentID}} {
		*field.target, err = model.ParseID(field.raw)
		if err != nil {
			return ConversationSegmentTask{}, false, storageError("segment_claim_id", err)
		}
	}
	err = tx.QueryRow(ctx, `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp()+$2::bigint * interval '1 microsecond' WHERE task_id=$1 RETURNING next_attempt_at`, rawID, lease.Microseconds()).Scan(&task.LeasedUntil)
	if err != nil {
		return ConversationSegmentTask{}, false, storageError("segment_claim_lease", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ConversationSegmentTask{}, false, storageError("segment_claim_commit", err)
	}
	return task, true, nil
}

// FailConversationSegmentTask는 확보 세대의 실패에만 재시도 또는 종료를 적용한다.
func (s *Store) FailConversationSegmentTask(ctx context.Context, task ConversationSegmentTask, reason string, retryable bool) (bool, error) {
	if !task.ID.IsV7() || task.Generation.IsZero() || task.LeasedUntil.IsZero() || task.Attempts < 0 || (reason != "permission_lost" && reason != "segment_failed") {
		return false, fmt.Errorf("구간화 실패 입력이 올바르지 않다")
	}
	attempts := task.Attempts + 1
	state := "pending"
	delay := time.Duration(0)
	if !retryable || reason == "permission_lost" || attempts >= 5 {
		state = "failed"
	} else {
		delay = indexRetryDelay(attempts)
	}
	result, err := s.pool.Exec(ctx, `UPDATE public.conversation_segment_task SET attempts=$2,state=$3,last_error=$4,next_attempt_at=statement_timestamp()+$5::bigint * interval '1 microsecond' WHERE task_id=$1 AND enqueued_at=$6 AND next_attempt_at=$7 AND state='pending'`, task.ID.String(), attempts, state, reason, delay.Microseconds(), task.Generation, task.LeasedUntil)
	if err != nil {
		return false, storageError("segment_fail", err)
	}
	return result.RowsAffected() == 1, nil
}

// DeferConversationSegmentTask는 아직 닫히지 않은 마지막 구간을 다시 예약한다.
func (s *Store) DeferConversationSegmentTask(ctx context.Context, task ConversationSegmentTask, until time.Time) (bool, error) {
	if !task.ID.IsV7() || task.Generation.IsZero() || task.LeasedUntil.IsZero() || until.IsZero() {
		return false, fmt.Errorf("구간화 대기 입력이 올바르지 않다")
	}
	result, err := s.pool.Exec(ctx, `UPDATE public.conversation_segment_task SET next_attempt_at=GREATEST($4::timestamptz,statement_timestamp()+interval '1 second') WHERE task_id=$1 AND enqueued_at=$2 AND next_attempt_at=$3 AND state='pending'`, task.ID.String(), task.Generation, task.LeasedUntil, until)
	if err != nil {
		return false, storageError("segment_defer", err)
	}
	return result.RowsAffected() == 1, nil
}

// CompleteConversationSegmentTask는 변경할 구간이 없는 확보분을 권한 확인 뒤 완료한다.
func (s *Store) CompleteConversationSegmentTask(ctx context.Context, task ConversationSegmentTask) (done bool, err error) {
	if !task.ID.IsV7() || !task.GraphID.IsV7() || !task.AccountID.IsV7() || !task.AgentID.IsV7() || task.Generation.IsZero() || task.LeasedUntil.IsZero() {
		return false, fmt.Errorf("구간화 완료 입력이 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, storageError("segment_complete_begin", err)
	}
	defer finishWrite(ctx, tx, "segment_complete", &err)
	if _, err = lockGraphs(ctx, tx, []model.ID{task.GraphID}); err != nil {
		return false, err
	}
	var found int
	err = tx.QueryRow(ctx, `SELECT 1 FROM public.conversation_segment_task WHERE task_id=$1 AND graph_id=$2 AND conversation_id=$3 AND trigger_account_id=$4 AND trigger_agent_id=$5 AND enqueued_at=$6 AND next_attempt_at=$7 AND state='pending' FOR UPDATE`, task.ID.String(), task.GraphID.String(), task.ConversationID, task.AccountID.String(), task.AgentID.String(), task.Generation, task.LeasedUntil).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
		return false, nil
	}
	if err != nil {
		return false, storageError("segment_complete_lookup", err)
	}
	grade, ok, err := effectiveGrade(ctx, tx, task.GraphID, task.AccountID)
	if err != nil {
		return false, err
	}
	if !ok || (grade != model.GraphGradeOwner && grade != model.GraphGradeEditor) {
		return false, ErrSegmentPermissionLost
	}
	if err = requireActiveGraph(ctx, tx, task.GraphID); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `DELETE FROM public.conversation_segment_task WHERE task_id=$1`, task.ID.String())
	if err != nil {
		return false, storageError("segment_complete_delete", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, storageError("segment_complete_commit", err)
	}
	return true, nil
}
