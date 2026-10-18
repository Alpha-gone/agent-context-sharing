package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"github.com/jackc/pgx/v5"
)

func lifeNodeDeletion(t *testing.T, database *Store, ctx context.Context, target model.Context, discard bool, operation *OperationRecord, limits WriteLimits) (model.Context, error) {
	t.Helper()
	if discard {
		return database.DiscardContext(ctx, target.GraphID, target.ID, operation, limits)
	}
	return database.RestoreContext(ctx, target.GraphID, target.ID, operation, limits)
}

func lifeNodeDeletionOperation(target model.Context, discard bool) *OperationRecord {
	kind := OperationUpdate
	if discard {
		kind = OperationDiscard
	}
	return registrationOperation(target, kind)
}

func TestLifeNodeDeletionRoundTripIntegration(t *testing.T) {
	for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
		for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
			t.Run(string(targets)+"/"+string(layer), func(t *testing.T) {
				database := newSmallPoolStore(t)
				database.indexTargets = targets
				target := restoreChannelTarget(t, database, layer)
				initialTasks := indexTaskCount(t, database, target.ID)
				var previousTask registrationTask
				if initialTasks != 0 {
					claimExec(t, database, `UPDATE public.index_task SET state='failed',attempts=3,last_error='synthetic' WHERE context_id=$1`, target.ID.String())
					previousTask = registrationTaskFor(t, database, target.ID)
				}
				graph, err := database.Graph(t.Context(), target.GraphID)
				if err != nil {
					t.Fatal(err)
				}
				revision := lifeContentRevision(t, database, target.GraphID)
				deleted, err := lifeNodeDeletion(t, database, t.Context(), target, true, lifeNodeDeletionOperation(target, true), WriteLimits{})
				if err != nil || deleted.DeletedAt == nil {
					t.Fatalf("폐기=%+v/%v", deleted, err)
				}
				before := registrationStateFingerprint(t, database)
				if _, err := lifeNodeDeletion(t, database, t.Context(), target, true, lifeNodeDeletionOperation(target, true), WriteLimits{}); !errors.Is(err, ErrInvalidState) {
					t.Fatal(err)
				}
				if before != registrationStateFingerprint(t, database) {
					t.Fatal("재폐기의 부분 적용")
				}
				after, err := database.Graph(t.Context(), target.GraphID)
				if err != nil || after.StoredChars != graph.StoredChars-int64(utf8.RuneCountInString(target.Body)) || lifeContentRevision(t, database, target.GraphID) != revision+1 {
					t.Fatalf("폐기 누계/내용 판=%+v/%v", after, err)
				}
				if indexTaskCount(t, database, target.ID) != initialTasks || initialTasks != 0 && !reflect.DeepEqual(previousTask, registrationTaskFor(t, database, target.ID)) {
					t.Fatal("폐기가 기존 색인 작업을 변경했다")
				}
				restored, err := lifeNodeDeletion(t, database, t.Context(), target, false, lifeNodeDeletionOperation(target, false), WriteLimits{})
				if err != nil || restored.DeletedAt != nil {
					t.Fatalf("복구=%+v/%v", restored, err)
				}
				want := target
				if layer != model.LayerSource {
					want.Version += 2
				}
				if !reflect.DeepEqual(want, restored) {
					t.Fatalf("복구 속성/참조가 달라졌다: want=%+v got=%+v", want, restored)
				}
				after, err = database.Graph(t.Context(), target.GraphID)
				if err != nil || after.StoredChars != graph.StoredChars || after.LastActivityAt.Before(graph.LastActivityAt) || lifeContentRevision(t, database, target.GraphID) != revision+2 {
					t.Fatalf("복구 누계/활동/내용 판=%+v/%v", after, err)
				}
				wantTasks := 1
				if targets == IndexTargetsWithoutSource && layer == model.LayerSource {
					wantTasks = 0
				}
				if indexTaskCount(t, database, target.ID) != wantTasks {
					t.Fatal("복구 색인 대상 구성 위반")
				}
				if wantTasks != 0 {
					task := registrationTaskFor(t, database, target.ID)
					if task.id != previousTask.id || task.state != "pending" || task.attempts != 0 || task.lastError != nil {
						t.Fatal("복구가 기존 실패 작업을 pending으로 다시 등록하지 않았다")
					}
				}
				var records int
				if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE graph_id=$1 AND context_id=$2 AND result='applied' AND actor_account_id=$3 AND actor_agent_id=$4`, target.GraphID.String(), target.ID.String(), target.CreatedBy.String(), target.CreatedByAgent.String()).Scan(&records); err != nil || records != 2 {
					t.Fatalf("폐기/복구 감사=%d/%v", records, err)
				}
				before = registrationStateFingerprint(t, database)
				if _, err := lifeNodeDeletion(t, database, t.Context(), target, false, lifeNodeDeletionOperation(target, false), WriteLimits{}); !errors.Is(err, ErrInvalidState) {
					t.Fatal(err)
				}
				if before != registrationStateFingerprint(t, database) {
					t.Fatal("재복구의 부분 적용")
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeNodeDeletionAuditBindingIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		for _, discard := range []bool{true, false} {
			for _, field := range []string{"kind", "graph", "context", "relation"} {
				t.Run(fmt.Sprintf("%s/discard=%t/%s", layer, discard, field), func(t *testing.T) {
					database := newSmallPoolStore(t)
					target := restoreChannelTarget(t, database, layer)
					if !discard {
						if _, err := lifeNodeDeletion(t, database, t.Context(), target, true, lifeNodeDeletionOperation(target, true), WriteLimits{}); err != nil {
							t.Fatal(err)
						}
					}
					operation := lifeNodeDeletionOperation(target, discard)
					switch field {
					case "kind":
						operation.Kind = OperationKeep
					case "graph":
						operation.GraphID = newTestID(t)
					case "context":
						operation.ContextID = newTestID(t)
					case "relation":
						operation.RelationID = newTestID(t)
					}
					before := registrationStateFingerprint(t, database)
					_, err := lifeNodeDeletion(t, database, t.Context(), target, discard, operation, WriteLimits{})
					if _, ok := errors.AsType[model.FieldError](err); !ok {
						t.Fatalf("감사 대상 불일치=%v", err)
					}
					if before != registrationStateFingerprint(t, database) {
						t.Fatal("잘못된 감사 입력이 상태/기록을 변경했다")
					}
				})
			}
		}
	}
}

func TestLifeNodeDeletionRollbackIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		for _, discard := range []bool{true, false} {
			for _, stage := range []string{"age", "graph", "audit", "commit", "queue", "limit"} {
				if discard && (stage == "queue" || stage == "limit") {
					continue
				}
				t.Run(fmt.Sprintf("%s/discard=%t/%s", layer, discard, stage), func(t *testing.T) {
					database := newSmallPoolStore(t)
					target := restoreChannelTarget(t, database, layer)
					if !discard {
						if _, err := lifeNodeDeletion(t, database, t.Context(), target, true, lifeNodeDeletionOperation(target, true), WriteLimits{}); err != nil {
							t.Fatal(err)
						}
					}
					operation := lifeNodeDeletionOperation(target, discard)
					ctx := t.Context()
					var outer pgx.Tx
					remove := func() {}
					limits := WriteLimits{}
					switch stage {
					case "age":
						var err error
						outer, err = database.pool.Begin(ctx)
						if err != nil {
							t.Fatal(err)
						}
						defer rollbackTransaction(t.Context(), outer)
						ctx = context.WithValue(ctx, writeTransactionContextKey{}, pgx.Tx(nodeCreationFailureTx{Tx: outer, fragment: " SET node = "}))
					case "limit":
						limits.StoredCharsPerGraph = 1
					default:
						table, event := "operation_log", "INSERT"
						if stage == "graph" {
							table, event = "context_graph", "UPDATE"
						}
						if stage == "queue" {
							table = "index_task"
						}
						remove = lifeFailGraphWrite(t, database, target.GraphID, table, event, stage == "commit")
					}
					before := registrationStateFingerprint(t, database)
					_, err := lifeNodeDeletion(t, database, ctx, target, discard, operation, limits)
					if stage == "limit" {
						if _, ok := errors.AsType[plan.LimitError](err); !ok {
							t.Fatalf("복구 한도=%v", err)
						}
					} else if _, ok := errors.AsType[*StorageError](err); !ok {
						t.Fatalf("실패 주입=%v", err)
					}
					if outer != nil {
						if err := outer.Commit(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					if before != registrationStateFingerprint(t, database) {
						t.Fatal("전이 실패 후 정점/간선/이력/큐/누계/감사의 부분 적용")
					}
					remove()
					if _, err := lifeNodeDeletion(t, database, t.Context(), target, discard, operation, WriteLimits{}); err != nil {
						t.Fatalf("실패 후 재시도=%v", err)
					}
					assertConnectionRecovery(t, database, nil, 0)
				})
			}
		}
	}
}

func TestLifeNodeDeletionConcurrentIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		for _, discard := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/discard=%t", layer, discard), func(t *testing.T) {
				database := newSmallPoolStore(t)
				target := restoreChannelTarget(t, database, layer)
				if !discard {
					var err error
					target, err = lifeNodeDeletion(t, database, t.Context(), target, true, lifeNodeDeletionOperation(target, true), WriteLimits{})
					if err != nil {
						t.Fatal(err)
					}
				}
				graph, err := database.Graph(t.Context(), target.GraphID)
				if err != nil {
					t.Fatal(err)
				}
				revision := lifeContentRevision(t, database, target.GraphID)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				const requests = 6
				results := make(chan error, requests)
				start := make(chan struct{})
				var group sync.WaitGroup
				for range requests {
					group.Go(func() {
						<-start
						_, err := lifeNodeDeletion(t, database, ctx, target, discard, lifeNodeDeletionOperation(target, discard), WriteLimits{})
						results <- err
					})
				}
				close(start)
				group.Wait()
				close(results)
				succeeded := 0
				for err := range results {
					if err == nil {
						succeeded++
					} else if !errors.Is(err, ErrInvalidState) {
						t.Fatal(err)
					}
				}
				if succeeded != 1 {
					t.Fatalf("같은 전이 성공=%d", succeeded)
				}
				after, err := database.Graph(t.Context(), target.GraphID)
				if err != nil {
					t.Fatal(err)
				}
				delta := int64(utf8.RuneCountInString(target.Body))
				if discard {
					delta = -delta
				}
				if after.StoredChars != graph.StoredChars+delta || lifeContentRevision(t, database, target.GraphID) != revision+1 {
					t.Fatal("동시 전이 누계/내용 판 중복")
				}
				var records int
				operation := lifeNodeDeletionOperation(target, discard)
				if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE context_id=$1 AND operation_kind=$2 AND result='applied'`, target.ID.String(), string(operation.Kind)).Scan(&records); err != nil || records != 1 {
					t.Fatalf("동시 전이 감사=%d/%v", records, err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeNodeDeletionCancelIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		for _, discard := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/discard=%t", layer, discard), func(t *testing.T) {
				database := newSmallPoolStore(t)
				target := restoreChannelTarget(t, database, layer)
				if !discard {
					if _, err := lifeNodeDeletion(t, database, t.Context(), target, true, lifeNodeDeletionOperation(target, true), WriteLimits{}); err != nil {
						t.Fatal(err)
					}
				}
				before := registrationStateFingerprint(t, database)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				tx, err := database.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer rollbackTransaction(t.Context(), tx)
				if layer == model.LayerEvent {
					if err := requireActiveGraph(ctx, tx, target.GraphID); err != nil {
						t.Fatal(err)
					}
				}
				if err := database.lockContext(ctx, tx, target.GraphID, target.ID); err != nil {
					t.Fatal(err)
				}
				request, stop := context.WithCancelCause(ctx)
				var group sync.WaitGroup
				defer func() { stop(context.Canceled); group.Wait() }()
				result := make(chan error, 1)
				group.Go(func() {
					_, err := lifeNodeDeletion(t, database, request, target, discard, lifeNodeDeletionOperation(target, discard), WriteLimits{})
					result <- err
				})
				waitGrantWaiters(t, ctx, database, int(tx.Conn().PgConn().PID()), 1)
				cause := errors.New("life07 합성 취소")
				stop(cause)
				if err := <-result; !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
					t.Fatalf("취소 원인=%v", err)
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if before != registrationStateFingerprint(t, database) {
					t.Fatal("취소 전이의 부분 적용")
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeNodeRestoreEvidenceBoundaryIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		for _, mode := range []string{"before", "equal", "rejected", "other_target"} {
			t.Run(string(layer)+"/"+mode, func(t *testing.T) {
				database := newSmallPoolStore(t)
				target := restoreChannelTarget(t, database, layer)
				if _, err := lifeNodeDeletion(t, database, t.Context(), target, true, lifeNodeDeletionOperation(target, true), WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				query := `UPDATE public.operation_log AS operation SET applied_at=(SELECT (properties ->> 'deleted_at'::text)::timestamptz FROM ` + database.contextTable() + ` WHERE properties ->> 'context_id'::text=$1::text)`
				if mode == "before" {
					query += " - interval '1 microsecond'"
				}
				if mode == "rejected" {
					query += ",result='rejected',reject_reason='synthetic'"
				}
				if mode == "other_target" {
					query += ",context_id='" + newTestID(t).String() + "'::uuid"
				}
				query += " WHERE context_id=$1::uuid"
				claimExec(t, database, query, target.ID.String())
				allowed, err := database.HasAppliedDiscard(t.Context(), target.GraphID, target.ID)
				if err != nil || allowed != (mode == "equal") {
					t.Fatalf("현재 삭제 근거=%t/%v", allowed, err)
				}
				before := registrationStateFingerprint(t, database)
				_, err = lifeNodeDeletion(t, database, t.Context(), target, false, lifeNodeDeletionOperation(target, false), WriteLimits{})
				if mode == "equal" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if !errors.Is(err, ErrRestoreChannel) {
						t.Fatalf("복구 채널=%v", err)
					}
					if before != registrationStateFingerprint(t, database) {
						t.Fatal("채널 거부의 부분 적용")
					}
				}
			})
		}
	}
}
