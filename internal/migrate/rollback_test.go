package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type rollbackFailure struct {
	pgx.Tx
	failure error
	active  bool
}

func (tx *rollbackFailure) Rollback(ctx context.Context) error {
	tx.active = ctx.Err() == nil
	return tx.failure
}

func TestMigrationRollbackPreservesCause(t *testing.T) {
	primary, cleanup := errors.New("synthetic apply failure"), errors.New("synthetic rollback failure")
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"success", nil}, {"closed", pgx.ErrTxClosed}, {"failure", cleanup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			tx := &rollbackFailure{failure: tc.failure}
			err := primary
			rollbackMigration(ctx, tx, &err)
			if !tx.active || !errors.Is(err, primary) || errors.Is(err, cleanup) != (tc.failure == cleanup) {
				t.Fatalf("취소 독립 정리·원인 보존 실패: %v", err)
			}
		})
	}
}
