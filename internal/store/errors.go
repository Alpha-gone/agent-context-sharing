package store

import (
	"context"
	"errors"
	"net"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// StorageError는 DB 또는 저장 값 해석의 내부 장애다. Error에는 입력·SQL·DB 상세를
// 넣지 않으며 원인 분석과 errors.Is/As에는 Unwrap으로 원래 오류를 제공한다.
type StorageError struct {
	operation string
	cause     error
}

func (err *StorageError) Error() string { return "저장소 처리 실패: " + err.operation }
func (err *StorageError) Unwrap() error { return err.cause }

func storageError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return &StorageError{operation: operation, cause: cause}
}

// writeFailure는 도메인 거부의 오류 타입을 유지하고 드라이버 장애만 감싼다.
func writeFailure(ctx context.Context, operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if ctx.Err() != nil {
		cause = errors.Join(cause, ctx.Err(), context.Cause(ctx))
	}
	_, stored := errors.AsType[*StorageError](cause)
	_, postgres := errors.AsType[*pgconn.PgError](cause)
	_, connect := errors.AsType[*pgconn.ConnectError](cause)
	_, network := errors.AsType[net.Error](cause)
	if stored || postgres || connect || network || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, pgx.ErrTxClosed) || errors.Is(cause, pgx.ErrTxCommitRollback) {
		return storageError(operation, cause)
	}
	return cause
}

// finishWrite는 실제 트랜잭션과 저장점 모두 바깥 예약이 해제되기 전에 정리한다.
func finishWrite(ctx context.Context, tx pgx.Tx, operation string, resultErr *error) {
	cleanupErr := storageError("write_rollback", rollbackTransaction(ctx, tx))
	*resultErr = writeFailure(ctx, operation, errors.Join(*resultErr, cleanupErr))
}
