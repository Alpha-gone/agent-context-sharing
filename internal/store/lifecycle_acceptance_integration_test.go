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

var lifeWriteTools = []string{"graph_create", "graph_update", "node_create", "node_update", "node_discard", "node_restore", "relation_confirm", "relation_discard"}

type lifeAcceptanceWrite struct {
	graph model.ID
	actor model.ID
	run   func(context.Context) error
}

func lifeAcceptanceFixture(t *testing.T, database *Store, tool string) lifeAcceptanceWrite {
	t.Helper()
	if strings.HasPrefix(tool, "graph_") {
		actor := newTestID(t)
		createTestAccount(t, database, actor)
		graph := lifeNewGraph(t, actor)
		if tool == "graph_create" {
			return lifeAcceptanceWrite{graph.ID, actor, func(ctx context.Context) error {
				_, err := database.CreateGraphWithOwner(ctx, graph, WriteLimits{GraphsPerAccount: 1})
				return err
			}}
		}
		if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
			t.Fatal(err)
		}
		return lifeAcceptanceWrite{graph.ID, actor, func(ctx context.Context) error {
			_, err := database.UpdateGraph(ctx, graph.ID, graph.Version, "acceptance update", "description")
			return err
		}}
	}
	if strings.HasPrefix(tool, "relation_") {
		graph, from, to := relationTestEvents(t, database)
		relation, operation := lifeRelationRequest(t, from, to, model.RelationTypePrecedes)
		if tool == "relation_confirm" {
			return lifeAcceptanceWrite{graph, operation.AccountID, func(ctx context.Context) error {
				_, err := database.ConfirmRelation(ctx, graph, relation, operation)
				return err
			}}
		}
		stored, err := database.ConfirmRelation(t.Context(), graph, relation, operation)
		if err != nil {
			t.Fatal(err)
		}
		operation.Kind, operation.RelationID = OperationDiscard, stored.ID
		return lifeAcceptanceWrite{graph, operation.AccountID, func(ctx context.Context) error {
			_, err := database.DiscardRelation(ctx, graph, stored.ID, operation)
			return err
		}}
	}
	graph, actor, source := boundaryGraph(t, database)
	value := testDerivedContext(t, graph, actor)
	if tool == "node_create" {
		return lifeAcceptanceWrite{graph, actor, func(ctx context.Context) error {
			_, err := database.CreateContextWithOperation(ctx, graph, value, []model.ID{source.ID}, registrationOperation(value, OperationAdd), WriteLimits{ActorID: actor, WritesPerMinute: 1})
			return err
		}}
	}
	stored := lifeAuditCreate(t, database, value, []model.ID{source.ID})
	if tool == "node_update" {
		next := stored
		next.Version++
		next.Body += " acceptance update"
		return lifeAcceptanceWrite{graph, actor, func(ctx context.Context) error {
			_, err := database.UpdateContextWithOperation(ctx, graph, stored.Version, next, registrationOperation(stored, OperationUpdate), WriteLimits{ActorID: actor, WritesPerMinute: 1})
			return err
		}}
	}
	// 폐기의 직접 근거 무효 표시도 바깥 커밋/롤백에 포함한다.
	lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{stored.ID})
	if tool == "node_restore" {
		if _, err := database.DiscardContext(t.Context(), graph, stored.ID, registrationOperation(stored, OperationDiscard), WriteLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	return lifeAcceptanceWrite{graph, actor, func(ctx context.Context) error {
		var err error
		if tool == "node_discard" {
			_, err = database.DiscardContext(ctx, graph, stored.ID, registrationOperation(stored, OperationDiscard), WriteLimits{})
		} else {
			_, err = database.RestoreContext(ctx, graph, stored.ID, registrationOperation(stored, OperationUpdate), WriteLimits{})
		}
		return err
	}}
}

func lifeAcceptanceFingerprint(t *testing.T, database *Store) string {
	t.Helper()
	parts := []string{registrationStateFingerprint(t, database)}
	for _, table := range []string{"graph_grant", "request_rate", "idempotency_record"} {
		var fingerprint string
		if err := database.pool.QueryRow(t.Context(), `SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY row_to_json(t)::text),'')) FROM public.`+table+` t`).Scan(&fingerprint); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, fingerprint)
	}
	return strings.Join(parts, ":")
}

// 결과 저장과 바깥 커밋을 각각 실패시킨다. 새 예약 INSERT는 성공하게 두어 업무가
// 저장점을 해제한 뒤의 실패라는 것을 callback 실행 수로 직접 확인한다.
func lifeFailIdempotencyResult(t *testing.T, database *Store, actor model.ID, deferred bool) func() {
	t.Helper()
	name := pgx.Identifier{"life09_" + actor.String()}.Sanitize()
	function := pgx.Identifier{"public", "life09_" + actor.String()}.Sanitize()
	remove := sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if _, err := database.pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON public.idempotency_record; DROP FUNCTION IF EXISTS "+function+"()"); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(remove)
	if _, err := database.pool.Exec(t.Context(), "CREATE FUNCTION "+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.actor_account_id='`+actor.String()+`'::uuid THEN RAISE EXCEPTION 'synthetic result failure'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	trigger := "CREATE TRIGGER " + name + " BEFORE UPDATE ON public.idempotency_record"
	if deferred {
		trigger = "CREATE CONSTRAINT TRIGGER " + name + " AFTER UPDATE ON public.idempotency_record DEFERRABLE INITIALLY DEFERRED"
	}
	if _, err := database.pool.Exec(t.Context(), trigger+" FOR EACH ROW EXECUTE FUNCTION "+function+"()"); err != nil {
		t.Fatal(err)
	}
	return remove
}

func TestLifeWriteOuterTransactionMatrixIntegration(t *testing.T) {
	for _, tool := range lifeWriteTools {
		for _, mode := range []string{"commit", "rollback", "result_failure", "commit_failure", "cancel"} {
			t.Run(tool+"/"+mode, func(t *testing.T) {
				database := newSmallPoolStore(t)
				write := lifeAcceptanceFixture(t, database, tool)
				remove := func() {}
				if mode == "result_failure" || mode == "commit_failure" {
					remove = lifeFailIdempotencyResult(t, database, write.actor, mode == "commit_failure")
				}
				before := lifeAcceptanceFingerprint(t, database)
				request := IdempotencyRequest{AccountID: write.actor, Key: newTestID(t), ToolName: tool}
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("outer boundary rejected")
				runs := 0
				apply := func(ctx context.Context) ([]byte, error) {
					runs++
					if err := write.run(ctx); err != nil {
						return nil, err
					}
					if got := lifeAcceptanceFingerprint(t, database); got != before {
						return nil, errors.New("저장점 해제 뒤 미커밋 업무가 외부에 보였다")
					}
					if mode == "rollback" {
						return nil, cause
					}
					if mode == "cancel" {
						cancel(cause)
					}
					return []byte(`{"ok": true}`), nil
				}
				_, err := database.ReplayIdempotent(ctx, request, apply)
				if runs != 1 {
					t.Fatalf("업무 호출=%d, err=%v", runs, err)
				}
				if mode == "commit" {
					if err != nil {
						t.Fatal(err)
					}
					after := lifeAcceptanceFingerprint(t, database)
					if after == before {
						t.Fatal("커밋된 업무가 없다")
					}
					if _, err := database.ReplayIdempotent(t.Context(), request, apply); err != nil || runs != 1 || after != lifeAcceptanceFingerprint(t, database) {
						t.Fatalf("재생이 상태·감사·큐·빈도·내용 판을 반복했다: %v", err)
					}
				} else {
					if err == nil || (mode == "rollback" || mode == "cancel") && !errors.Is(err, cause) || mode == "cancel" && !errors.Is(err, context.Canceled) {
						t.Fatalf("원인 분류=%v", err)
					}
					if mode == "result_failure" || mode == "commit_failure" {
						if _, ok := errors.AsType[*pgconn.PgError](err); !ok {
							t.Fatal(err)
						}
					}
					if before != lifeAcceptanceFingerprint(t, database) {
						t.Fatal("바깥 실패가 정점·간선·색인·누계·내용 판·감사·등급·빈도·재생 결과에 부분 적용을 남겼다")
					}
					remove()
					if _, err := database.ReplayIdempotent(t.Context(), request, func(ctx context.Context) ([]byte, error) {
						return []byte(`{"ok": true}`), write.run(ctx)
					}); err != nil {
						t.Fatalf("같은 식별자·판·한도로 재시도=%v", err)
					}
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeWriteLockSurvivesSavepointIntegration(t *testing.T) {
	for _, tool := range lifeWriteTools {
		t.Run(tool, func(t *testing.T) {
			database := newSmallPoolStore(t)
			write := lifeAcceptanceFixture(t, database, tool)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			outer, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(ctx, outer)
			if err := write.run(context.WithValue(ctx, writeTransactionContextKey{}, outer)); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := outer.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			var group sync.WaitGroup
			group.Go(func() {
				probe, err := database.pool.Begin(ctx)
				if err == nil {
					defer rollbackTransaction(ctx, probe)
					table, column, id := "context_graph", "graph_id", write.graph
					if tool == "graph_create" {
						table, column, id = "account", "account_id", write.actor
					}
					_, err = probe.Exec(ctx, "SELECT 1 FROM public."+table+" WHERE "+column+"=$1 FOR UPDATE", id.String())
				}
				done <- err
			})
			defer func() { cancel(); rollbackTransaction(ctx, outer); group.Wait() }()
			waitGrantWaiters(t, ctx, database, pid, 1)
			if err := outer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			group.Wait()
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

type lifeCurrentReadFailure struct {
	count, at int
	cause     error
	cancel    func()
}

type lifeCurrentReadFailureTx struct {
	pgx.Tx
	failure *lifeCurrentReadFailure
}

func (tx lifeCurrentReadFailureTx) Begin(ctx context.Context) (pgx.Tx, error) {
	nested, err := tx.Tx.Begin(ctx)
	return lifeCurrentReadFailureTx{Tx: nested, failure: tx.failure}, err
}

func (tx lifeCurrentReadFailureTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if strings.Contains(query, "RETURN node ORDER BY node.context_id") {
		tx.failure.count++
		if tx.failure.count == tx.failure.at {
			if tx.failure.cancel != nil {
				tx.failure.cancel()
			}
			return nil, tx.failure.cause
		}
	}
	return tx.Tx.Query(ctx, query, args...)
}

func TestLifeCurrentStateReadFailureIntegration(t *testing.T) {
	for _, action := range []string{"discard", "restore", "keep", "event_update"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%t", action, canceled), func(t *testing.T) {
				database := newSmallPoolStore(t)
				target := restoreChannelTarget(t, database, model.LayerEvent)
				if action == "restore" {
					if _, err := database.DiscardContext(t.Context(), target.GraphID, target.ID, registrationOperation(target, OperationDiscard), WriteLimits{}); err != nil {
						t.Fatal(err)
					}
				}
				run := func(ctx context.Context) error {
					switch action {
					case "discard":
						_, err := database.DiscardContext(ctx, target.GraphID, target.ID, registrationOperation(target, OperationDiscard), WriteLimits{})
						return err
					case "restore":
						_, err := database.RestoreContext(ctx, target.GraphID, target.ID, registrationOperation(target, OperationUpdate), WriteLimits{})
						return err
					case "keep":
						_, err := database.KeepContext(ctx, target.GraphID, target.ID, target.Version, registrationOperation(target, OperationKeep))
						return err
					default:
						next := target
						next.Version++
						next.Body += " next"
						_, err := database.UpdateContextWithOperation(ctx, target.GraphID, target.Version, next, registrationOperation(target, OperationUpdate), WriteLimits{})
						return err
					}
				}
				outer, err := database.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer rollbackTransaction(t.Context(), outer)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("current state canceled")
				failure := &lifeCurrentReadFailure{at: 2, cause: &pgconn.PgError{Code: "XX000", Message: "synthetic current state failure"}}
				if action == "event_update" {
					failure.at = 1
				}
				if canceled {
					failure.cancel = func() { cancel(cause) }
				}
				before := lifeAcceptanceFingerprint(t, database)
				ctx = context.WithValue(ctx, writeTransactionContextKey{}, pgx.Tx(lifeCurrentReadFailureTx{Tx: outer, failure: failure}))
				err = run(ctx)
				if _, ok := errors.AsType[*StorageError](err); !ok || !errors.Is(err, failure.cause) || canceled && (!errors.Is(err, cause) || !errors.Is(err, context.Canceled)) {
					t.Fatal(err)
				}
				if failure.count != failure.at {
					t.Fatal("현재 상태 재조회 전에 실패했다")
				}
				if err := outer.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				if before != lifeAcceptanceFingerprint(t, database) {
					t.Fatal("현재 상태 재조회 실패의 부분 적용")
				}
				if err := run(t.Context()); err != nil {
					t.Fatal(err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}
