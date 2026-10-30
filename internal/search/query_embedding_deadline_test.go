package search

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type queryDeadlineObservation struct {
	deadline  time.Time
	remaining time.Duration
}

type waitingQueryEmbedder struct{ entered chan queryDeadlineObservation }

func (e waitingQueryEmbedder) ModelID() string { return "query-deadline" }
func (e waitingQueryEmbedder) Embed(ctx context.Context, _ string) ([]float64, error) {
	deadline, _ := ctx.Deadline()
	e.entered <- queryDeadlineObservation{deadline, time.Until(deadline)}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestQueryEmbeddingDeadlineKeepsOtherChannels(t *testing.T) {
	for _, consistency := range []Consistency{"", ConsistencySnapshot} {
		for _, execution := range []Execution{ExecutionParallel, ExecutionSequential} {
			t.Run(string(consistency)+"/"+string(execution), func(t *testing.T) {
				t.Parallel()
				graph := testID(t, "019a0000-0000-7000-8000-000000000011")
				value := testContext(t, graph, "019a0000-0000-7000-8000-000000000012", "남은 채널 후보", 1)
				database := &snapshotBoundaryStore{Store: &fakeStore{keyword: []model.SearchCandidate{{Context: value}}, time: []model.SearchCandidate{{Context: value}}}}
				embedder := waitingQueryEmbedder{entered: make(chan queryDeadlineObservation, 1)}
				service, err := New(database, embedder, Config{Execution: execution, CandidateLimit: 5, FoldThreshold: .9, Consistency: consistency}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
				defer cancel()
				flow, err := service.Flow(ctx, Input{GraphID: graph, WorkContext: "작업", Budget: 100})
				if err != nil || ctx.Err() != nil || len(flow.Contexts) != 1 || flow.Contexts[0].Value.ID != value.ID {
					t.Fatalf("임베딩 지연 뒤 부분 응답: %+v/%v, 요청=%v", flow, err, ctx.Err())
				}
				if flow.Channels["semantic"].Failure != "embedding_unavailable" || flow.Channels["keyword"].Failure != "" || flow.Channels["time"].Failure != "" || flow.Channels["keyword"].Contribution != 1 || flow.Channels["time"].Contribution != 1 {
					t.Fatalf("채널 부분 상태=%+v", flow.Channels)
				}
				observed := <-embedder.entered
				parent, _ := ctx.Deadline()
				if observed.remaining < 4*time.Second || observed.remaining > 5*time.Second || !observed.deadline.Before(parent) {
					t.Fatalf("질의 임베딩 전용 5초 마감: %+v, 요청 마감=%v", observed, parent)
				}
				if consistency == ConsistencySnapshot && (database.opened != 1 || database.closed != 1 || database.readContext.Err() != nil) {
					t.Fatal("임베딩의 만료 context가 스냅숏 읽기로 전파됐다")
				}
			})
		}
	}
}

func TestQueryEmbeddingCallerCancellationRemainsFatal(t *testing.T) {
	for _, consistency := range []Consistency{"", ConsistencySnapshot} {
		t.Run(string(consistency), func(t *testing.T) {
			graph := testID(t, "019a0000-0000-7000-8000-000000000011")
			value := testContext(t, graph, "019a0000-0000-7000-8000-000000000012", "반환하면 안 되는 본문", 1)
			database := &snapshotBoundaryStore{Store: &fakeStore{time: []model.SearchCandidate{{Context: value}}}}
			embedder := waitingQueryEmbedder{entered: make(chan queryDeadlineObservation, 1)}
			service, err := New(database, embedder, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: .9, Consistency: consistency}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := errors.New("caller cancellation")
			var group sync.WaitGroup
			defer func() { cancel(cause); group.Wait() }()
			result := make(chan struct {
				flow Flow
				err  error
			}, 1)
			group.Go(func() {
				flow, err := service.Flow(ctx, Input{GraphID: graph, WorkContext: "작업", Budget: 100})
				result <- struct {
					flow Flow
					err  error
				}{flow, err}
			})
			select {
			case <-embedder.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("질의 임베딩 호출 없음")
			}
			cancel(cause)
			got := <-result
			if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, cause) || len(got.flow.Contexts) != 0 {
				t.Fatalf("호출자 취소가 부분 성공으로 바뀌었다: %+v/%v", got.flow, got.err)
			}
		})
	}
}

func TestQueryEmbeddingEarlierCallerDeadlineRemainsFatal(t *testing.T) {
	for _, consistency := range []Consistency{"", ConsistencySnapshot} {
		t.Run(string(consistency), func(t *testing.T) {
			database := &snapshotBoundaryStore{Store: &fakeStore{}}
			embedder := waitingQueryEmbedder{entered: make(chan queryDeadlineObservation, 1)}
			service, err := New(database, embedder, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: .9, Consistency: consistency}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			graph := testID(t, "019a0000-0000-7000-8000-000000000011")
			flow, err := service.Flow(ctx, Input{GraphID: graph, WorkContext: "작업", Budget: 100})
			if !errors.Is(err, context.DeadlineExceeded) || len(flow.Contexts) != 0 {
				t.Fatalf("요청 마감이 부분 성공으로 바뀌었다: %+v/%v", flow, err)
			}
			observed := <-embedder.entered
			deadline, _ := ctx.Deadline()
			if !observed.deadline.Equal(deadline) {
				t.Fatal("질의 임베딩이 더 이른 호출자 마감을 연장했다")
			}
		})
	}
}
