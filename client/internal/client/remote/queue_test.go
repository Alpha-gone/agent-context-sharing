package remote

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

func await(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !check() {
		select {
		case <-deadline.C:
			t.Fatal("예상 상태를 기다리다 시간 초과했습니다")
		case <-tick.C:
		}
	}
}

func TestCallQueueFIFOOverflowAndCancellation(t *testing.T) {
	q := new(callQueue)
	for range 8 {
		if err := q.acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan int, 128)
	var wg sync.WaitGroup
	var cancels []context.CancelFunc
	for index := range 128 {
		ctx, cancel := context.WithCancel(t.Context())
		cancels = append(cancels, cancel)
		wg.Go(func() {
			if err := q.acquire(ctx); err != nil {
				if index != 40 || !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
				return
			}
			results <- index
		})
		await(t, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.waiters) == index+1 })
	}
	if err := q.acquire(t.Context()); !errors.Is(err, contract.ErrBusy) {
		t.Fatal("129번째 대기 요청을 수락했습니다")
	}
	cancels[40]()
	await(t, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.waiters) == 127 })
	for index := range 128 {
		if index == 40 {
			continue
		}
		q.release()
		if got := <-results; got != index {
			t.Fatalf("FIFO 순서 %d, 기대 %d", got, index)
		}
	}
	wg.Wait()
	for _, cancel := range cancels {
		cancel()
	}
	for range 8 {
		q.release()
	}
	if q.active != 0 || len(q.waiters) != 0 {
		t.Fatal("slot이나 대기자가 남았습니다")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := q.acquire(canceled); !errors.Is(err, context.Canceled) || q.active != 0 {
		t.Fatal("취소 요청이 slot을 점유했습니다")
	}
}

func TestCallQueueConcurrentLimit(t *testing.T) {
	q := new(callQueue)
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if err := q.acquire(t.Context()); err != nil {
				t.Error(err)
				return
			}
			defer q.release()
			current := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
			}
			time.Sleep(time.Millisecond)
		})
	}
	wg.Wait()
	if peak.Load() > 8 || q.active != 0 || len(q.waiters) != 0 {
		t.Fatal("동시 호출 상한이나 정리 계약이 다릅니다")
	}
}
