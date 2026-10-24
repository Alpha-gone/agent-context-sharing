package store

import (
	"context"
	"errors"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// Diagnostics는 고정 코드만 받으며 기록 누락으로 업무 결과를 바꾸지 않는다.
// 요청 객체·본문·error·URL·HTTP 헤더·자격은 전달하지 않는다.
type Diagnostics interface {
	RecordDiagnostic(ctx context.Context, level, stage, result, reasonCode string) bool
}

// SetDiagnostics는 조립 시 기록기를 주입한다. 요청·작업자를 시작하기 전에만 호출한다.
// nil이면 진단 전달을 생략하며 기존 실행기의 동작을 유지한다.
func (s *Store) SetDiagnostics(diagnostics Diagnostics) {
	s.diagnostics = diagnostics
}

// SetOperationDiagnostics는 업무 감사와 독립된 관리·색인 이벤트 수신기를 조립한다.
func (s *Store) SetOperationDiagnostics(management, index diagnostic.Observer) {
	s.managementDiagnostics, s.indexDiagnostics = management, index
}

type managementObservation struct {
	stage, state string
	fields       diagnostic.Fields
}
type managementObservationKey struct{}

func beginManagement(ctx context.Context, operation string) (context.Context, *managementObservation) {
	observation := &managementObservation{stage: "argument_validation", state: "unknown", fields: diagnostic.Fields{Management: diagnostic.ManagementFields{Operation: operation}}}
	return context.WithValue(ctx, managementObservationKey{}, observation), observation
}
func (s *Store) finishManagement(ctx context.Context, observation *managementObservation, err error) {
	level, result, reason := "INFO", "completed", "completed"
	if err != nil {
		level, result, reason = "WARN", "failed", "constraint_violation"
		_, internal := errors.AsType[*StorageError](err)
		if internal {
			level, reason = "ERROR", "store_unavailable"
		}
		if field, ok := errors.AsType[model.FieldError](err); ok && !internal {
			observation.fields.Management.ValidationField = diagnostic.ValidationField(field.Field)
			reason = "invalid_argument"
		}
		if !internal {
			switch observation.stage {
			case "layer_check":
				reason = "layer_mismatch"
			case "time_check":
				reason = "time_invalid"
			case "cycle_check":
				if _, ok := errors.AsType[model.FieldError](err); ok {
					reason = "cycle_detected"
				}
			}
		}
		if errors.Is(err, ErrNotFound) && !internal {
			reason = "not_found"
		}
		if observation.state == "unknown" && observation.stage == "commit" {
			reason = "commit_unknown"
		}
		if errors.Is(err, context.Canceled) {
			reason = "canceled"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "deadline_exceeded"
		}
	}
	fields := observation.fields
	emit := func(state string) {
		fields.Management.CommitState = state
		if err == nil && state != "committed" {
			result = state
			if state == "unknown" {
				reason = "commit_unknown"
			}
		}
		diagnostic.Observe(s.managementDiagnostics, ctx, level, observation.stage, result, reason, fields.Identity != (diagnostic.IdentityFields{}), fields)
	}
	// 저장점 해제를 실제 커밋으로 기록하지 않는다. 거부된 저장점은 이미 되돌렸으므로 바로 보고한다.
	if observation.state == "pending" {
		if outcomes, ok := ctx.Value(afterWriteCommitContextKey{}).(*afterWriteCommit); ok {
			outcomes.outcomes = append(outcomes.outcomes, emit)
			return
		}
	}
	state := observation.state
	if state == "pending" {
		state = "unknown"
	}
	emit(state)
}

type observedWriteTx struct {
	pgx.Tx
	observation *managementObservation
	nested      bool
	callbacks   []func(string)
}

func (tx *observedWriteTx) Commit(ctx context.Context) error {
	tx.observation.stage = "commit"
	err := tx.Tx.Commit(ctx)
	switch {
	case err == nil:
		tx.observation.state = "committed"
		if tx.nested {
			tx.observation.state = "pending"
		}
	case errors.Is(err, pgx.ErrTxCommitRollback):
		tx.observation.state = "rolled_back"
	default:
		tx.observation.state = "unknown"
	}
	for _, report := range tx.callbacks {
		if tx.nested && err == nil {
			if outcomes, ok := ctx.Value(afterWriteCommitContextKey{}).(*afterWriteCommit); ok {
				outcomes.outcomes = append(outcomes.outcomes, report)
				continue
			}
		}
		state := "unknown"
		if err == nil && !tx.nested {
			state = "committed"
		}
		report(state)
	}
	tx.callbacks = nil
	return err
}
func (tx *observedWriteTx) Rollback(ctx context.Context) error {
	err := tx.Tx.Rollback(ctx)
	if err == nil {
		tx.observation.state = "rolled_back"
	} else if !errors.Is(err, pgx.ErrTxClosed) {
		tx.observation.state = "unknown"
	}
	for _, report := range tx.callbacks {
		state := "unknown"
		if err == nil {
			state = "rolled_back"
		}
		report(state)
	}
	tx.callbacks = nil
	return err
}
func observeWriteTx(ctx context.Context, tx pgx.Tx, nested bool) pgx.Tx {
	observation, ok := ctx.Value(managementObservationKey{}).(*managementObservation)
	if tx == nil {
		return nil
	}
	if !ok {
		return &observedWriteTx{Tx: tx, observation: &managementObservation{state: "unknown"}, nested: nested}
	}
	return &observedWriteTx{Tx: tx, observation: observation, nested: nested}
}

func (s *Store) indexObservation(ctx context.Context, task IndexTask, stage, result, reason, lease string) {
	level := "INFO"
	if result == "failed" {
		level = "WARN"
	}
	diagnostic.Observe(s.indexDiagnostics, ctx, level, stage, result, reason, true, diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: task.GraphID.String(), ContextID: task.ContextID.String()}, Index: diagnostic.IndexFields{TaskID: task.ID.String(), Generation: uint64(task.Generation.UnixMicro()), Attempts: uint64(task.Attempts), LeaseResult: lease}})
}
