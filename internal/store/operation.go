package store

import (
	"context"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// OperationKind는 컨텍스트 관리 연산 기록에 보관하는 다섯 동작 중 하나다.
type OperationKind string

const (
	// OperationAdd는 새 컨텍스트를 기록한 연산이다.
	OperationAdd OperationKind = "add"
	// OperationUpdate는 기존 컨텍스트를 갱신한 연산이다.
	OperationUpdate OperationKind = "update"
	// OperationDiscard는 컨텍스트를 폐기한 연산이다.
	OperationDiscard OperationKind = "discard"
)

// OperationRecord은 적용 또는 거부한 컨텍스트 관리 연산을 기록하는 입력이다.
type OperationRecord struct {
	Kind          OperationKind
	GraphID       model.ID
	ContextID     model.ID
	TargetVersion int64
	JudgmentInput string
	AccountID     model.ID
	AgentID       model.ID
}

// RecordRejectedOperation은 되돌린 변경 트랜잭션 밖에 거부 기록을 남긴다.
func (s *Store) RecordRejectedOperation(ctx context.Context, operation OperationRecord, reason string) error {
	if err := operation.valid(); err != nil {
		return err
	}
	if reason == "" {
		return fmt.Errorf("거부 사유가 비어 있다")
	}
	return s.insertOperation(ctx, s.pool, operation, "rejected", reason)
}

// HasAppliedDiscard는 컨텍스트가 MCP 관리 연산으로 폐기됐는지 확인한다.
func (s *Store) HasAppliedDiscard(ctx context.Context, graphID, contextID model.ID) (bool, error) {
	if !graphID.IsV7() || !contextID.IsV7() {
		return false, fmt.Errorf("그래프와 컨텍스트 식별자는 UUIDv7이어야 한다")
	}
	var found bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM public.operation_log
			WHERE graph_id = $1 AND context_id = $2 AND operation_kind = 'discard' AND result = 'applied'
		)`, graphID.String(), contextID.String()).Scan(&found); err != nil {
		return false, fmt.Errorf("관리 연산 폐기 기록 조회: %w", err)
	}
	return found, nil
}

func (operation OperationRecord) valid() error {
	if operation.Kind != OperationAdd && operation.Kind != OperationUpdate && operation.Kind != OperationDiscard {
		return fmt.Errorf("관리 연산 종류 %q가 올바르지 않다", operation.Kind)
	}
	if !operation.GraphID.IsV7() || !operation.ContextID.IsV7() || !operation.AccountID.IsV7() || !operation.AgentID.IsV7() {
		return fmt.Errorf("관리 연산 식별자가 UUIDv7이 아니다")
	}
	if operation.TargetVersion < 1 || operation.JudgmentInput == "" {
		return fmt.Errorf("관리 연산 대상 판 또는 판단 입력이 올바르지 않다")
	}
	return nil
}

func (s *Store) recordAppliedOperation(ctx context.Context, tx pgx.Tx, operation *OperationRecord, targetVersion int64) error {
	if operation == nil {
		return nil
	}
	copy := *operation
	copy.TargetVersion = targetVersion
	if err := copy.valid(); err != nil {
		return err
	}
	return s.insertOperation(ctx, tx, copy, "applied", "")
}

// enqueueIndexTask는 색인 대기 작업을 컨텍스트 변경과 같은 트랜잭션에서 등록한다.
// 컨텍스트마다 한 행만 두므로 같은 행이 있으면 대기 상태로 되돌린다.
func (s *Store) enqueueIndexTask(ctx context.Context, tx pgx.Tx, graphID, contextID model.ID) error {
	if !graphID.IsV7() || !contextID.IsV7() {
		return fmt.Errorf("색인 작업 식별자가 UUIDv7이 아니다")
	}
	taskID, err := model.NewID()
	if err != nil {
		return fmt.Errorf("색인 작업 식별자 생성: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.index_task (
			task_id, context_id, graph_id, attempts, state, enqueued_at, next_attempt_at
		) VALUES ($1, $2, $3, 0, 'pending', $4, $4)
		ON CONFLICT (context_id) DO UPDATE SET
			attempts = 0, state = 'pending', last_error = NULL,
			enqueued_at = EXCLUDED.enqueued_at, next_attempt_at = EXCLUDED.next_attempt_at`,
		taskID.String(), contextID.String(), graphID.String(), now); err != nil {
		return fmt.Errorf("색인 작업 등록: %w", err)
	}
	return nil
}

type operationQueryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (s *Store) insertOperation(ctx context.Context, queryer operationQueryer, operation OperationRecord, result, reason string) error {
	operationID, err := model.NewID()
	if err != nil {
		return fmt.Errorf("관리 연산 기록 식별자 생성: %w", err)
	}
	_, err = queryer.Exec(ctx, `
		INSERT INTO public.operation_log (
			operation_id, graph_id, operation_kind, context_id, target_version,
			judgment_input, actor_account_id, actor_agent_id, applied_at, result, reject_reason
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		operationID.String(), operation.GraphID.String(), string(operation.Kind), operation.ContextID.String(), operation.TargetVersion,
		operation.JudgmentInput, operation.AccountID.String(), operation.AgentID.String(), time.Now().UTC(), result, nullableString(reason),
	)
	if err != nil {
		return fmt.Errorf("관리 연산 기록: %w", err)
	}
	return nil
}
