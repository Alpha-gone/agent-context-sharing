package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryEmbeddingAccess(t *testing.T) {
	conflict := &pgconn.PgError{Code: "40001"}
	other := &pgconn.PgError{Code: "40P01"}
	for _, test := range []struct {
		name     string
		failures []error
		want     error
	}{
		{"success", []error{nil}, nil},
		{"moved_partition", []error{fmt.Errorf("이동 충돌: %w", conflict), nil}, nil},
		{"retry_limit", []error{conflict, conflict}, conflict},
		{"retry_failure", []error{conflict, other}, other},
		{"other_failure", []error{other}, other},
		{"canceled", []error{context.Canceled}, context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := retryEmbeddingAccess(func() error {
				if calls >= len(test.failures) {
					t.Fatal("허용 횟수보다 더 재시도했다")
				}
				failure := test.failures[calls]
				calls++
				return failure
			})
			if !errors.Is(err, test.want) || calls != len(test.failures) {
				t.Fatalf("접근 기록 재시도 = %v, %d회, want %v, %d회", err, calls, test.want, len(test.failures))
			}
		})
	}
}
