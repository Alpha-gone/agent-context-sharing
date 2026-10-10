package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func testMCPDiscardOperation(value model.Context) *OperationRecord {
	return &OperationRecord{Kind: OperationDiscard, GraphID: value.GraphID, ContextID: value.ID,
		TargetVersion: value.Version, JudgmentInput: "복구 채널 시험 폐기",
		AccountID: value.CreatedBy, AgentID: value.CreatedByAgent}
}

func restoreChannelTarget(t *testing.T, database *Store, layer model.Layer) model.Context {
	t.Helper()
	graph, actor, source := boundaryGraph(t, database)
	cleanupIndexTasks(t, database, source.ID)
	if layer == model.LayerSource {
		return source
	}
	value := testDerivedContext(t, graph, actor)
	refs := []model.ID{source.ID}
	if layer == model.LayerEvent {
		value = testEventContext(t, graph, actor, source.ID)
		refs = nil
	}
	stored, err := database.CreateContext(t.Context(), graph, value, refs)
	if err != nil {
		t.Fatal(err)
	}
	cleanupIndexTasks(t, database, stored.ID)
	return stored
}

func TestRestoreChannelLifecycleIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		t.Run(string(layer), func(t *testing.T) {
			database := newIntegrationStore(t)
			target := restoreChannelTarget(t, database, layer)
			graph, id, actor := target.GraphID, target.ID, target.CreatedBy
			operation := testMCPDiscardOperation(target)
			if _, err := database.DiscardContext(t.Context(), graph, id, operation, WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			operation.Kind = OperationUpdate
			if _, err := database.RestoreContext(t.Context(), graph, id, operation, WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			if _, err := database.SetContextDeleted(t.Context(), graph, id, actor, true, 0); err != nil {
				t.Fatal(err)
			}
			if found, err := database.HasAppliedDiscard(t.Context(), graph, id); err != nil || found {
				t.Fatalf("현재 웹 삭제에 과거 폐기 기록이 적용됐다: %t, %v", found, err)
			}
			if _, err := database.RestoreContext(t.Context(), graph, id, operation, WriteLimits{}); !errors.Is(err, ErrRestoreChannel) {
				t.Fatalf("과거 MCP 복구 뒤 웹 삭제의 MCP 복구: %v", err)
			}
			if _, err := database.SetContextDeleted(t.Context(), graph, id, actor, false, 0); err != nil {
				t.Fatal(err)
			}
			operation.Kind = OperationDiscard
			if _, err := database.DiscardContext(t.Context(), graph, id, operation, WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			// 실제 시각을 바꾸지 않고 보존 기간 이후에 정리를 실행한다.
			future := time.Now().UTC().AddDate(1, 0, 0)
			retention := func(owner model.ID) int {
				if owner == actor {
					return 1
				}
				return 0
			}
			cleanup := func() {
				t.Helper()
				if _, err := database.CleanupAuditRecords(t.Context(), future, retention, retention); err != nil {
					t.Fatal(err)
				}
			}
			cleanup()
			var records int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE context_id = $1`, id.String()).Scan(&records); err != nil {
				t.Fatal(err)
			}
			if records != 1 {
				t.Fatalf("정리 뒤 현재 폐기 근거 = %d, want 1", records)
			}
			operation.Kind = OperationUpdate
			if _, err := database.RestoreContext(t.Context(), graph, id, operation, WriteLimits{}); err != nil {
				t.Fatalf("감사 보존 기간 뒤 MCP 복구: %v", err)
			}
			cleanup()
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE context_id = $1`, id.String()).Scan(&records); err != nil {
				t.Fatal(err)
			}
			if records != 0 {
				t.Fatalf("복구 뒤 만료 기록 = %d, want 0", records)
			}
			if _, err := database.SetContextDeleted(t.Context(), graph, id, actor, true, 0); err != nil {
				t.Fatal(err)
			}
			cleanup()
			if _, err := database.RestoreContext(t.Context(), graph, id, operation, WriteLimits{}); !errors.Is(err, ErrRestoreChannel) {
				t.Fatalf("웹 감사 정리 뒤 MCP 복구: %v", err)
			}
		})
	}
}

// 잠금 대기 전에 읽은 삭제 채널이 현재 상태의 허용 판정을 대신하지 않아야 한다.
func TestRestoreChannelRechecksAfterWebTransitionIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		for _, finalMCP := range []bool{false, true} {
			name := string(layer) + "/web"
			if finalMCP {
				name = string(layer) + "/mcp"
			}
			t.Run(name, func(t *testing.T) {
				database := newSmallPoolStore(t)
				target := restoreChannelTarget(t, database, layer)
				graph, id, actor := target.GraphID, target.ID, target.CreatedBy
				operation := testMCPDiscardOperation(target)
				if finalMCP {
					if _, err := database.SetContextDeleted(t.Context(), graph, id, actor, true, 0); err != nil {
						t.Fatal(err)
					}
				} else if _, err := database.DiscardContext(t.Context(), graph, id, operation, WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				first, err := database.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer first.Rollback(context.WithoutCancel(ctx))
				firstContext := context.WithValue(ctx, writeTransactionContextKey{}, first)
				if _, err := database.SetContextDeleted(firstContext, graph, id, actor, false, 0); err != nil {
					t.Fatal(err)
				}
				if finalMCP {
					if _, err := database.DiscardContext(firstContext, graph, id, operation, WriteLimits{}); err != nil {
						t.Fatal(err)
					}
				} else if _, err := database.SetContextDeleted(firstContext, graph, id, actor, true, 0); err != nil {
					t.Fatal(err)
				}
				second, err := database.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer second.Rollback(context.WithoutCancel(ctx))
				operation.Kind = OperationUpdate
				done := make(chan error, 1)
				var group sync.WaitGroup
				defer func() {
					cancel()
					first.Rollback(context.WithoutCancel(ctx))
					group.Wait()
					second.Rollback(context.WithoutCancel(ctx))
				}()
				group.Go(func() {
					_, err := database.RestoreContext(context.WithValue(ctx, writeTransactionContextKey{}, second), graph, id, operation, WriteLimits{})
					done <- err
				})
				// 시간 지연을 추측하지 않고 실제 DB 잠금 대기를 확인한다.
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for blocked := false; !blocked; {
					select {
					case err := <-done:
						t.Fatalf("선행 커밋 전에 복구 종료: %v", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-ticker.C:
						if err := database.pool.QueryRow(ctx, `SELECT $1::integer = ANY(pg_blocking_pids($2::integer))`, first.Conn().PgConn().PID(), second.Conn().PgConn().PID()).Scan(&blocked); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := first.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				err = <-done
				if finalMCP {
					if err != nil {
						t.Fatalf("잠금 뒤 새 MCP 폐기의 복구: %v", err)
					}
					if err := second.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrRestoreChannel) {
					t.Fatalf("잠금 뒤 새 웹 삭제의 MCP 복구: %v", err)
				}
				second.Rollback(ctx)
				current, err := database.Context(ctx, graph, id)
				if err != nil || (current.DeletedAt == nil) != finalMCP {
					t.Fatalf("최종 삭제 상태: %+v, %v", current, err)
				}
			})
		}
	}
}

func TestCleanupRetainsDiscardDuringUncommittedRestoreIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	target := restoreChannelTarget(t, database, model.LayerSource)
	operation := testMCPDiscardOperation(target)
	if _, err := database.DiscardContext(t.Context(), target.GraphID, target.ID, operation, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	if _, err := database.SetContextDeleted(context.WithValue(t.Context(), writeTransactionContextKey{}, tx), target.GraphID, target.ID, target.CreatedBy, false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CleanupAuditRecords(t.Context(), time.Now().UTC().AddDate(1, 0, 0), func(owner model.ID) int {
		if owner == target.CreatedBy {
			return 1
		}
		return 0
	}, func(model.ID) int { return 0 }); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	operation.Kind = OperationUpdate
	if _, err := database.RestoreContext(t.Context(), target.GraphID, target.ID, operation, WriteLimits{}); err != nil {
		t.Fatalf("복구 롤백 뒤 현재 폐기 근거가 사라졌다: %v", err)
	}
}
