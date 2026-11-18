package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// UpdateConversationSegment는 늦은 턴을 기존 구간과 사건에 원자적으로 반영한다.
func (s *Store) UpdateConversationSegment(ctx context.Context, task ConversationSegmentTask, event model.Context, expectedVersion, last int64, final bool, limits WriteLimits) (result model.Context, applied bool, err error) {
	if !task.ID.IsV7() || !task.GraphID.IsV7() || !task.AccountID.IsV7() || !task.AgentID.IsV7() || task.Generation.IsZero() || task.LeasedUntil.IsZero() || !event.ID.IsV7() || event.GraphID != task.GraphID || event.Event == nil || event.Event.ConversationID != task.ConversationID || expectedVersion < 1 || last < 0 {
		return model.Context{}, false, fmt.Errorf("구간 갱신 입력이 올바르지 않다")
	}
	release, err := s.reserveContextWriteConnections(ctx)
	if err != nil {
		return model.Context{}, false, err
	}
	defer release()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Context{}, false, storageError("segment_update_begin", err)
	}
	defer finishWrite(ctx, tx, "segment_update", &err)
	if err = lockReferenceChanges(ctx, tx, task.GraphID); err != nil {
		return model.Context{}, false, err
	}
	if _, err = lockGraphs(ctx, tx, []model.ID{task.GraphID}); err != nil {
		return model.Context{}, false, err
	}
	var found int
	err = tx.QueryRow(ctx, `SELECT 1 FROM public.conversation_segment_task WHERE task_id=$1 AND graph_id=$2 AND conversation_id=$3 AND trigger_account_id=$4 AND trigger_agent_id=$5 AND enqueued_at=$6 AND next_attempt_at=$7 AND state='pending' FOR UPDATE`, task.ID.String(), task.GraphID.String(), task.ConversationID, task.AccountID.String(), task.AgentID.String(), task.Generation, task.LeasedUntil).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
		return model.Context{}, false, nil
	}
	if err != nil {
		return model.Context{}, false, storageError("segment_update_task", err)
	}
	grade, ok, err := effectiveGrade(ctx, tx, task.GraphID, task.AccountID)
	if err != nil {
		return model.Context{}, false, err
	}
	if !ok || (grade != model.GraphGradeOwner && grade != model.GraphGradeEditor) {
		return model.Context{}, false, ErrSegmentPermissionLost
	}
	if err = requireActiveGraph(ctx, tx, task.GraphID); err != nil {
		return model.Context{}, false, err
	}
	var first, oldLast int64
	var frozen bool
	err = tx.QueryRow(ctx, `SELECT first_turn_index,last_turn_index,frozen_at IS NOT NULL FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id=$2 AND event_context_id=$3 FOR UPDATE`, task.GraphID.String(), task.ConversationID, event.ID.String()).Scan(&first, &oldLast, &frozen)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Context{}, false, model.ErrNotFound
	}
	if err != nil {
		return model.Context{}, false, storageError("segment_update_lookup", err)
	}
	if frozen {
		return model.Context{}, false, ErrSegmentFrozen
	}
	if last < oldLast {
		return model.Context{}, false, model.ErrInvalidState
	}
	var overlap bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id=$2 AND event_context_id<>$3 AND first_turn_index<=$5 AND last_turn_index>=$4)`, task.GraphID.String(), task.ConversationID, event.ID.String(), first, last).Scan(&overlap)
	if err != nil {
		return model.Context{}, false, storageError("segment_update_overlap", err)
	}
	if overlap {
		return model.Context{}, false, model.ErrInvalidState
	}
	previous, err := s.context(ctx, tx, task.GraphID, event.ID)
	if err != nil {
		return model.Context{}, false, err
	}
	if previous.Event == nil || previous.DeletedAt != nil || previous.Version != expectedVersion || len(event.Event.MemberIDs) > 1000 {
		return model.Context{}, false, model.ErrInvalidState
	}
	for _, id := range previous.Event.MemberIDs {
		if !slices.Contains(event.Event.MemberIDs, id) {
			return model.Context{}, false, model.ErrInvalidState
		}
	}
	for _, id := range event.Event.MemberIDs {
		if slices.Contains(previous.Event.MemberIDs, id) {
			continue
		}
		member, e := s.context(ctx, tx, task.GraphID, id)
		if e != nil {
			return model.Context{}, false, e
		}
		if member.Source == nil || member.Source.ConversationID != task.ConversationID || member.Source.TurnIndex == nil || *member.Source.TurnIndex < first || *member.Source.TurnIndex > last || member.DeletedAt != nil {
			return model.Context{}, false, model.ErrInvalidState
		}
		var bound bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.agent_registration WHERE agent_id=$1 AND account_id=$2 AND ingest_credential_id IS NOT NULL)`, member.CreatedByAgent.String(), member.CreatedBy.String()).Scan(&bound); e != nil {
			return model.Context{}, false, storageError("segment_update_member", e)
		}
		if !bound {
			return model.Context{}, false, model.ErrInvalidState
		}
	}
	outcomes := &afterWriteCommit{}
	outcome := "rolled_back"
	defer func() {
		for _, report := range outcomes.outcomes {
			report(outcome)
		}
	}()
	nested := context.WithValue(ctx, writeTransactionContextKey{}, tx)
	nested = context.WithValue(nested, afterWriteCommitContextKey{}, outcomes)
	op := &OperationRecord{Kind: OperationUpdate, GraphID: task.GraphID, ContextID: event.ID, AccountID: task.AccountID, AgentID: task.AgentID, JudgmentSource: model.ConfirmationSourceIngest, JudgmentInput: "conversation segmentation"}
	result, err = s.UpdateContextWithOperation(nested, task.GraphID, expectedVersion, event, op, limits)
	if err != nil {
		return model.Context{}, false, err
	}
	_, err = tx.Exec(ctx, `UPDATE public.conversation_segment SET last_turn_index=$2 WHERE event_context_id=$1`, event.ID.String(), last)
	if err != nil {
		return model.Context{}, false, storageError("segment_update_range", err)
	}
	if final {
		_, err = tx.Exec(ctx, `DELETE FROM public.conversation_segment_task WHERE task_id=$1 AND enqueued_at=$2 AND next_attempt_at=$3`, task.ID.String(), task.Generation, task.LeasedUntil)
		if err != nil {
			return model.Context{}, false, storageError("segment_update_delete", err)
		}
	}
	outcome = "unknown"
	if err = tx.Commit(ctx); err != nil {
		if errors.Is(err, pgx.ErrTxCommitRollback) {
			outcome = "rolled_back"
		}
		return model.Context{}, false, storageError("segment_update_commit", err)
	}
	outcome = "committed"
	release()
	for _, callback := range outcomes.callbacks {
		callback()
	}
	return result, true, nil
}
