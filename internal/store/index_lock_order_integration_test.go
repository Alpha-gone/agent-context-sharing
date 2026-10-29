package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// 그래프를 잠근 쓰기를 결과 저장이 기다리는 동안 작업 재등록까지 진행한다.
// 결과 저장이 작업 행을 먼저 잠그면 여기서 실제 DB 교착이 발생한다.
func TestIndexResultSerializesWithContextWritesIntegration(t *testing.T) {
	for _, sample := range []struct {
		layer   model.Layer
		restore bool
	}{
		{model.LayerEvent, false}, {model.LayerDerived, false},
		{model.LayerSource, true}, {model.LayerDerived, true}, {model.LayerEvent, true},
	} {
		t.Run(fmt.Sprintf("%s/restore=%t", sample.layer, sample.restore), func(t *testing.T) {
			database := newIsolatedIntegrationStore(t)
			graph, actor, source := boundaryGraph(t, database)
			target := source
			var err error
			if sample.layer == model.LayerEvent {
				target, err = database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
			} else if sample.layer == model.LayerDerived {
				target, err = database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
			}
			if err != nil {
				t.Fatal(err)
			}
			readyIndexTasks(t, database, graph, target.ID)
			task, claim, found, err := database.claimIndexTask(t.Context(), graph)
			if err != nil || !found || task.ContextID != target.ID {
				t.Fatalf("색인 확보: %+v/%t/%v", task, found, err)
			}
			if sample.restore {
				target, err = database.DiscardContext(t.Context(), graph, target.ID, registrationOperation(target, OperationDiscard), WriteLimits{})
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			first, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(t.Context())
			if _, err := lockGraphs(ctx, first, []model.ID{graph}); err != nil {
				t.Fatal(err)
			}
			var revision int64
			if err := first.QueryRow(ctx, `SELECT content_revision FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			vector := make([]float64, embeddingDimension(t, database))
			vector[0] = 1
			result := make(chan error, 1)
			var workers sync.WaitGroup
			defer func() { cancel(); workers.Wait() }()
			workers.Go(func() {
				stored, err := database.storeIndexResult(ctx, task, claim, IndexTaskResult{Embedding: vector, ModelID: "lock-order"})
				if err == nil && stored {
					err = fmt.Errorf("재등록 전 결과가 공개됐다")
				}
				result <- err
			})
			waitIndexGraphBlocker(t, ctx, database, first.Conn().PgConn().PID(), result)
			writeCtx := context.WithValue(ctx, writeTransactionContextKey{}, first)
			if sample.restore {
				_, err = database.RestoreContext(writeCtx, graph, target.ID, registrationOperation(target, OperationUpdate), WriteLimits{})
			} else {
				next := target
				next.Body, next.Version = "잠금 경합 뒤 갱신한 본문", target.Version+1
				_, err = database.UpdateContext(writeCtx, graph, target.Version, next)
			}
			if err != nil {
				t.Fatalf("결과 저장 대기 중 노드 쓰기: %v", err)
			}
			if err := first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatalf("재등록 뒤 이전 확보분: %v", err)
			}
			var current int64
			if err := database.pool.QueryRow(ctx, `SELECT content_revision FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&current); err != nil || current != revision+1 {
				t.Fatalf("내용 판=%d, want %d: %v", current, revision+1, err)
			}
			if count := indexTaskCount(t, database, target.ID); count != 1 {
				t.Fatalf("재등록 작업 수=%d, want 1", count)
			}
			var embeddings int
			if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM public.context_embedding WHERE context_id=$1`, target.ID.String()).Scan(&embeddings); err != nil || embeddings != 0 {
				t.Fatalf("이전 확보분의 임베딩 수=%d: %v", embeddings, err)
			}
			readyIndexTasks(t, database, graph, target.ID)
			processed, err := database.ProcessNextIndexTaskInGraph(ctx, graph, func(context.Context, IndexTask) IndexTaskResult {
				return IndexTaskResult{Embedding: vector, ModelID: "lock-order"}
			})
			if err != nil || !processed.Succeeded || indexTaskCount(t, database, target.ID) != 0 {
				t.Fatalf("새 작업 완료: %+v/%v", processed, err)
			}
		})
	}
}

func TestReindexLocksGraphBeforeTasksIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, _, source := boundaryGraph(t, database)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(t.Context())
	if _, err := lockGraphs(ctx, first, []model.ID{graph}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	workers.Go(func() { result <- database.ReindexGraph(ctx, graph) })
	waitIndexGraphBlocker(t, ctx, database, first.Conn().PgConn().PID(), result)
	// 그래프 대기 중에는 작업 행을 선점하지 않아야 한다.
	if _, err := first.Exec(ctx, `SELECT task_id FROM public.index_task WHERE context_id=$1 FOR UPDATE NOWAIT`, source.ID.String()); err != nil {
		t.Fatalf("재색인의 작업 행 선점: %v", err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if count := indexTaskCount(t, database, source.ID); count != 1 {
		t.Fatalf("재색인 작업 수=%d, want 1", count)
	}
}

func waitIndexGraphBlocker(t *testing.T, ctx context.Context, database *Store, holder uint32, result <-chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			t.Fatalf("그래프 잠금 해제 전에 색인 쓰기가 끝났다: %v", err)
		case <-ctx.Done():
			t.Fatalf("색인 쓰기의 그래프 대기 확인: %v", ctx.Err())
		case <-ticker.C:
			var blocked bool
			if err := database.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE datname=current_database() AND $1::integer = ANY(pg_blocking_pids(pid)))`, holder).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				return
			}
		}
	}
}
