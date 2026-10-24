package store

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTEST03ConcurrentWriteCommitBoundaryIntegration(t *testing.T) {
	for _, tool := range lifeWriteTools {
		for _, abort := range []bool{false, true} {
			mode := "commit"
			if abort {
				mode = "cancel"
			}
			t.Run(tool+"/"+mode, func(t *testing.T) {
				first, second, _ := newAgentContractDatabase(t)
				write := lifeAcceptanceFixture(t, first, tool)
				before := lifeAcceptanceFingerprint(t, first)
				ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
				initialContext, cancelInitial := context.WithCancel(ctx)
				gate := newFoundationCommitGate(t, 1)
				var workers sync.WaitGroup
				t.Cleanup(func() { stop(); cancelInitial(); gate.open(); workers.Wait() })
				type outcome struct {
					value []byte
					err   error
				}
				initial, waiting := make(chan outcome, 1), make(chan outcome, 1)
				var applies atomic.Int32
				result := []byte(`{"applied":true,"tool":"` + tool + `"}`)
				normalized := []byte(`{"tool":"` + tool + `","arguments":{}}`)
				const key = "test03_shared_key_1"
				workers.Go(func() {
					value, err := first.ExecuteWrite(initialContext, write.actor, key, tool, normalized, writeReplayCheck, func(ctx context.Context) ([]byte, error) {
						applies.Add(1)
						if err := write.run(ctx); err != nil {
							return nil, err
						}
						return result, gate.Wait(ctx)
					})
					initial <- outcome{value, err}
				})
				select {
				case <-gate.arrived:
				case got := <-initial:
					t.Fatalf("최초 쓰기가 커밋 경계에 도달하지 못했다: %v", got.err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				workers.Go(func() {
					value, err := second.ExecuteWrite(ctx, write.actor, key, tool, normalized, writeReplayCheck, func(ctx context.Context) ([]byte, error) {
						applies.Add(1)
						return result, write.run(ctx)
					})
					waiting <- outcome{value, err}
				})
				waitMCP05Lock(t, first)
				if got := lifeAcceptanceFingerprint(t, first); got != before {
					t.Fatal("미커밋 정점·간선·판·큐·저장량·감사·멱등성 결과가 외부에 보였다")
				}
				select {
				case got := <-waiting:
					t.Fatalf("커밋 전 대기 요청이 완료됐다: %v", got.err)
				default:
				}
				if abort {
					cancelInitial()
				} else {
					gate.open()
				}
				var firstResult, secondResult outcome
				select {
				case firstResult = <-initial:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case secondResult = <-waiting:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if abort {
					if !errors.Is(firstResult.err, context.Canceled) || firstResult.value != nil || applies.Load() != 2 {
						t.Fatalf("취소 롤백 뒤 대기자의 재실행: %v, 실행 %d", firstResult.err, applies.Load())
					}
				} else if firstResult.err != nil || !bytes.Equal(firstResult.value, secondResult.value) || applies.Load() != 1 {
					t.Fatalf("커밋 뒤 대기자의 중복 적용: %v, 실행 %d", firstResult.err, applies.Load())
				}
				var decoded struct {
					Applied bool   `json:"applied"`
					Tool    string `json:"tool"`
				}
				if err := json.Unmarshal(secondResult.value, &decoded); secondResult.err != nil || err != nil || !decoded.Applied || decoded.Tool != tool {
					t.Fatalf("두 번째 인스턴스 결과: %v", secondResult.err)
				}
				workers.Wait()
				after := lifeAcceptanceFingerprint(t, first)
				if after == before {
					t.Fatal("커밋된 업무·완료 결과가 없다")
				}
				for _, database := range []*Store{first, second} {
					value, err := database.ExecuteWrite(ctx, write.actor, key, tool, normalized, writeReplayCheck, func(context.Context) ([]byte, error) {
						t.Error("완료된 키를 재적용했다")
						return result, nil
					})
					if err != nil || !bytes.Equal(value, secondResult.value) || lifeAcceptanceFingerprint(t, first) != after {
						t.Fatalf("후속 재생이 전체 상태를 변경했다: %v", err)
					}
					assertConnectionRecovery(t, database, nil, 0)
				}
			})
		}
	}
}
