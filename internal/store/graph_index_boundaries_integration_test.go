package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"github.com/jackc/pgx/v5"
)

// boundaryGraph는 각 회귀 시험이 독립적으로 사용하는 그래프와 원천을 만든다.
func boundaryGraph(t *testing.T, database *Store) (model.ID, model.ID, model.Context) {
	t.Helper()
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://boundaries/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	return graph, actor, source
}

func TestEventUpdateChecksOnlyConfirmedRelationTimesIntegration(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirmed=%t", confirmed), func(t *testing.T) {
			database := newIntegrationStore(t)
			graph, actor, source := boundaryGraph(t, database)
			a, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
			if err != nil {
				t.Fatal(err)
			}
			b, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
			if err != nil {
				t.Fatal(err)
			}
			if confirmed {
				_, err = database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypePrecedes, a.ID, b.ID), nil)
			} else {
				proposal := proposedRelation(graph, model.RelationTypePrecedes, a.ID, b.ID)
				proposal.ID = newTestID(t)
				_, err = database.CreateRelation(t.Context(), graph, proposal)
			}
			if err != nil {
				t.Fatal(err)
			}
			// 구성원 발생 시각은 유지하면서 시작 순서만 뒤집는다.
			next := a
			attributes := *a.Event
			attributes.Start = b.Event.Start.Add(time.Microsecond)
			next.Event = &attributes
			next.Version++
			_, err = database.UpdateContext(t.Context(), graph, a.Version, next)
			if confirmed {
				if !errors.Is(err, ErrInvalidRelation) {
					t.Fatalf("확정 시간 위반: %v", err)
				}
				if _, ok := errors.AsType[model.FieldError](err); !ok {
					t.Fatalf("입력 필드 오류가 없다: %v", err)
				}
				stored, readErr := database.Context(t.Context(), graph, a.ID)
				if readErr != nil || stored.Version != a.Version {
					t.Fatalf("거부된 갱신이 남았다: %+v, %v", stored, readErr)
				}
			} else if err != nil {
				t.Fatalf("제안이 사건 갱신을 막았다: %v", err)
			}
		})
	}
}

func TestHopIncludesLastDepthEdgesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, actor, source := boundaryGraph(t, database)
	first, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID}, first.ID, nil, WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, prefetched := range []bool{false, true} {
		var result HopResult
		if prefetched {
			result, err = database.hopContextsPrefetched(t.Context(), graph, []model.Context{source}, 1, "both", nil, 0)
		} else {
			result, err = database.HopContexts(t.Context(), graph, source.ID, 1, "both", nil, 0)
		}
		if err != nil || len(result.Contexts) != 3 || !slices.Contains(result.Edges, HopEdge{FromID: second.ID, ToID: first.ID, Kind: "supersedes"}) {
			t.Fatalf("사전 조회=%t, 마지막 깊이 간선: %+v, %v", prefetched, result, err)
		}
	}
}

func TestRestoreRequeuesMissingIndexAndChecksLimitIntegration(t *testing.T) {
	for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
		t.Run(string(targets), func(t *testing.T) {
			database := newIntegrationStoreWithTargets(t, targets)
			graph, actor, source := boundaryGraph(t, database)
			target := source
			if targets == IndexTargetsWithoutSource {
				var err error
				target, err = database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
				if err != nil {
					t.Fatal(err)
				}
			}
			readyIndexTasks(t, database, graph, target.ID)
			if _, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, scopeTestProcessor(embeddingDimension(t, database))); err != nil {
				t.Fatal(err)
			}
			if _, err := database.DiscardContext(t.Context(), graph, target.ID, testMCPDiscardOperation(target), WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			// 차원 전환이 삭제 대상의 임베딩과 대기 작업을 비운 상태를 재현한다.
			if _, err := database.pool.Exec(t.Context(), `DELETE FROM public.context_embedding WHERE context_id = $1`, target.ID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := database.pool.Exec(t.Context(), `DELETE FROM public.index_task WHERE context_id = $1`, target.ID.String()); err != nil {
				t.Fatal(err)
			}
			_, err := database.RestoreContext(t.Context(), graph, target.ID, nil, WriteLimits{StoredCharsPerGraph: 1})
			if _, ok := errors.AsType[plan.LimitError](err); !ok {
				t.Fatalf("복구 한도: %v", err)
			}
			if indexTaskCount(t, database, target.ID) != 0 {
				t.Fatal("거부된 복구가 색인 작업을 남겼다")
			}
			if _, err := database.RestoreContext(t.Context(), graph, target.ID, nil, WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			if indexTaskCount(t, database, target.ID) != 1 {
				t.Fatal("복구가 색인 작업을 등록하지 않았다")
			}
			readyIndexTasks(t, database, graph, target.ID)
			result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, scopeTestProcessor(embeddingDimension(t, database)))
			if err != nil || !result.Succeeded {
				t.Fatalf("복구 대상 재색인: %+v, %v", result, err)
			}
			if _, err := database.SetContextDeleted(t.Context(), graph, target.ID, actor, true, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := database.SetContextDeleted(t.Context(), graph, target.ID, actor, false, 1); err == nil {
				t.Fatal("웹 복구가 저장량 한도를 우회했다")
			}
		})
	}
}

func TestIndexFailuresConsumeAttemptBudgetIntegration(t *testing.T) {
	for _, stage := range []string{"read", "save", "empty"} {
		t.Run(stage, func(t *testing.T) {
			database := newIntegrationStore(t)
			graph, actor, target := boundaryGraph(t, database)
			other, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://healthy/"+graph.String()), nil)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "read" {
				query := "MATCH (n:Context {context_id: " + cypherString(target.ID.String()) + "}) SET n.version = 0 RETURN n"
				if _, err := database.pool.Exec(t.Context(), database.cypherSQL(query, "n agtype"), pgx.QueryExecModeExec); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					query := "MATCH (n:Context {context_id: " + cypherString(target.ID.String()) + "}) SET n.version = 1 RETURN n"
					if _, err := database.pool.Exec(context.WithoutCancel(t.Context()), database.cypherSQL(query, "n agtype"), pgx.QueryExecModeExec); err != nil {
						t.Errorf("조회 실패 표본 복원: %v", err)
					}
				})
			}
			processor := func(context.Context, IndexTask) IndexTaskResult {
				if stage == "read" {
					t.Fatal("읽기 실패 뒤 제공자를 호출했다")
				}
				if stage == "empty" {
					return IndexTaskResult{}
				}
				return IndexTaskResult{Embedding: []float64{1}, ModelID: "wrong-dimension"}
			}
			attemptLimit := maxIndexAttempts
			if stage == "empty" {
				attemptLimit = 1
			}
			for attempt := 1; attempt <= attemptLimit; attempt++ {
				readyIndexTasks(t, database, graph, target.ID)
				result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, processor)
				if err == nil || !result.Found {
					t.Fatalf("실패 회차 %d: %+v, %v", attempt, result, err)
				}
				var attempts int
				var state string
				var delayed bool
				if err := database.pool.QueryRow(t.Context(), `SELECT attempts, state, next_attempt_at > now() FROM public.index_task WHERE context_id = $1`, target.ID.String()).Scan(&attempts, &state, &delayed); err != nil {
					t.Fatal(err)
				}
				if attempts != attempt || (attempt < attemptLimit && (!delayed || state != "pending")) || (attempt == attemptLimit && state != "failed") {
					t.Fatalf("재시도 예산: attempts=%d state=%s delayed=%t", attempts, state, delayed)
				}
			}
			readyIndexTasks(t, database, graph, other.ID)
			result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, scopeTestProcessor(embeddingDimension(t, database)))
			if err != nil || !result.Succeeded || result.Task.ContextID != other.ID {
				t.Fatalf("후속 정상 작업: %+v, %v", result, err)
			}
		})
	}
}

func TestContextRejectsNULAndInvalidCypherTextIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, actor, source := boundaryGraph(t, database)
	for _, body := range []string{"본문\x00", string([]byte{0xff})} {
		value := testDerivedContext(t, graph, actor)
		value.Body = body
		_, err := database.CreateContext(t.Context(), graph, value, []model.ID{source.ID})
		if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != "body" {
			t.Fatalf("본문 검증: %v", err)
		}
		if _, err := database.findSource(t.Context(), graph, body); !errors.Is(err, ErrNotFound) {
			t.Fatalf("유효하지 않은 질의 문자열: %v", err)
		}
	}
}

func TestDeletedGraphRejectsWritesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, actor, source := boundaryGraph(t, database)
	derived, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetGraphDeleted(t.Context(), graph, actor, true); err != nil {
		t.Fatal(err)
	}
	derived.Version++
	derived.Body = "삭제 뒤 갱신"
	for name, write := range map[string]func() error{
		"create": func() error {
			_, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
			return err
		},
		"update": func() error { _, err := database.UpdateContext(t.Context(), graph, 1, derived); return err },
		"discard": func() error {
			_, err := database.DiscardContext(t.Context(), graph, source.ID, nil, WriteLimits{})
			return err
		},
		"keep": func() error { _, err := database.KeepContext(t.Context(), graph, source.ID, 1, nil); return err },
		"graph": func() error {
			_, err := database.UpdateGraph(t.Context(), graph, 1, "삭제 뒤 변경", "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("삭제 그래프 쓰기: %v", err)
			}
		})
	}
	stored, err := database.Context(t.Context(), graph, derived.ID)
	if err != nil || stored.Version != 1 || stored.Body == derived.Body {
		t.Fatalf("삭제 그래프에 변경이 남았다: %+v, %v", stored, err)
	}
}

func TestAuditCleanupPreservesRestoreStateIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, requester, operator := operatorRestoreFixture(t, database, true)
	old := time.Now().UTC().AddDate(0, 0, -60)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.web_audit_log SET occurred_at = $2 WHERE graph_id = $1`, graph.String(), old); err != nil {
		t.Fatal(err)
	}
	metadata, err := database.Graph(t.Context(), graph)
	if err != nil {
		t.Fatal(err)
	}
	days := func(account model.ID) int {
		if account == metadata.CreatedBy {
			return 1
		}
		return 0
	}
	if _, err := database.CleanupAuditRecords(t.Context(), time.Now().UTC(), days, days); err != nil {
		t.Fatal(err)
	}
	eligible, err := database.ListRestoreEligibleGraphs(t.Context(), requester)
	if err != nil || !containsGraph(eligible, graph) {
		t.Fatalf("소유자 복구 자격 유실: %+v, %v", eligible, err)
	}
	pending, err := database.PendingRestoreRequests(t.Context())
	if err != nil || !slices.ContainsFunc(pending, func(request model.RestoreRequest) bool { return request.GraphID == graph }) {
		t.Fatalf("미처리 요청 유실: %+v, %v", pending, err)
	}
	if err := database.OperatorRestoreGraph(t.Context(), graph, operator); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CleanupAuditRecords(t.Context(), time.Now().UTC().AddDate(0, 0, 60), days, days); err != nil {
		t.Fatal(err)
	}
	pending, err = database.PendingRestoreRequests(t.Context())
	if err != nil || slices.ContainsFunc(pending, func(request model.RestoreRequest) bool { return request.GraphID == graph }) {
		t.Fatalf("처리 요청이 대기로 돌아왔다: %+v, %v", pending, err)
	}
}

// TestGraphDeletionWinsConcurrentWritesIntegration은 전송 계층의 활성 판정 뒤 삭제가
// 먼저 커밋되어도 저장소가 쓰기를 되돌리는지 실제 행 잠금 경합으로 확인한다.
func TestGraphDeletionWinsConcurrentWritesIntegration(t *testing.T) {
	for _, action := range []string{"context", "metadata"} {
		t.Run(action, func(t *testing.T) {
			database := newIntegrationStore(t)
			graph, actor, _ := boundaryGraph(t, database)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			var holderPID int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE public.context_graph SET deleted_at = now() WHERE graph_id = $1`, graph.String()); err != nil {
				t.Fatal(err)
			}
			value := testSourceContext(t, graph, actor, "api://deletion-race/"+graph.String())
			finished := make(chan error, 1)
			go func() {
				if action == "context" {
					_, err := database.CreateContext(ctx, graph, value, nil)
					finished <- err
				} else {
					_, err := database.UpdateGraph(ctx, graph, 1, "삭제 경합 변경", "")
					finished <- err
				}
			}()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err := database.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND $1::integer = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-finished:
					t.Fatalf("삭제 잠금과 경합하지 않았다: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-finished; !errors.Is(err, ErrNotFound) {
				t.Fatalf("삭제 뒤 쓰기 결과: %v", err)
			}
			if action == "context" {
				if _, err := database.Context(ctx, graph, value.ID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("거부된 정점이 남았다: %v", err)
				}
				if indexTaskCount(t, database, value.ID) != 0 {
					t.Fatal("거부된 색인 작업이 남았다")
				}
			}
		})
	}
}

func TestPeriodicJobsDoNotStarveSmallPoolIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var group sync.WaitGroup
	errors := make(chan error, 9)
	for job := range 9 {
		group.Go(func() {
			_, acquired, err := database.WithTryAdvisoryLock(ctx, 8200100+int64(job), func(ctx context.Context) (int, error) {
				// 잠금 연결을 확보한 여러 작업이 본문 연결도 필요하게 만든다.
				time.Sleep(30 * time.Millisecond)
				_, err := database.pool.Exec(ctx, `SELECT 1`)
				return 1, err
			})
			if err == nil && !acquired {
				err = fmt.Errorf("독립 주기 작업의 잠금을 얻지 못했다")
			}
			errors <- err
		})
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
}
