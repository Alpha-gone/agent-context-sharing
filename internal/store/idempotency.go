package store

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// ReplayIdempotent는 같은 계정·키의 완료 결과를 재생하거나, 새 요청의 결과를 보관한다.
//
// 저장소는 계정·키 자문 잠금을 보유한 채 업무 함수를 실행해 동시 재시도를 직렬화한다.
// 업무 쓰기가 자체 트랜잭션을 소유하는 기존 공개 계약을 보존하면서도, 완료 결과를 쓰기
// 뒤 응답 전 보관해 응답 전달 실패를 같은 키로 회수할 수 있게 한다.
func (s *Store) ReplayIdempotent(ctx context.Context, request IdempotencyRequest, run func(context.Context) ([]byte, error)) ([]byte, error) {
	if !request.AccountID.IsV7() || !request.Key.IsV7() || request.ToolName == "" || run == nil {
		return nil, fmt.Errorf("멱등성 요청 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("멱등성 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	// 계정과 키를 함께 두 int64 잠금으로 나눠, 다른 계정의 같은 키가 서로 기다리지 않게 한다.
	lockInput := make([]byte, 0, len(request.AccountID)+len(request.Key))
	lockInput = append(lockInput, request.AccountID[:]...)
	lockInput = append(lockInput, request.Key[:]...)
	lockDigest := sha256.Sum256(lockInput)
	var lockKey int64
	for _, octet := range lockDigest[:8] {
		lockKey = lockKey<<8 | int64(octet)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("멱등성 요청 잠금: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `DELETE FROM public.idempotency_record WHERE actor_account_id = $1 AND idempotency_key = $2 AND expires_at <= $3`, request.AccountID.String(), request.Key.String(), now); err != nil {
		return nil, fmt.Errorf("만료 멱등성 결과 삭제: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.idempotency_record (
			actor_account_id, idempotency_key, tool_name, request_fingerprint, tool_result, received_at, expires_at
		) VALUES ($1, $2, $3, $4, '{}'::jsonb, $5, $6)
		ON CONFLICT DO NOTHING`, request.AccountID.String(), request.Key.String(), request.ToolName, request.Fingerprint[:], now, now.Add(idempotencyRetention)); err != nil {
		return nil, fmt.Errorf("멱등성 요청 예약: %w", err)
	}
	var toolName string
	var fingerprint, stored []byte
	if err := tx.QueryRow(ctx, `
		SELECT tool_name, request_fingerprint, tool_result
		FROM public.idempotency_record
		WHERE actor_account_id = $1 AND idempotency_key = $2
		FOR UPDATE`, request.AccountID.String(), request.Key.String()).Scan(&toolName, &fingerprint, &stored); err != nil {
		return nil, fmt.Errorf("멱등성 요청 조회: %w", err)
	}
	if toolName != request.ToolName || !bytes.Equal(fingerprint, request.Fingerprint[:]) {
		return nil, ErrIdempotencyConflict
	}
	if string(stored) != "{}" {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("멱등성 재생 커밋: %w", err)
		}
		return stored, nil
	}
	afterCommit := &afterWriteCommit{}
	transactionContext := context.WithValue(ctx, writeTransactionContextKey{}, tx)
	transactionContext = context.WithValue(transactionContext, afterWriteCommitContextKey{}, afterCommit)
	result, err := run(transactionContext)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE public.idempotency_record
		SET tool_result = $3::jsonb
		WHERE actor_account_id = $1 AND idempotency_key = $2`, request.AccountID.String(), request.Key.String(), result); err != nil {
		return nil, fmt.Errorf("멱등성 결과 저장: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("멱등성 결과 커밋: %w", err)
	}
	for _, callback := range afterCommit.callbacks {
		callback()
	}
	return result, nil
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
		return tx.Begin(ctx)
	}
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}

// CleanupExpiredIdempotencyRecords는 보관 기간이 끝난 재시도 결과를 지운다.
func (s *Store) CleanupExpiredIdempotencyRecords(ctx context.Context, now time.Time) (int, error) {
	return s.deleteBefore(ctx, "멱등성 결과 정리", `DELETE FROM public.idempotency_record WHERE expires_at <= $1`, now)
}
