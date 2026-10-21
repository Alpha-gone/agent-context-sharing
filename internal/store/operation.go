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
type OperationKind = model.OperationKind

const (
	// OperationAdd는 새 컨텍스트를 기록한 연산이다.
	OperationAdd = model.OperationAdd
	// OperationUpdate는 기존 컨텍스트를 갱신한 연산이다.
	OperationUpdate = model.OperationUpdate
	// OperationSupersede는 새 파생 판으로 이전 파생을 대체한 연산이다.
	OperationSupersede = model.OperationSupersede
	// OperationDiscard는 컨텍스트를 폐기한 연산이다.
	OperationDiscard = model.OperationDiscard
	// OperationKeep는 상태를 바꾸지 않고 유지하기로 판단한 연산이다.
	OperationKeep = model.OperationKeep
)

// OperationRecord은 적용 또는 거부한 관리 연산을 기록하는 입력이다.
//
// 대상은 컨텍스트이거나 사건 관계이며 둘을 동시에 가리키지 않는다. 「기록 항목」이 정한
// 구분이며, 관계에는 판 번호가 없어 RelationID를 쓰는 기록은 TargetVersion을 남기지 않는다.
type OperationRecord struct {
	Kind          OperationKind
	GraphID       model.ID
	ContextID     model.ID
	RelationID    model.ID
	TargetVersion int64
	JudgmentInput string
	AccountID     model.ID
	AgentID       model.ID
}

// forRelation은 대상이 사건 관계인지 알려준다.
func (operation OperationRecord) forRelation() bool { return operation.RelationID.IsV7() }

// RecordRejectedOperation은 되돌린 변경 트랜잭션 밖에 거부 기록을 남긴다.
func (s *Store) RecordRejectedOperation(ctx context.Context, operation OperationRecord, reason string) (err error) {
	defer func() { err = writeFailure(ctx, "rejected_operation", err) }()
	if err := operation.valid("rejected"); err != nil {
		return err
	}
	if reason == "" {
		return fmt.Errorf("거부 사유가 비어 있다")
	}
	queryer := operationQueryer(s.pool)
	if tx, ok := ctx.Value(writeTransactionContextKey{}).(pgx.Tx); ok {
		queryer = tx
	}
	return s.insertOperation(ctx, queryer, operation, "rejected", reason)
}

// HasAppliedDiscard는 현재 삭제에 대응하는 MCP 폐기 기록이 있는지 확인한다.
// 상태 변경의 허용 판정은 RestoreContext가 정점 잠금 뒤 같은 트랜잭션에서 다시 수행한다.
func (s *Store) HasAppliedDiscard(ctx context.Context, graphID, contextID model.ID) (bool, error) {
	if !graphID.IsV7() || !contextID.IsV7() {
		return false, fmt.Errorf("그래프와 컨텍스트 식별자는 UUIDv7이어야 한다")
	}
	return s.hasAppliedDiscard(ctx, s.pool, graphID, contextID)
}

func (s *Store) hasAppliedDiscard(ctx context.Context, queryer cypherQueryer, graphID, contextID model.ID) (bool, error) {
	var found bool
	if err := queryer.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM public.operation_log AS operation
			JOIN `+s.contextTable()+` AS node
			  ON node.properties ->> 'graph_id'::text = operation.graph_id::text
			 AND node.properties ->> 'context_id'::text = operation.context_id::text
			WHERE operation.graph_id = $1 AND operation.context_id = $2
			  AND operation.operation_kind = 'discard' AND operation.result = 'applied'
			  AND node.properties ->> 'deleted_at'::text IS NOT NULL
			  AND operation.applied_at >= (node.properties ->> 'deleted_at'::text)::timestamptz
		)`, graphID.String(), contextID.String()).Scan(&found); err != nil {
		return false, fmt.Errorf("관리 연산 폐기 기록 조회: %w", err)
	}
	return found, nil
}

// HasOperationJudgment는 그래프의 관리 연산 기록에 같은 판단 입력이
// 존재하는지 확인한다. 지속 평가는 본문 대신 시나리오 표식을 판단 입력에
// 남겨 반복 강화 여부를 검증한다.
func (s *Store) HasOperationJudgment(ctx context.Context, graphID model.ID, judgmentInput string) (bool, error) {
	if !graphID.IsV7() || judgmentInput == "" {
		return false, fmt.Errorf("관리 연산 판단 조회 인자가 올바르지 않다")
	}
	var found bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM public.operation_log
			WHERE graph_id = $1 AND judgment_input = $2
		)`, graphID.String(), judgmentInput).Scan(&found); err != nil {
		return false, fmt.Errorf("관리 연산 판단 입력 조회: %w", err)
	}
	return found, nil
}

// valid는 적용과 거부의 대상 요구가 다르므로 기록 결과를 함께 본다. 추가와 확정의 거부는
// 「기록 항목」이 대상을 비우기로 확정했고, 적용 기록은 언제나 대상을 하나 갖는다.
func (operation OperationRecord) valid(result string) error {
	// 대상의 잘못된 UUID를 대상 없음으로 바꾸지 않고 논리 모델과 같은 계약으로 검사한다.
	var reason string
	if result == "rejected" {
		reason = "validation"
	}
	return operation.managementOperation(time.Now().UTC(), result, reason).Validate()
}

func (operation OperationRecord) managementOperation(at time.Time, result, reason string) model.ManagementOperation {
	return model.ManagementOperation{
		Kind: operation.Kind, GraphID: operation.GraphID, ContextID: operation.ContextID, RelationID: operation.RelationID,
		TargetVersion: operation.TargetVersion, JudgmentInput: operation.JudgmentInput, AccountID: operation.AccountID,
		AgentID: operation.AgentID, AppliedAt: at, Result: model.OperationResult(result), RejectionReason: reason,
	}
}

func (s *Store) recordAppliedOperation(ctx context.Context, tx pgx.Tx, operation *OperationRecord, targetVersion int64) error {
	if operation == nil {
		return nil
	}
	copy := *operation
	if !copy.forRelation() {
		copy.TargetVersion = targetVersion
	}
	if err := copy.valid("applied"); err != nil {
		return err
	}
	return s.insertOperation(ctx, tx, copy, "applied", "")
}

// recordAppliedRelationOperation은 실제로 저장된 관계 식별자로 적용 기록을 남긴다.
// 확정 요청이 미리 만든 식별자는 기존 후보를 확정할 때 쓰이지 않으므로, 그대로 기록하면
// 기록이 존재하지 않는 관계를 가리킨다.
func (s *Store) recordAppliedRelationOperation(ctx context.Context, tx pgx.Tx, operation *OperationRecord, relationID model.ID) error {
	if operation == nil {
		return nil
	}
	copy := *operation
	copy.ContextID = model.ID{}
	copy.RelationID = relationID
	copy.TargetVersion = 0
	if err := copy.valid("applied"); err != nil {
		return err
	}
	return s.insertOperation(ctx, tx, copy, "applied", "")
}

// 확정은 실제 저장된 ID로 기록 대상을 채우지만, 호출자가 다른 업무·종류·대상을
// 지정한 기록까지 덮어써서 정상 감사처럼 저장하지 않는다.
func validateRelationOperation(operation *OperationRecord, graphID, relationID model.ID, kind OperationKind) error {
	if operation == nil {
		return nil
	}
	if operation.GraphID != graphID || operation.Kind != kind || !operation.ContextID.IsZero() ||
		operation.TargetVersion != 0 || !operation.RelationID.IsZero() && operation.RelationID != relationID {
		return model.FieldError{Field: "relation_id", Message: "관리 기록의 그래프·대상·연산 종류가 실제 관계 연산과 일치해야 한다"}
	}
	copy := *operation
	copy.RelationID = relationID
	return copy.valid("applied")
}

// enqueueIndexTask는 색인 대기 작업을 컨텍스트 변경과 같은 트랜잭션에서 등록한다.
// 컨텍스트마다 한 행만 두므로 같은 행이 있으면 대기 상태로 되돌린다.
func (s *Store) enqueueIndexTask(ctx context.Context, tx pgx.Tx, graphID, contextID model.ID, layer model.Layer) error {
	if !graphID.IsV7() || !contextID.IsV7() {
		return fmt.Errorf("색인 작업 식별자가 UUIDv7이 아니다")
	}
	switch layer {
	case model.LayerSource, model.LayerDerived, model.LayerEvent:
	default:
		return fmt.Errorf("색인 작업 대상 계층이 올바르지 않다")
	}
	// 「색인 대상 비교」의 원천 제외 구성에서는 등록하지 않는다. 등록 조건만 달라지고
	// 저장·간선·기록은 그대로이므로 두 구성이 나머지 조건에서 같다.
	if !s.indexTargets.indexes(layer) {
		return nil
	}
	taskID, err := model.NewID()
	if err != nil {
		return fmt.Errorf("색인 작업 식별자 생성: %w", err)
	}
	now := time.Now().UTC()
	var correlationID *string
	if id := model.CorrelationID(ctx); !id.IsZero() {
		correlationID = new(id.String())
	}
	var storedTask string
	var generation time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO public.index_task (
			task_id, context_id, graph_id, attempts, state, enqueued_at, next_attempt_at, correlation_id
		) VALUES ($1, $2, $3, 0, 'pending', $4, $4, $5)
		ON CONFLICT (context_id) DO UPDATE SET
			attempts = 0, state = 'pending', last_error = NULL,
			enqueued_at = EXCLUDED.enqueued_at, next_attempt_at = EXCLUDED.next_attempt_at,
			correlation_id = EXCLUDED.correlation_id
		WHERE index_task.graph_id = EXCLUDED.graph_id
 RETURNING task_id::text,enqueued_at`,
		taskID.String(), contextID.String(), graphID.String(), now, correlationID).Scan(&storedTask, &generation)
	if err != nil {
		return fmt.Errorf("색인 작업 등록: %w", err)
	}
	id, parseErr := model.ParseID(storedTask)
	if parseErr != nil {
		return storageError("index_task_identifier", parseErr)
	}
	task := IndexTask{ID: id, ContextID: contextID, GraphID: graphID, Generation: generation}
	if observed, ok := tx.(*observedWriteTx); ok {
		observed.callbacks = append(observed.callbacks, func(state string) {
			result, reason := "pending", "completed"
			if state != "committed" {
				result, reason = "skipped", "commit_unknown"
			}
			s.indexObservation(ctx, task, "enqueue", result, reason, "")
		})
	} else {
		s.indexObservation(ctx, task, "enqueue", "pending", "completed", "")
	}

	return nil
}

type operationQueryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (s *Store) insertOperation(ctx context.Context, queryer operationQueryer, operation OperationRecord, result, reason string) error {
	if observation, ok := ctx.Value(managementObservationKey{}).(*managementObservation); ok {
		observation.stage = "audit"
	}
	at := time.Now().UTC()
	if err := operation.managementOperation(at, result, reason).Validate(); err != nil {
		return err
	}
	operationID, err := model.NewID()
	if err != nil {
		return fmt.Errorf("관리 연산 기록 식별자 생성: %w", err)
	}
	var contextID, relationID *string
	var targetVersion *int64
	switch {
	case operation.forRelation():
		identifier := operation.RelationID.String()
		relationID = &identifier
	case operation.ContextID.IsV7():
		identifier := operation.ContextID.String()
		version := operation.TargetVersion
		contextID, targetVersion = &identifier, &version
	}
	_, err = queryer.Exec(ctx, `
		INSERT INTO public.operation_log (
			operation_id, graph_id, operation_kind, context_id, relation_id, target_version,
			judgment_input, actor_account_id, actor_agent_id, applied_at, result, reject_reason
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		operationID.String(), operation.GraphID.String(), string(operation.Kind), contextID, relationID, targetVersion,
		operation.JudgmentInput, operation.AccountID.String(), operation.AgentID.String(), at, result, nullableString(reason),
	)
	if err != nil {
		return fmt.Errorf("관리 연산 기록: %w", err)
	}
	return nil
}
