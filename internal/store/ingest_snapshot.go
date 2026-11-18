package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// ConversationSegmentSnapshot은 한 읽기 스냅숏의 수집 원천과 기존 구간이다.
type ConversationSegmentSnapshot struct {
	Now      time.Time
	Turns    []model.Context
	Segments []ConversationSegmentRecord
}

type ConversationSegmentRecord struct {
	Event       model.Context
	First, Last int64
	FrozenAt    *time.Time
}

// ReadConversationSegmentSnapshot은 에이전트가 직접 만든 원천을 제외하고 읽는다.
func (s *Store) ReadConversationSegmentSnapshot(ctx context.Context, task ConversationSegmentTask) (snapshot ConversationSegmentSnapshot, err error) {
	if !task.ID.IsV7() || !task.GraphID.IsV7() || task.Generation.IsZero() || task.LeasedUntil.IsZero() {
		return snapshot, fmt.Errorf("구간 읽기 작업이 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot, storageError("segment_read_begin", err)
	}
	defer func() {
		if cleanup := rollbackTransaction(ctx, tx); cleanup != nil {
			snapshot = ConversationSegmentSnapshot{}
			err = errors.Join(err, storageError("segment_read_rollback", cleanup))
		}
	}()
	var active bool
	err = tx.QueryRow(ctx, `SELECT statement_timestamp(), EXISTS (SELECT 1 FROM public.conversation_segment_task WHERE task_id=$1 AND graph_id=$2 AND conversation_id=$3 AND enqueued_at=$4 AND next_attempt_at=$5 AND state='pending')`, task.ID.String(), task.GraphID.String(), task.ConversationID, task.Generation, task.LeasedUntil).Scan(&snapshot.Now, &active)
	if err != nil {
		return ConversationSegmentSnapshot{}, storageError("segment_read_task", err)
	}
	if !active {
		return ConversationSegmentSnapshot{}, model.ErrNotFound
	}
	rows, err := tx.Query(ctx, `SELECT c.properties::text FROM `+s.contextTable()+` c JOIN public.agent_registration a ON a.agent_id::text = c.properties ->> 'created_by_agent'::text AND a.account_id::text = c.properties ->> 'created_by'::text AND a.ingest_credential_id IS NOT NULL WHERE c.properties ->> 'graph_id'::text=$1 AND c.properties ->> 'layer'::text='source' AND c.properties ->> 'conversation_id'::text=$2 ORDER BY (c.properties ->> 'turn_index'::text)::bigint`, task.GraphID.String(), task.ConversationID)
	if err != nil {
		return ConversationSegmentSnapshot{}, storageError("segment_read_turns", err)
	}
	for rows.Next() {
		var properties string
		if err = rows.Scan(&properties); err != nil {
			rows.Close()
			return ConversationSegmentSnapshot{}, storageError("segment_read_turn", err)
		}
		value, parseErr := parseContext(`{"label":"Context","properties":`+properties+`}`, task.GraphID)
		if parseErr != nil {
			rows.Close()
			return ConversationSegmentSnapshot{}, storageError("segment_read_turn_parse", parseErr)
		}
		snapshot.Turns = append(snapshot.Turns, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ConversationSegmentSnapshot{}, storageError("segment_read_turn_rows", err)
	}
	rows, err = tx.Query(ctx, `SELECT event_context_id::text,first_turn_index,last_turn_index,frozen_at FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id=$2 ORDER BY first_turn_index`, task.GraphID.String(), task.ConversationID)
	if err != nil {
		return ConversationSegmentSnapshot{}, storageError("segment_read_segments", err)
	}
	var eventIDs []model.ID
	for rows.Next() {
		var raw string
		var item ConversationSegmentRecord
		if err = rows.Scan(&raw, &item.First, &item.Last, &item.FrozenAt); err != nil {
			rows.Close()
			return ConversationSegmentSnapshot{}, storageError("segment_read_segment", err)
		}
		id, parseErr := model.ParseID(raw)
		if parseErr != nil {
			rows.Close()
			return ConversationSegmentSnapshot{}, storageError("segment_read_event_id", parseErr)
		}
		eventIDs = append(eventIDs, id)
		snapshot.Segments = append(snapshot.Segments, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ConversationSegmentSnapshot{}, storageError("segment_read_segment_rows", err)
	}
	for i, id := range eventIDs {
		snapshot.Segments[i].Event, err = s.context(ctx, tx, task.GraphID, id)
		if err != nil {
			return ConversationSegmentSnapshot{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return ConversationSegmentSnapshot{}, storageError("segment_read_commit", err)
	}
	return snapshot, nil
}
