package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestContextWritesReserveBeforeTransactionIntegration은 예약 대기 동안 쓰기 연결을
// 먼저 잡지 않는지 생성·갱신·폐기·복구의 공개 경계에서 확인한다.
func TestContextWritesReserveBeforeTransactionIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	value := testSourceContext(t, graphID, actorID, "https://example.test/reserve-"+graphID.String())
	release, err := database.reserveConnections(t.Context(), connectionBudget(4))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for name, run := range map[string]func(context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := database.CreateContext(ctx, graphID, value, nil)
			return err
		},
		"update": func(ctx context.Context) error { _, err := database.UpdateContext(ctx, graphID, 1, value); return err },
		"discard": func(ctx context.Context) error {
			_, err := database.DiscardContext(ctx, graphID, value.ID, nil, WriteLimits{})
			return err
		},
		"restore": func(ctx context.Context) error {
			_, err := database.RestoreContext(ctx, graphID, value.ID, nil, WriteLimits{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			before := database.pool.Stat().AcquireCount()
			if err := run(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("예약 대기 취소: %v", err)
			}
			if after := database.pool.Stat().AcquireCount(); after != before {
				t.Fatalf("예약 전에 연결을 획득했다: %d -> %d", before, after)
			}
		})
	}
	release()
	if _, err := database.CreateContext(t.Context(), graphID, value, nil); err != nil {
		t.Fatalf("예약 취소 뒤 생성: %v", err)
	}
}

// TestContextAGEConflictsDoNotStarveSmallPoolIntegration은 풀 밖의 선행 갱신으로
// 시작한 쓰기를 모두 옛 판에서 기다리게 한 뒤 커밋해 충돌 재조회 경로를 실행한다.
func TestContextAGEConflictsDoNotStarveSmallPoolIntegration(t *testing.T) {
	for _, action := range []string{"update", "discard", "restore"} {
		t.Run(action, func(t *testing.T) {
			database := newIntegrationStore(t)
			small := newSmallPoolStore(t)
			actorID := newTestID(t)
			createTestAccount(t, database, actorID)
			graphID := createTestGraph(t, database, actorID)
			source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/starve-"+graphID.String()), nil)
			if err != nil {
				t.Fatal(err)
			}
			target := source
			if action == "update" {
				target, err = database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
				if err != nil {
					t.Fatal(err)
				}
			}
			if action == "restore" {
				target, err = database.DiscardContext(t.Context(), graphID, target.ID, testMCPDiscardOperation(target), WriteLimits{})
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
			defer first.Rollback(context.Background())
			run := func(store *Store, ctx context.Context) error {
				var err error
				switch action {
				case "update":
					next := target
					next.Body, next.Version = "경합 갱신 본문", target.Version+1
					_, err = store.UpdateContext(ctx, graphID, target.Version, next)
				case "discard":
					_, err = store.DiscardContext(ctx, graphID, target.ID, nil, WriteLimits{})
				case "restore":
					_, err = store.RestoreContext(ctx, graphID, target.ID, nil, WriteLimits{})
				}
				return err
			}
			if err := run(database, context.WithValue(ctx, writeTransactionContextKey{}, first)); err != nil {
				t.Fatal(err)
			}
			const requests = 8
			results := make(chan error, requests)
			var group sync.WaitGroup
			defer func() { cancel(); group.Wait() }()
			for range requests {
				group.Go(func() { results <- run(small, ctx) })
			}
			// 두 요청이 실제 AGE 행 잠금을 기다릴 때까지 관측한다. 예약하지 않는 구현은
			// 이 구간에서 풀 상한 네 연결을 모두 쓰기 트랜잭션에 내준다.
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked int
				// 동일 정점의 두 번째 대기는 선행 쓰기뿐 아니라 앞선 대기자의 tuple
				// 잠금을 기다릴 수도 있으므로 직접 차단 PID 하나로 제한하지 않는다.
				if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND cardinality(pg_blocking_pids(pid))>0`).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked >= 2 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if acquired := small.pool.Stat().AcquiredConns(); acquired > 2 {
				t.Fatalf("충돌 쓰기의 동시 첫 연결 = %d, want 2 이하", acquired)
			}
			if err := first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			group.Wait()
			close(results)
			for err := range results {
				if action == "update" {
					if _, ok := errors.AsType[VersionConflictError](err); !ok {
						t.Fatalf("판 충돌: %v", err)
					}
				} else if !errors.Is(err, ErrInvalidState) {
					t.Fatalf("상태 충돌: %v", err)
				}
			}
			if !small.reservations.TryAcquire(connectionBudget(4)) {
				t.Fatal("실패 뒤 예약이 남았다")
			}
			small.reservations.Release(connectionBudget(4))
		})
	}
}
