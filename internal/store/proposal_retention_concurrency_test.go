package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type proposalCleanupResult struct {
	count int
	err   error
}

func expiredProposalFixture(t *testing.T, database *Store, kind model.RelationType) (model.Relation, model.Relation) {
	t.Helper()
	graph, from, to := relationTestEvents(t, database)
	request := confirmable(t, graph, kind, from.ID, to.ID)
	candidate := proposedRelation(graph, kind, from.ID, to.ID)
	candidate.ID = request.ID
	candidate.ProposedAt = time.Now().UTC().AddDate(0, 0, -91)
	candidate, err := database.CreateRelation(t.Context(), graph, candidate)
	if err != nil {
		t.Fatal(err)
	}
	return candidate, request
}

// 실제 DB의 차단 세션과 SQL로 그래프 행 잠금 대기를 확인한다.
func waitProposalGraphLock(t *testing.T, ctx context.Context, database *Store, pid uint32, done <-chan proposalCleanupResult) {
	t.Helper()
	for {
		select {
		case result := <-done:
			t.Fatalf("그래프 잠금 대기 없이 종료: %+v", result)
		default:
		}
		var query string
		err := database.pool.QueryRow(ctx, `SELECT query FROM pg_stat_activity WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid)) LIMIT 1`, int32(pid)).Scan(&query)
		if err == nil {
			if !strings.Contains(query, "public.context_graph") || !strings.Contains(query, "FOR UPDATE") {
				t.Fatalf("그래프 행 대신 다른 잠금 대기: %s", query)
			}
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestProposalCleanupWaitsForGraphLockIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	candidate, _ := expiredProposalFixture(t, database, model.RelationTypePrecedes)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	holder, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), holder)
	if err := requireActiveGraph(ctx, holder, candidate.GraphID); err != nil {
		t.Fatal(err)
	}
	done := make(chan proposalCleanupResult, 1)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	workers.Go(func() {
		n, e := database.cleanupGraphProposals(ctx, candidate.GraphID, time.Now().UTC())
		done <- proposalCleanupResult{n, e}
	})
	waitProposalGraphLock(t, ctx, database, holder.Conn().PgConn().PID(), done)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || result.count != 1 {
		t.Fatalf("잠금 해제 뒤 정리=%+v", result)
	}
}

func TestAGERawProposalDeleteAfterConfirmIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	var server, ageVersion string
	if err := database.pool.QueryRow(t.Context(), `SELECT current_setting('server_version'), extversion FROM pg_extension WHERE extname='age'`).Scan(&server, &ageVersion); err != nil {
		t.Fatal(err)
	}
	t.Logf("적용 PostgreSQL=%s AGE=%s", server, ageVersion)

	candidate, request := expiredProposalFixture(t, database, model.RelationTypePrecedes)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), first)
	// 그래프 잠금을 의도적으로 우회해 이슈의 미확인 AGE 갱신/삭제 순서를 관찰한다.
	request.ID = candidate.ID
	if _, err := database.updateRelation(ctx, first, candidate.GraphID, request); err != nil {
		t.Fatal(err)
	}
	second, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), second)
	done := make(chan proposalCleanupResult, 1)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	workers.Go(func() {
		removed, e := database.deleteRelationProposal(ctx, second, candidate.GraphID, candidate)
		n := 0
		if removed {
			n = 1
		}
		done <- proposalCleanupResult{n, e}
	})
	waitLifeBlock(t, ctx, database, int(first.Conn().PgConn().PID()))
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-done
	code := ""
	if pgErr, ok := errors.AsType[*pgconn.PgError](result.err); ok {
		code = pgErr.Code
	}
	t.Logf("AGE 직접 확정 UPDATE 선행/후보 DELETE 후행: removed=%d sqlstate=%s error=%v", result.count, code, result.err)
	if result.err != nil {
		if err := second.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	} else if err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := database.relationByID(ctx, database.pool, candidate.GraphID, candidate.ID)
	t.Logf("AGE 직접 경합 최종 상태=%s error=%v", stored.State, err)
	if code != "XX000" || result.count != 0 || err != nil || stored.State != model.RelationStateConfirmed {
		t.Fatalf("고정 AGE 판의 직접 경합 결과가 달라짐: %+v 상태=%s err=%v", result, stored.State, err)
	}
}

func TestProposalCleanupSerializesJudgmentIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	for _, kind := range []model.RelationType{model.RelationTypePrecedes, model.RelationTypeRelatesTo} {
		for _, action := range []string{"confirm", "discard"} {
			for _, cleanupFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/cleanupFirst=%t", kind, action, cleanupFirst), func(t *testing.T) {
					candidate, request := expiredProposalFixture(t, database, kind)
					judge := func(ctx context.Context) (int, error) {
						if action == "confirm" {
							_, err := database.ConfirmRelation(ctx, candidate.GraphID, request, nil)
							return 0, err
						}
						_, err := database.DiscardRelation(ctx, candidate.GraphID, candidate.ID, nil)
						return 0, err
					}
					cleanup := func(ctx context.Context) (int, error) {
						return database.cleanupGraphProposals(ctx, candidate.GraphID, time.Now().UTC().AddDate(0, 0, -90))
					}
					leading, trailing := judge, cleanup
					if cleanupFirst {
						leading, trailing = cleanup, judge
					}
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					holder, err := database.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer rollbackTransaction(t.Context(), holder)
					count, err := leading(context.WithValue(ctx, writeTransactionContextKey{}, holder))
					if err != nil {
						t.Fatal(err)
					}
					if cleanupFirst && count != 1 {
						t.Fatalf("선행 정리=%d", count)
					}
					done := make(chan proposalCleanupResult, 1)
					var workers sync.WaitGroup
					defer func() { cancel(); workers.Wait() }()
					workers.Go(func() { n, e := trailing(ctx); done <- proposalCleanupResult{n, e} })
					waitProposalGraphLock(t, ctx, database, holder.Conn().PgConn().PID(), done)
					if err := holder.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					result := <-done
					if cleanupFirst && action == "discard" {
						if !errors.Is(result.err, ErrNotFound) {
							t.Fatalf("정리 뒤 폐기 오류=%v", result.err)
						}
					} else if result.err != nil {
						t.Fatalf("후행 결과=%+v", result)
					}
					if !cleanupFirst && result.count != 0 {
						t.Fatalf("판단된 관계 삭제=%d", result.count)
					}
					stored, err := database.relationByID(ctx, database.pool, candidate.GraphID, candidate.ID)
					if cleanupFirst && action == "discard" {
						if !errors.Is(err, ErrNotFound) {
							t.Fatalf("정리한 후보가 남음: %v", err)
						}
					} else {
						want := model.RelationStateConfirmed
						if action == "discard" {
							want = model.RelationStateDiscarded
						}
						if err != nil || stored.State != want {
							t.Fatalf("최종 상태=%s want=%s err=%v", stored.State, want, err)
						}
					}
				})
			}
		}
	}
}

func TestProposalCleanupRechecksRetentionIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	candidate, _ := expiredProposalFixture(t, database, model.RelationTypePrecedes)
	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	holder, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), holder)
	if err := requireActiveGraph(ctx, holder, candidate.GraphID); err != nil {
		t.Fatal(err)
	}
	done := make(chan proposalCleanupResult, 1)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	workers.Go(func() {
		n, e := database.cleanupGraphProposals(ctx, candidate.GraphID, cutoff)
		done <- proposalCleanupResult{n, e}
	})
	waitProposalGraphLock(t, ctx, database, holder.Conn().PgConn().PID(), done)
	// 목록을 읽고 실제 잠금 대기에 들어간 뒤 새 제안 시각을 커밋한다.
	candidate.ProposedAt = time.Now().UTC()
	if _, err := database.updateRelation(ctx, holder, candidate.GraphID, candidate); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || result.count != 0 {
		t.Fatalf("新しい提案時刻を無視した: %+v", result)
	}
	stored, err := database.relationByID(ctx, database.pool, candidate.GraphID, candidate.ID)
	if err != nil || !stored.ProposedAt.Equal(candidate.ProposedAt) {
		t.Fatalf("更新した候補が失われた: %+v %v", stored, err)
	}
}

func TestProposalCleanupCancelGraphWaitIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	candidate, _ := expiredProposalFixture(t, database, model.RelationTypePrecedes)
	holder, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), holder)
	if err := requireActiveGraph(t.Context(), holder, candidate.GraphID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan proposalCleanupResult, 1)
	var workers sync.WaitGroup
	defer func() { cancel(context.Canceled); workers.Wait() }()
	workers.Go(func() {
		n, e := database.cleanupGraphProposals(ctx, candidate.GraphID, time.Now().UTC())
		done <- proposalCleanupResult{n, e}
	})
	waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	waitProposalGraphLock(t, waitCtx, database, holder.Conn().PgConn().PID(), done)
	cause := errors.New("정리 잠금 대기 취소")
	cancel(cause)
	result := <-done
	if result.count != 0 || !errors.Is(result.err, context.Canceled) || !errors.Is(result.err, cause) {
		t.Fatalf("취소 결과=%+v", result)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertConnectionRecovery(t, database, nil, 0)
	stored, err := database.relationByID(t.Context(), database.pool, candidate.GraphID, candidate.ID)
	if err != nil || stored.State != model.RelationStateProposed {
		t.Fatalf("취소된 정리가 후보를 삭제함: %v", err)
	}
}

func TestProposalCleanupDeletedGraphAndUpdateMissingIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	candidate, request := expiredProposalFixture(t, database, model.RelationTypePrecedes)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at=now() WHERE graph_id=$1`, candidate.GraphID.String()); err != nil {
		t.Fatal(err)
	}
	if n, err := database.cleanupGraphProposals(t.Context(), candidate.GraphID, time.Now().UTC()); err != nil || n != 1 {
		t.Fatalf("삭제 그래프의 후보 정리=%d %v", n, err)
	}
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), tx)
	if _, err := database.updateRelation(t.Context(), tx, candidate.GraphID, request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("갱신 0행 오류=%v", err)
	}
}

type proposalDeleteMarker struct{}
type cancelProposalDeleteTracer struct {
	cancel context.CancelCauseFunc
	cause  error
}

func (tracer cancelProposalDeleteTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "DELETE edge RETURN 1") {
		return context.WithValue(ctx, proposalDeleteMarker{}, true)
	}
	return ctx
}
func (tracer cancelProposalDeleteTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(proposalDeleteMarker{}) == true {
		tracer.cancel(tracer.cause)
	}
}

func TestProposalCleanupDeleteRollbackIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	candidate, _ := expiredProposalFixture(t, database, model.RelationTypePrecedes)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause := errors.New("후보 삭제 뒤 커밋 전 취소")
	configureTierPool(t, database, cancelProposalDeleteTracer{cancel: cancel, cause: cause})
	t.Cleanup(database.Close)
	count, err := database.cleanupGraphProposals(ctx, candidate.GraphID, time.Now().UTC())
	if count != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("취소된 삭제 집계=%d err=%v", count, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
	stored, err := database.relationByID(t.Context(), database.pool, candidate.GraphID, candidate.ID)
	if err != nil || stored.State != model.RelationStateProposed {
		t.Fatalf("롤백된 후보가 사라짐: %v", err)
	}
}
