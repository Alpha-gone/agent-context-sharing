package store

import (
	"context"
	"errors"
	"fmt"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// ApplyConversationSegment는 확보한 작업의 사건과 구간을 같은 트랜잭션에 반영한다.
func (s *Store) ApplyConversationSegment(ctx context.Context, task ConversationSegmentTask, event model.Context, first, last int64, final bool, limits WriteLimits) (result model.Context, applied bool, err error) {
	if !task.ID.IsV7() || !task.GraphID.IsV7() || !task.AccountID.IsV7() || !task.AgentID.IsV7() || task.Generation.IsZero() || task.LeasedUntil.IsZero() || first < 0 || last < first || event.GraphID != task.GraphID || event.CreatedBy != task.AccountID || event.CreatedByAgent != task.AgentID || event.Layer != model.LayerEvent || event.Event == nil || event.Event.ConversationID != task.ConversationID || event.Event.PreviousEventID != nil {
		return model.Context{}, false, fmt.Errorf("구간 반영 입력이 올바르지 않다")
	}
	event.Event = new(*event.Event)
	release, err := s.reserveContextWriteConnections(ctx)
	if err != nil {
		return model.Context{}, false, err
	}
	defer release()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Context{}, false, storageError("segment_apply_begin", err)
	}
	defer finishWrite(ctx, tx, "segment_apply", &err)
	outcomes := &afterWriteCommit{}
	outcome := "rolled_back"
	defer func() {
		for _, report := range outcomes.outcomes {
			report(outcome)
		}
	}()
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
		return model.Context{}, false, storageError("segment_apply_task", err)
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
	var overlap bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id=$2 AND first_turn_index <= $4 AND last_turn_index >= $3)`, task.GraphID.String(), task.ConversationID, first, last).Scan(&overlap)
	if err != nil {
		return model.Context{}, false, storageError("segment_overlap", err)
	}
	if overlap {
		return model.Context{}, false, model.ErrInvalidState
	}
	for _, memberID := range event.Event.MemberIDs {
		member, readErr := s.context(ctx, tx, task.GraphID, memberID)
		if readErr != nil {
			return model.Context{}, false, readErr
		}
		if member.Source == nil || member.Source.ConversationID != task.ConversationID || member.Source.TurnIndex == nil || *member.Source.TurnIndex < first || *member.Source.TurnIndex > last || member.DeletedAt != nil {
			return model.Context{}, false, model.ErrInvalidState
		}
		var bound bool
		if readErr = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.agent_registration WHERE agent_id=$1 AND account_id=$2 AND ingest_credential_id IS NOT NULL)`, member.CreatedByAgent.String(), member.CreatedBy.String()).Scan(&bound); readErr != nil {
			return model.Context{}, false, storageError("segment_member_binding", readErr)
		}
		if !bound {
			return model.Context{}, false, model.ErrInvalidState
		}
	}
	var previousRaw string
	err = tx.QueryRow(ctx, `SELECT event_context_id::text FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id=$2 AND last_turn_index<$3 ORDER BY last_turn_index DESC LIMIT 1`, task.GraphID.String(), task.ConversationID, first).Scan(&previousRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return model.Context{}, false, storageError("segment_previous", err)
	}
	if err == nil {
		previousID, parseErr := model.ParseID(previousRaw)
		if parseErr != nil {
			return model.Context{}, false, storageError("segment_previous_id", parseErr)
		}
		previous, readErr := s.context(ctx, tx, task.GraphID, previousID)
		if readErr != nil {
			return model.Context{}, false, readErr
		}
		if previous.DeletedAt == nil {
			event.Event.PreviousEventID = new(previousID)
		}
	}
	err = nil
	nested := context.WithValue(ctx, writeTransactionContextKey{}, tx)
	nested = context.WithValue(nested, afterWriteCommitContextKey{}, outcomes)
	op := &OperationRecord{Kind: OperationAdd, GraphID: task.GraphID, ContextID: event.ID, AccountID: task.AccountID, AgentID: task.AgentID, JudgmentSource: model.ConfirmationSourceIngest, JudgmentInput: "conversation segmentation"}
	result, err = s.CreateContextWithOperation(nested, task.GraphID, event, nil, op, limits)
	if err != nil {
		return model.Context{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.conversation_segment (event_context_id,graph_id,conversation_id,first_turn_index,last_turn_index,created_at) VALUES ($1,$2,$3,$4,$5,statement_timestamp())`, event.ID.String(), task.GraphID.String(), task.ConversationID, first, last)
	if err != nil {
		return model.Context{}, false, storageError("segment_insert", err)
	}
	if final {
		_, err = tx.Exec(ctx, `DELETE FROM public.conversation_segment_task WHERE task_id=$1 AND enqueued_at=$2 AND next_attempt_at=$3`, task.ID.String(), task.Generation, task.LeasedUntil)
		if err != nil {
			return model.Context{}, false, storageError("segment_task_delete", err)
		}
	}
	outcome = "unknown"
	if err = tx.Commit(ctx); err != nil {
		if errors.Is(err, pgx.ErrTxCommitRollback) {
			outcome = "rolled_back"
		}
		return model.Context{}, false, storageError("segment_apply_commit", err)
	}
	outcome = "committed"
	release()
	for _, callback := range outcomes.callbacks {
		callback()
	}
	return result, true, nil
}

func isAgentOperation(operation *OperationRecord) bool {
	return operation != nil && (operation.JudgmentSource == "" || operation.JudgmentSource == model.ConfirmationSourceAgent)
}

// freezeConversationSegment는 에이전트 변경과 같은 트랜잭션에서 구간을 고정한다.
func freezeConversationSegment(ctx context.Context, tx pgx.Tx, graph, event model.ID) error {
	_, err := tx.Exec(ctx, `UPDATE public.conversation_segment SET frozen_at=COALESCE(frozen_at,statement_timestamp()) WHERE graph_id=$1 AND event_context_id=$2`, graph.String(), event.String())
	return storageError("segment_freeze", err)
}
