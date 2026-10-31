package index

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestConsumerPollingIntervals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		worker := &Worker{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		sequence := []bool{true, false, false, false, false, false, false, true, true, false}
		var observed []time.Duration
		start := time.Now()
		worker.consume(ctx, func(context.Context) (bool, error) {
			observed = append(observed, time.Since(start))
			result := sequence[len(observed)-1]
			if len(observed) == len(sequence) {
				cancel()
			}
			// 확보 실패도 빈 회차와 같은 상한을 사용해 반복 부하를 제한한다.
			return result, errors.New("synthetic cycle error")
		})
		seconds := []int{0, 1, 3, 7, 15, 31, 61, 91, 92, 93}
		want := make([]time.Duration, len(seconds))
		for i, second := range seconds {
			want[i] = time.Duration(second) * time.Second
		}
		if !slices.Equal(observed, want) {
			t.Fatalf("조회 시각=%v, want=%v", observed, want)
		}
	})
}

// TestConsumerProcessesBacklogWithoutWaiting은 작업을 처리한 회차 뒤에는 기다리지 않고 다음
// 작업을 찾고, 이어지는 빈 회차는 최소 간격부터 다시 늘리는지 확인한다.
func TestConsumerProcessesBacklogWithoutWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		worker := &Worker{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		sequence := []bool{true, true, false, false, true, false}
		var observed []time.Duration
		start := time.Now()
		worker.consume(ctx, func(context.Context) (bool, error) {
			observed = append(observed, time.Since(start))
			result := sequence[len(observed)-1]
			if len(observed) == len(sequence) {
				cancel()
			}
			return result, nil
		})
		seconds := []int{0, 0, 0, 1, 3, 3}
		want := make([]time.Duration, len(seconds))
		for i, second := range seconds {
			want[i] = time.Duration(second) * time.Second
		}
		if !slices.Equal(observed, want) {
			t.Fatalf("조회 시각=%v, want=%v", observed, want)
		}
	})
}

func TestConsumerCancellationBoundaries(t *testing.T) {
	for _, mode := range []string{"before_start", "idle", "processing"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "before_start" {
					cancel()
				}
				worker := &Worker{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				calls := 0
				done := make(chan struct{})
				go func() {
					defer close(done)
					worker.consume(ctx, func(ctx context.Context) (bool, error) {
						calls++
						if mode == "processing" {
							<-ctx.Done()
						}
						return mode == "processing", ctx.Err()
					})
				}()
				synctest.Wait()
				cancel()
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("취소 뒤 소비 루프가 남았다")
				}
				want := 1
				if mode == "before_start" {
					want = 0
				}
				if calls != want {
					t.Fatalf("취소 경계의 회차 수=%d want=%d", calls, want)
				}
			})
		})
	}
}

func TestConsumerConcurrentStartClose(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 32 {
		worker := testWorker(t, nil)
		var calls sync.WaitGroup
		for range 16 {
			calls.Go(func() { worker.Start(ctx) })
			calls.Go(func() {
				if err := worker.Close(t.Context()); err != nil {
					t.Error(err)
				}
			})
		}
		calls.Wait()
		select {
		case <-worker.done:
		default:
			t.Fatal("시작·종료 경합 뒤 작업자가 남았다")
		}
	}
}
