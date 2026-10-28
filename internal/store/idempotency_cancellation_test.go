package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestWritePreservesCauseWhenAlreadyCanceled(t *testing.T) {
	cause := errors.New("쓰기 시작 전 취소")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	for _, run := range []func(*Store) error{
		func(database *Store) error {
			_, err := database.ExecuteWrite(ctx, model.ID{}, "", "", nil, nil, nil)
			return err
		},
		func(database *Store) error {
			_, err := database.replayWrite(ctx, model.ID{}, "", "", [sha256.Size]byte{}, nil, nil)
			return err
		},
	} {
		if err := run(&Store{}); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatalf("취소 원인 보존: %v", err)
		}
	}
}
