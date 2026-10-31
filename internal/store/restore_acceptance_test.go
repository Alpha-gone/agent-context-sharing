package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type ops06RestoreFixture struct {
	graph, creator, former, viewer, operator, team model.ID
}

func ops06DeletedGraph(t *testing.T, database *Store) ops06RestoreFixture {
	t.Helper()
	f := ops06RestoreFixture{creator: newTestID(t), former: newTestID(t), viewer: newTestID(t), operator: newTestID(t), team: newTestID(t)}
	for _, id := range []model.ID{f.creator, f.former, f.viewer, f.operator} {
		createTestAccount(t, database, id)
	}
	f.graph = createTestGraph(t, database, f.creator)
	createTestTeam(t, database, f.team, f.creator, false)
	addTestTeamMember(t, database, f.team, f.creator)
	grantTeam(t, database, f.graph, f.team, model.GraphGradeOwner)
	for id, grade := range map[model.ID]model.GraphGrade{f.former: model.GraphGradeOwner, f.viewer: model.GraphGradeViewer} {
		if err := database.GrantGraph(t.Context(), f.graph, f.creator, id, GrantSubjectAccount, grade); err != nil {
			t.Fatal(err)
		}
		if err := database.RevokeGraphGrantWithAudit(t.Context(), f.graph, f.creator, id, GrantSubjectAccount); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.RemoveTeamMemberWithAudit(t.Context(), f.team, f.creator, f.creator); err != nil {
		t.Fatal(err)
	}
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_expires_at=clock_timestamp() WHERE graph_id=$1`, f.graph.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExpireGrace(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOPS06RequestsSelectionAndRetentionIntegration(t *testing.T) {
	database, _, _ := newAgentContractDatabase(t)
	for _, mode := range []string{"time", "audit_id", "requester_is_operator"} {
		t.Run(mode, func(t *testing.T) {
			f := ops06DeletedGraph(t, database)
			for _, actor := range []model.ID{f.viewer, f.operator} {
				before := planStateFingerprint(t, database, f.graph)
				if err := database.RequestGraphRestore(t.Context(), f.graph, actor); !errors.Is(err, ErrNotFound) {
					t.Fatalf("소유자 이력 없는 계정의 요청: %v", err)
				}
				if before != planStateFingerprint(t, database, f.graph) {
					t.Fatal("거부한 요청이 상태를 변경했다")
				}
			}
			for _, actor := range []model.ID{f.creator, f.former} {
				eligible, err := database.ListRestoreEligibleGraphs(t.Context(), actor)
				if err != nil || !containsGraph(eligible, f.graph) {
					t.Fatalf("복구 가능 목록: %v", err)
				}
				if err := database.RequestGraphRestore(t.Context(), f.graph, actor); err != nil {
					t.Fatal(err)
				}
			}
			// 같은 시각에는 나중 요청의 작은 ID가 먼저 선택되어야 한다.
			at := time.Now().UTC().Add(-48 * time.Hour)
			for i, actor := range []model.ID{f.creator, f.former} {
				requestedAt := at
				if mode == "time" && i == 0 {
					requestedAt = at.Add(-time.Second)
				}
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.web_audit_log SET audit_id=$3,occurred_at=$4 WHERE graph_id=$1 AND target_kind='restore_request' AND requester_account_id=$2`, f.graph.String(), actor.String(), obs06ID(t, 100-i).String(), requestedAt); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Add(24 * time.Hour)
			days := func(model.ID) int { return 1 }
			if _, err := database.CleanupAuditRecords(t.Context(), now, days, days); err != nil {
				t.Fatal(err)
			}
			pending, err := database.PendingRestoreRequests(t.Context())
			selected := f.former
			if mode == "time" {
				selected = f.creator
			}
			if err != nil || len(pending) != 2 || pending[0].RequestedBy != selected {
				t.Fatalf("가장 오래된 미처리 요청: %+v / %v", pending, err)
			}
			operator := f.operator
			if mode == "requester_is_operator" {
				operator = selected
			}
			if err := database.OperatorRestoreGraph(t.Context(), f.graph, operator); err != nil {
				t.Fatal(err)
			}
			for _, actor := range []model.ID{f.creator, f.former, f.operator} {
				grade, found, err := database.EffectiveGrade(t.Context(), f.graph, actor)
				if err != nil || found != (actor == selected) || (found && grade != model.GraphGradeOwner) {
					t.Fatalf("복구 등급: %s / %t / %v", grade, found, err)
				}
			}
			var actor, requester string
			if err := database.pool.QueryRow(t.Context(), `SELECT actor_account_id, requester_account_id FROM public.web_audit_log WHERE graph_id=$1 AND target_kind='operator_restore'`, f.graph.String()).Scan(&actor, &requester); err != nil || actor != operator.String() || requester != selected.String() {
				t.Fatalf("처리자/요청자 감사: %s / %s / %v", actor, requester, err)
			}
			// 다시 삭제되어도 처리 완료 요청은 정리 전후 모두 대기로 돌아오지 않는다.
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at=$2,grace_started_at=$2 WHERE graph_id=$1`, f.graph.String(), now); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				pending, err := database.PendingRestoreRequests(t.Context())
				if err != nil || slices.ContainsFunc(pending, func(r model.RestoreRequest) bool { return r.GraphID == f.graph }) {
					t.Fatalf("처리 완료 요청 재등장: %+v / %v", pending, err)
				}
				if err := database.OperatorRestoreGraph(t.Context(), f.graph, f.operator); !errors.Is(err, ErrNotFound) {
					t.Fatal("처리한 요청 재사용", err)
				}
				if _, err := database.CleanupAuditRecords(t.Context(), now.AddDate(0, 0, 30), days, days); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestOPS06StorageFailureRollsBackIntegration(t *testing.T) {
	database, _, _ := newAgentContractDatabase(t)
	for _, stage := range []string{"request_audit", "grade", "restore_audit"} {
		t.Run(stage, func(t *testing.T) {
			f := ops06DeletedGraph(t, database)
			if stage != "request_audit" {
				if err := database.RequestGraphRestore(t.Context(), f.graph, f.former); err != nil {
					t.Fatal(err)
				}
			}
			table, predicate := "public.web_audit_log", "target_kind='restore_request'"
			if stage == "grade" {
				table, predicate = "public.graph_grant", "grade='owner' AND subject_type='account'"
			} else if stage == "restore_audit" {
				predicate = "target_kind='operator_restore'"
			}
			// 제약은 이 시험이 생성한 DB·그래프에만 적용한다.
			query := fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT ops06_failure CHECK (NOT (graph_id='%s' AND %s)) NOT VALID`, table, f.graph.String(), predicate)
			if _, err := database.pool.Exec(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := database.pool.Exec(t.Context(), "ALTER TABLE "+table+" DROP CONSTRAINT IF EXISTS ops06_failure"); err != nil {
					t.Error(err)
				}
			}()
			before := planStateFingerprint(t, database, f.graph)
			var err error
			if stage == "request_audit" {
				err = database.RequestGraphRestore(t.Context(), f.graph, f.creator)
			} else {
				err = database.OperatorRestoreGraph(t.Context(), f.graph, f.operator)
			}
			failure, ok := errors.AsType[*pgconn.PgError](err)
			if !ok || failure.Code != "23514" || before != planStateFingerprint(t, database, f.graph) {
				t.Fatalf("저장 실패·원자성: %v", err)
			}
			var grants int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.graph_grant WHERE graph_id=$1 AND subject_type='account'`, f.graph.String()).Scan(&grants); err != nil || grants != 0 {
				t.Fatalf("실패한 직접 등급이 남았다: %d / %v", grants, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
			if _, err := database.pool.Exec(t.Context(), "ALTER TABLE "+table+" DROP CONSTRAINT ops06_failure"); err != nil {
				t.Fatal(err)
			}
			if stage == "request_audit" {
				if err := database.RequestGraphRestore(t.Context(), f.graph, f.creator); err != nil {
					t.Fatal("요청 감사 실패 뒤 재시도", err)
				}
			}
			if err := database.OperatorRestoreGraph(t.Context(), f.graph, f.operator); err != nil {
				t.Fatal("저장 실패 뒤 복구 재시도", err)
			}
		})
	}
}

func TestOPS06LockWaitRechecksAndCancelsIntegration(t *testing.T) {
	database, peer, _ := newAgentContractDatabase(t)
	for _, mode := range []string{"request_state", "restore_state", "request_cancel", "restore_cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := ops06DeletedGraph(t, database)
			if strings.HasPrefix(mode, "restore") {
				if err := database.RequestGraphRestore(t.Context(), f.graph, f.creator); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			tx, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(ctx, tx)
			if _, err := lockGraphs(ctx, tx, []model.ID{f.graph}); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("restore request canceled")
			requestCtx, cancelRequest := context.WithCancelCause(ctx)
			defer cancelRequest(nil)
			results := make(chan error, 1)
			var group sync.WaitGroup
			group.Go(func() {
				if strings.HasPrefix(mode, "request") {
					results <- peer.RequestGraphRestore(requestCtx, f.graph, f.creator)
				} else {
					results <- peer.OperatorRestoreGraph(requestCtx, f.graph, f.operator)
				}
			})
			defer func() { cancelRequest(nil); _ = rollbackTransaction(ctx, tx); group.Wait() }()
			waitLifeBlock(t, ctx, database, pid)
			if strings.HasSuffix(mode, "cancel") {
				cancelRequest(cause)
			} else {
				if _, err := tx.Exec(ctx, `UPDATE public.context_graph SET grace_started_at=NULL,grace_expires_at=NULL WHERE graph_id=$1`, f.graph.String()); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			err = <-results
			if strings.HasSuffix(mode, "cancel") {
				if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
					t.Fatal("호출자 취소 원인 유실", err)
				}
			} else if !errors.Is(err, ErrNotFound) {
				t.Fatal("잠금 뒤 직접 삭제 상태를 재확인하지 않았다", err)
			}
			assertConnectionRecovery(t, peer, nil, 0)
		})
	}
}

type ops06RollbackFailureTx struct {
	pgx.Tx
	cancel                           context.CancelCauseFunc
	writeCause, cancelCause, failure error
	cleanup                          *bool
}

func (tx *ops06RollbackFailureTx) Begin(ctx context.Context) (pgx.Tx, error) {
	child, err := tx.Tx.Begin(ctx)
	return &ops06RollbackFailureTx{Tx: child, cancel: tx.cancel, writeCause: tx.writeCause, cancelCause: tx.cancelCause, failure: tx.failure, cleanup: tx.cleanup}, err
}

func (tx *ops06RollbackFailureTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO public.web_audit_log") {
		tx.cancel(tx.cancelCause)
		return pgconn.CommandTag{}, tx.writeCause
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *ops06RollbackFailureTx) Rollback(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	*tx.cleanup = ctx.Err() == nil && ok && time.Until(deadline) > 0 && time.Until(deadline) <= 5*time.Second
	return errors.Join(tx.Tx.Rollback(ctx), tx.failure)
}

func TestOPS06RollbackPreservesAllCausesIntegration(t *testing.T) {
	database, _, _ := newAgentContractDatabase(t)
	f := ops06DeletedGraph(t, database)
	if err := database.RequestGraphRestore(t.Context(), f.graph, f.creator); err != nil {
		t.Fatal(err)
	}
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), tx)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	writeCause, cancelCause, rollbackCause := errors.New("audit failed"), errors.New("caller stopped"), errors.New("rollback report failed")
	cleanup := false
	wrapped := &ops06RollbackFailureTx{Tx: tx, cancel: cancel, writeCause: writeCause, cancelCause: cancelCause, failure: rollbackCause, cleanup: &cleanup}
	err = database.OperatorRestoreGraph(context.WithValue(ctx, writeTransactionContextKey{}, pgx.Tx(wrapped)), f.graph, f.operator)
	for _, cause := range []error{writeCause, cancelCause, context.Canceled, rollbackCause} {
		if !errors.Is(err, cause) {
			t.Fatalf("원인 유실: %v / %v", cause, err)
		}
	}
	if !cleanup {
		t.Fatal("취소와 독립된 유한 롤백 예산을 사용하지 않았다")
	}
	var deleted bool
	var grants int
	if err := tx.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL FROM public.context_graph WHERE graph_id=$1`, f.graph.String()).Scan(&deleted); err != nil || !deleted {
		t.Fatal("저장점의 그래프 복구가 롤백되지 않았다", err)
	}
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM public.graph_grant WHERE graph_id=$1 AND subject_type='account'`, f.graph.String()).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("저장점의 등급이 롤백되지 않았다", err)
	}
}

func TestOPS06ConcurrentInstancesIntegration(t *testing.T) {
	database, peer, _ := newAgentContractDatabase(t)
	f := ops06DeletedGraph(t, database)
	if err := database.RequestGraphRestore(t.Context(), f.graph, f.creator); err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 8)
	var group sync.WaitGroup
	for i := range 8 {
		backend := database
		if i%2 != 0 {
			backend = peer
		}
		group.Go(func() {
			<-start
			results <- backend.OperatorRestoreGraph(t.Context(), f.graph, f.operator)
		})
	}
	close(start)
	group.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrNotFound) {
			t.Fatal("동시 복구 결과", err)
		}
	}
	var count int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log WHERE graph_id=$1 AND target_kind='operator_restore'`, f.graph.String()).Scan(&count); err != nil || count != 1 || succeeded != 1 {
		t.Fatalf("복구 성공·감사 횟수: %d / %d / %v", succeeded, count, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
	assertConnectionRecovery(t, peer, nil, 0)
}

func TestOPS06CancellationBeforeBeginIntegration(t *testing.T) {
	database, _, _ := newAgentContractDatabase(t)
	f := ops06DeletedGraph(t, database)
	cause := errors.New("canceled before transaction")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	before := planStateFingerprint(t, database, f.graph)
	for _, call := range []func() error{
		func() error { return database.RequestGraphRestore(ctx, f.graph, f.creator) },
		func() error { return database.OperatorRestoreGraph(ctx, f.graph, f.operator) },
	} {
		if err := call(); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatal("트랜잭션 시작 전 취소 원인 유실", err)
		}
		if before != planStateFingerprint(t, database, f.graph) {
			t.Fatal("미리 취소한 복구가 상태를 변경했다")
		}
		assertConnectionRecovery(t, database, nil, 0)
	}
}
