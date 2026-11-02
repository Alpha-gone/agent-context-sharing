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

func TestUnkeyedWriteRunsPostCommitOnlyAfterCompletion(t *testing.T) {
	for _, mode := range []string{"complete", "apply_failure", "incomplete", "cancel_during_apply"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var database Store
			callbacks := 0
			result, err := database.ExecuteWrite(ctx, newTestID(t), "", "node_create", []byte(`{}`), writeReplayCheck, func(applyCtx context.Context) ([]byte, error) {
				if !deferAfterWriteCommit(applyCtx, func() { callbacks++; cancel() }) {
					t.Error("단독 쓰기의 후처리 경계 누락")
				}
				if callbacks != 0 {
					t.Error("apply 완료 전 후처리 실행")
				}
				switch mode {
				case "apply_failure":
					return nil, errors.New("synthetic apply failure")
				case "incomplete":
					return nil, nil
				case "cancel_during_apply":
					cancel()
				}
				return []byte(`{"ok":true}`), nil
			})
			if mode == "complete" {
				if err != nil || len(result) == 0 || callbacks != 1 {
					t.Fatalf("완료 결과/후처리: %s %v calls=%d", result, err, callbacks)
				}
			} else if err == nil || result != nil || callbacks != 0 {
				t.Fatalf("미완료 후처리 실행: %s %v calls=%d", result, err, callbacks)
			}
		})
	}
}
