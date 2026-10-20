package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

const idempotencyRetention = 24 * time.Hour

// IdempotencyRequest은 MCP 전송 계층이 검증한 재시도 식별자와 요청 지문이다.
type IdempotencyRequest struct {
	AccountID   model.ID
	Key         model.ID
	ToolName    string
	Fingerprint [sha256.Size]byte
}

type writeTransactionContextKey struct{}

type afterWriteCommitContextKey struct{}

type afterWriteCommit struct {
	callbacks []func()
}

// ErrIdempotencyConflict는 같은 키를 다른 도구 또는 인자에 쓴 경우다.
var ErrIdempotencyConflict = errors.New("멱등성 키가 다른 요청에 쓰였다")

// ReplayIdempotent는 기존 UUID 호출의 호환 진입점이다. 새 MCP 호출은
// 문자열 키·현재 권한 재검사를 필수로 받는 ExecuteWrite를 사용한다.
func (s *Store) ReplayIdempotent(ctx context.Context, request IdempotencyRequest, run func(context.Context) ([]byte, error)) ([]byte, error) {
	if !request.AccountID.IsV7() || !request.Key.IsV7() || request.ToolName == "" || run == nil {
		return nil, fmt.Errorf("멱등성 요청 인자가 올바르지 않다")
	}
	return s.replayWrite(ctx, request.AccountID, request.Key.String(), request.ToolName, request.Fingerprint, func(context.Context) error { return nil }, run)
}

// ExecuteWrite는 완료 도메인 객체만 보관하며 check를 잠금 대기 뒤에도 실행한다.
// check는 읽기 전용이고, apply의 결정적 거부는 JSON 객체·nil error로 반환한다.
// error는 미완료 실패이며 바깥 예약·쓰기를 모두 되돌린다.
func (s *Store) ExecuteWrite(ctx context.Context, account model.ID, key, tool string, normalized []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !account.IsV7() || check == nil || apply == nil {
		return nil, fmt.Errorf("쓰기 주체와 처리기가 필요하다")
	}
	switch tool {
	case "graph_create", "graph_update", "node_create", "node_update", "node_discard", "node_restore", "relation_confirm", "relation_discard":
	default:
		return nil, model.FieldError{Field: "name", Message: "쓰기 도구를 확인해 주십시오."}
	}
	if _, ok := ctx.Value(writeTransactionContextKey{}).(pgx.Tx); ok {
		return nil, storageError("nested_idempotency", errors.New("재생 경계는 중첩할 수 없다"))
	}
	if key != "" {
		if err := model.ValidateRegistrationKey(key); err != nil {
			return nil, model.FieldError{Field: "request_id", Message: "쓰기 키를 확인해 주십시오."}
		}
	}
	if len(normalized) == 0 || !jsontext.Value(normalized).IsValid() {
		return nil, storageError("write_fingerprint", errors.New("정규화 인자가 필요하다"))
	}
	if key == "" {
		if err := check(ctx); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := apply(ctx)
		if err != nil {
			return nil, writeFailure(ctx, "write_apply", storageError("write_apply", err))
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !completedWriteJSON(result) {
			return nil, storageError("write_result", errors.New("완료 도메인 객체가 필요하다"))
		}
		return result, nil
	}
	return s.replayWrite(ctx, account, key, tool, sha256.Sum256(normalized), check, apply)
}

func (s *Store) replayWrite(ctx context.Context, account model.ID, key, tool string, fingerprint [sha256.Size]byte, check func(context.Context) error, run func(context.Context) ([]byte, error)) (result []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Value(writeTransactionContextKey{}).(pgx.Tx); ok {
		return nil, storageError("nested_idempotency", errors.New("재생 경계는 중첩할 수 없다"))
	}
	// 바깥 예약은 업무 저장점과 공유한다. 잠금 대기 중에도 풀 읽기 한 자리를 남긴다.
	release, err := s.reserveConnections(ctx, idempotencyConnections)
	if err != nil {
		return nil, err
	}
	defer release()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, storageError("idempotency_begin", connectionError(ctx, "재생 트랜잭션 시작", err))
	}
	defer func() {
		if cleanupErr := rollbackTransaction(ctx, tx); cleanupErr != nil {
			err = errors.Join(err, storageError("idempotency_rollback", cleanupErr))
		}
		err = writeFailure(ctx, "idempotency", err)
		if err != nil {
			result = nil
		}
	}()
	lockInput := append(append([]byte{}, account[:]...), []byte(key)...)
	digest := sha256.Sum256(lockInput)
	var lock int64
	for _, octet := range digest[:8] {
		lock = lock<<8 | int64(octet)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lock); err != nil {
		return nil, storageError("idempotency_lock", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 모든 인스턴스가 같은 DB 시계로 잠금 대기 뒤의 수명을 판정한다.
	var received time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&received); err != nil {
		return nil, storageError("idempotency_clock", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.idempotency_record WHERE actor_account_id=$1 AND idempotency_key=$2 AND expires_at <= $3`, account.String(), key, received); err != nil {
		return nil, storageError("idempotency_expiry", err)
	}
	inserted, err := tx.Exec(ctx, `
		INSERT INTO public.idempotency_record(actor_account_id,idempotency_key,tool_name,request_fingerprint,tool_result,received_at,expires_at)
		VALUES($1,$2,$3,$4,'{}'::jsonb,$5,$6) ON CONFLICT DO NOTHING`, account.String(), key, tool, fingerprint[:], received, received.Add(idempotencyRetention))
	if err != nil {
		return nil, storageError("idempotency_reserve", err)
	}
	var storedTool string
	var storedFingerprint, stored []byte
	if err := tx.QueryRow(ctx, `SELECT tool_name,request_fingerprint,tool_result FROM public.idempotency_record WHERE actor_account_id=$1 AND idempotency_key=$2 FOR UPDATE`, account.String(), key).Scan(&storedTool, &storedFingerprint, &stored); err != nil {
		return nil, storageError("idempotency_lookup", err)
	}
	if storedTool != tool || !bytes.Equal(storedFingerprint, fingerprint[:]) {
		return nil, model.FieldError{Field: "request_id", Message: "같은 쓰기 키의 요청이 다릅니다.", Cause: ErrIdempotencyConflict}
	}
	if inserted.RowsAffected() == 0 {
		if !completedWriteJSON(stored) {
			return nil, storageError("idempotency_stored_result", errors.New("완료 결과를 확인할 수 없다"))
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, storageError("idempotency_replay_commit", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return stored, nil
	}
	afterCommit := &afterWriteCommit{}
	transactionContext := context.WithValue(ctx, writeTransactionContextKey{}, tx)
	transactionContext = context.WithValue(transactionContext, afterWriteCommitContextKey{}, afterCommit)
	result, err = run(transactionContext)
	if err != nil {
		return nil, storageError("idempotency_apply", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !completedWriteJSON(result) {
		return nil, storageError("idempotency_result", errors.New("완료 도메인 객체가 필요하다"))
	}
	// 최초 응답과 재생에 같은 jsonb 표현을 사용한다. 빈 예약은 커밋하지 않는다.
	if err := tx.QueryRow(ctx, `UPDATE public.idempotency_record SET tool_result=$3::jsonb WHERE actor_account_id=$1 AND idempotency_key=$2 RETURNING tool_result`, account.String(), key, result).Scan(&result); err != nil {
		return nil, storageError("idempotency_result_store", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, storageError("idempotency_commit", err)
	}
	for _, callback := range afterCommit.callbacks {
		callback()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func completedWriteJSON(data []byte) bool {
	var object map[string]jsontext.Value
	if json.Unmarshal(data, &object) != nil || len(object) == 0 {
		return false
	}
	for _, field := range []string{"jsonrpc", "resultType", "_meta", "access_token", "refresh_token"} {
		if _, present := object[field]; present {
			return false
		}
	}
	return true
}

// deferAfterWriteCommit은 호출자가 소유하지 않은 쓰기 트랜잭션의 커밋 뒤에만 실행할
// 부수 작업을 등록한다. 멱등성 트랜잭션이 롤백되면 콜백도 실행되지 않는다.
func deferAfterWriteCommit(ctx context.Context, callback func()) bool {
	afterCommit, ok := ctx.Value(afterWriteCommitContextKey{}).(*afterWriteCommit)
	if !ok || callback == nil {
		return false
	}
	afterCommit.callbacks = append(afterCommit.callbacks, callback)
	return true
}

// writeTransaction은 업무 쓰기 트랜잭션을 연다. 멱등성 트랜잭션 안에서는 저장점을 열어,
// 쓰기 뒤에 드러난 도메인 거부가 앞선 변경을 되돌린 채 거부 기록과 완료 결과만 확정되게
// 한다. 저장점의 Commit은 해제일 뿐이고 실제 커밋은 멱등성 트랜잭션이 한다.
func (s *Store) writeTransaction(ctx context.Context) (pgx.Tx, error) {
	if tx, ok := ctx.Value(writeTransactionContextKey{}).(pgx.Tx); ok {
		nested, err := tx.Begin(ctx)
		return nested, storageError("write_savepoint", connectionError(ctx, "저장점 시작", err))
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	return tx, storageError("write_begin", connectionError(ctx, "쓰기 트랜잭션 시작", err))
}

// CleanupExpiredIdempotencyRecords는 보관 기간이 끝난 재시도 결과를 지운다.
func (s *Store) CleanupExpiredIdempotencyRecords(ctx context.Context, now time.Time) (int, error) {
	return s.deleteBefore(ctx, "멱등성 결과 정리", `DELETE FROM public.idempotency_record WHERE expires_at <= $1`, now)
}
