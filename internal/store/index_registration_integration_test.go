package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func indexRequestContext(t *testing.T) (context.Context, model.ID) {
	t.Helper()
	id := newTestID(t)
	ctx, err := model.WithCorrelationID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, id
}

type registrationTask struct {
	id, graph, state       string
	attempts               int
	lastError, correlation *string
	enqueued, next         time.Time
}

func registrationTaskFor(t *testing.T, database *Store, id model.ID) registrationTask {
	t.Helper()
	var task registrationTask
	if err := database.pool.QueryRow(t.Context(), `SELECT task_id::text, graph_id::text, state, attempts,
		last_error, correlation_id, enqueued_at, next_attempt_at FROM public.index_task WHERE context_id=$1`, id.String()).Scan(
		&task.id, &task.graph, &task.state, &task.attempts, &task.lastError, &task.correlation, &task.enqueued, &task.next); err != nil {
		t.Fatal(err)
	}
	return task
}

func assertRegistration(t *testing.T, database *Store, node model.Context, taskID string, correlation model.ID, since time.Time) {
	t.Helper()
	if database.indexTargets == IndexTargetsWithoutSource && node.Layer == model.LayerSource {
		if indexTaskCount(t, database, node.ID) != 0 {
			t.Fatal("제외 계층을 등록했다")
		}
		return
	}
	task := registrationTaskFor(t, database, node.ID)
	var want *string
	if !correlation.IsZero() {
		want = new(correlation.String())
	}
	if indexTaskCount(t, database, node.ID) != 1 || (taskID != "" && task.id != taskID) || task.graph != node.GraphID.String() || task.state != "pending" || task.attempts != 0 || task.lastError != nil || !reflect.DeepEqual(task.correlation, want) || !task.enqueued.Equal(task.next) || task.enqueued.Before(since.Add(-time.Microsecond)) {
		t.Fatalf("등록 상태=%+v, 상관 ID=%v", task, want)
	}
}

func registrationOperation(node model.Context, kind OperationKind) *OperationRecord {
	return &OperationRecord{Kind: kind, GraphID: node.GraphID, ContextID: node.ID, TargetVersion: node.Version,
		JudgmentInput: "작업 등록 모듈 시험", AccountID: node.CreatedBy, AgentID: node.CreatedByAgent}
}

func TestIndexRegistrationLifecycleIntegration(t *testing.T) {
	for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
		t.Run(string(targets), func(t *testing.T) {
			database := newEmbeddingContractStore(t, "vector", 8)
			database.indexTargets = targets
			actor := newTestID(t)
			createTestAccount(t, database, actor)
			graph := createTestGraph(t, database, actor)
			seed, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://registration-seed/"+graph.String()), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
				t.Run(string(layer), func(t *testing.T) {
					value := testSourceContext(t, graph, actor, "api://registration/"+newTestID(t).String())
					var refs []model.ID
					if layer == model.LayerDerived {
						value = testDerivedContext(t, graph, actor)
						refs = []model.ID{seed.ID}
					}
					if layer == model.LayerEvent {
						value = testEventContext(t, graph, actor, seed.ID)
					}
					ctx, correlation := indexRequestContext(t)
					since := time.Now().UTC()
					node, err := database.CreateContextWithOperation(ctx, graph, value, refs, registrationOperation(value, OperationAdd), WriteLimits{})
					if err != nil {
						t.Fatal(err)
					}
					assertRegistration(t, database, node, "", correlation, since)
					var operations int
					if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE context_id=$1 AND operation_kind='add' AND result='applied'`, node.ID.String()).Scan(&operations); err != nil || operations != 1 {
						t.Fatalf("동일한 관리 기록=%d: %v", operations, err)
					}
					stored, err := database.Context(t.Context(), graph, node.ID)
					if err != nil || stored.Body != value.Body || stored.Layer != layer {
						t.Fatalf("저장 결과=%+v: %v", stored, err)
					}
					if layer == model.LayerDerived && (len(stored.Derived.DerivedFrom) != 1 || stored.Derived.DerivedFrom[0] != seed.ID) {
						t.Fatal("근거 간선이 달라졌다")
					}
					if layer == model.LayerEvent && (len(stored.Event.MemberIDs) != 1 || stored.Event.MemberIDs[0] != seed.ID) {
						t.Fatal("구성원 간선이 달라졌다")
					}
					taskID := ""
					if indexTaskCount(t, database, node.ID) != 0 {
						taskID = registrationTaskFor(t, database, node.ID).id
					}
					// 대기·실패 상태 모두 같은 행에 합쳐져야 하며 본문 불변 쓰기는 그대로 둔다.
					if _, err := database.pool.Exec(t.Context(), `UPDATE public.index_task SET attempts=5,state='failed',last_error='fixture',enqueued_at=now()-interval '1 day',next_attempt_at=now()+interval '1 day' WHERE context_id=$1`, node.ID.String()); err != nil {
						t.Fatal(err)
					}
					before := registrationStateFingerprint(t, database)
					if layer == model.LayerSource {
						duplicate := value
						duplicate.ID = newTestID(t)
						got, err := database.CreateContextWithOperation(ctx, graph, duplicate, nil, registrationOperation(duplicate, OperationAdd), WriteLimits{})
						if err != nil || got.ID != node.ID || registrationStateFingerprint(t, database) != before {
							t.Fatalf("원천 중복의 등록·감사 건너뜀: %v", err)
						}
					} else {
						taskBefore := registrationTaskFor(t, database, node.ID)
						next := node
						next.Version++
						node, err = database.UpdateContextWithOperation(ctx, graph, node.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
						if err != nil || !reflect.DeepEqual(registrationTaskFor(t, database, node.ID), taskBefore) {
							t.Fatalf("본문 불변 재등록=%v", err)
						}
						ctx, correlation = indexRequestContext(t)
						since = time.Now().UTC()
						next = node
						next.Body += " 새 본문"
						next.Version++
						node, err = database.UpdateContextWithOperation(ctx, graph, node.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
						if err != nil {
							t.Fatal(err)
						}
						assertRegistration(t, database, node, taskID, correlation, since)
					}
					if _, err := database.DiscardContext(t.Context(), graph, node.ID, registrationOperation(node, OperationDiscard), WriteLimits{}); err != nil {
						t.Fatal(err)
					}
					ctx, correlation = indexRequestContext(t)
					since = time.Now().UTC()
					node, err = database.RestoreContext(ctx, graph, node.ID, registrationOperation(node, OperationUpdate), WriteLimits{})
					if err != nil {
						t.Fatal(err)
					}
					assertRegistration(t, database, node, taskID, correlation, since)
					// 요청 없는 재색인은 옛 요청 상관 ID를 물려받지 않는다.
					since = time.Now().UTC()
					if err := database.ReindexGraph(t.Context(), graph); err != nil {
						t.Fatal(err)
					}
					assertRegistration(t, database, node, taskID, model.ID{}, since)
				})
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

// 개별 임시 DB의 업무 행 전체를 비교하되 시험 sequence·트리거는 제외한다.
func registrationStateFingerprint(t *testing.T, database *Store) string {
	t.Helper()
	tables := []string{"public.context_graph", "public.index_task", "public.context_embedding", "public.operation_log", "public.web_audit_log", database.contextTable(), pgx.Identifier{"agent_context", "_ag_label_edge"}.Sanitize()}
	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		parts = append(parts, `(SELECT COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY row_to_json(t)::text),'') FROM `+table+` t)`)
	}
	var result string
	if err := database.pool.QueryRow(t.Context(), `SELECT md5(`+strings.Join(parts, " || ")+`)`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIndexRegistrationRollbackIntegration(t *testing.T) {
	for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
		for _, action := range []string{"create_derived", "create_event", "update", "restore_mcp", "restore_web", "reindex"} {
			t.Run(string(targets)+"/"+action, func(t *testing.T) {
				database := newEmbeddingContractStore(t, "vector", 8)
				database.indexTargets = targets
				actor := newTestID(t)
				createTestAccount(t, database, actor)
				graph := createTestGraph(t, database, actor)
				grantAccount(t, database, graph, actor, model.GraphGradeOwner)
				seed, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://registration-rollback/"+graph.String()), nil)
				if err != nil {
					t.Fatal(err)
				}
				node, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{seed.ID})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, seed.ID), nil); err != nil {
					t.Fatal(err)
				}
				if action == "restore_mcp" {
					_, err = database.DiscardContext(t.Context(), graph, node.ID, registrationOperation(node, OperationDiscard), WriteLimits{})
				}
				if action == "restore_web" {
					_, err = database.SetContextDeleted(t.Context(), graph, node.ID, actor, true, 0)
				}
				if err != nil {
					t.Fatal(err)
				}
				failAt := 1
				if action == "reindex" {
					failAt = 2
				}
				if _, err := database.pool.Exec(t.Context(), fmt.Sprintf(`CREATE SEQUENCE public.registration_attempt;
				CREATE FUNCTION public.registration_failure() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN IF nextval('public.registration_attempt') >= %d THEN RAISE EXCEPTION 'registration fixture' USING ERRCODE='P0001'; END IF; RETURN NEW; END $$;
				CREATE TRIGGER registration_failure BEFORE INSERT ON public.index_task FOR EACH ROW EXECUTE FUNCTION public.registration_failure()`, failAt)); err != nil {
					t.Fatal(err)
				}
				before := registrationStateFingerprint(t, database)
				ctx, _ := indexRequestContext(t)
				switch action {
				case "create_derived":
					value := testDerivedContext(t, graph, actor)
					_, err = database.CreateContextWithOperation(ctx, graph, value, []model.ID{seed.ID}, registrationOperation(value, OperationAdd), WriteLimits{})
				case "create_event":
					value := testEventContext(t, graph, actor, seed.ID)
					_, err = database.CreateContextWithOperation(ctx, graph, value, nil, registrationOperation(value, OperationAdd), WriteLimits{})
				case "update":
					value := node
					value.Version++
					value.Body += " 실패할 본문"
					_, err = database.UpdateContextWithOperation(ctx, graph, node.Version, value, registrationOperation(value, OperationUpdate), WriteLimits{})
				case "restore_mcp":
					_, err = database.RestoreContext(ctx, graph, node.ID, registrationOperation(node, OperationUpdate), WriteLimits{})
				case "restore_web":
					_, err = database.SetContextDeleted(ctx, graph, node.ID, actor, false, 0)
				case "reindex":
					err = database.ReindexGraph(ctx, graph)
				}
				failure, ok := errors.AsType[*pgconn.PgError](err)
				if !ok || failure.Code != "P0001" || registrationStateFingerprint(t, database) != before {
					t.Fatalf("등록 실패의 전체 롤백=%v", err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestIndexRegistrationScopeIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	seed, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://registration-scope/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{seed.ID})
	if err != nil {
		t.Fatal(err)
	}
	other := createTestGraph(t, database, actor)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.index_task SET graph_id=$2 WHERE context_id=$1`, node.ID.String(), other.String()); err != nil {
		t.Fatal(err)
	}
	before := registrationStateFingerprint(t, database)
	next := node
	next.Body += " 거부할 갱신"
	next.Version++
	if _, err := database.UpdateContext(t.Context(), graph, node.Version, next); err == nil {
		t.Fatal("다른 그래프의 작업을 덮었다")
	}
	if registrationStateFingerprint(t, database) != before {
		t.Fatal("범위 거부가 정점·작업·그래프를 변경했다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIndexRegistrationConcurrentUpsertIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	node, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://registration-concurrent/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	original := registrationTaskFor(t, database, node.ID).id
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var group sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			for range 10 {
				err := pgx.BeginFunc(ctx, database.pool, func(tx pgx.Tx) error { return database.enqueueIndexTask(ctx, tx, graph, node.ID, node.Layer) })
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("경합 등록=%v", err)
	}
	assertRegistration(t, database, node, original, model.ID{}, time.Time{})
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIndexRegistrationClaimCorrelationIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	database.indexTargets = IndexTargetsWithoutSource
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	seed, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://registration-claim/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, first := indexRequestContext(t)
	node, err := database.CreateContext(firstCtx, graph, testDerivedContext(t, graph, actor), []model.ID{seed.ID})
	if err != nil {
		t.Fatal(err)
	}
	readyIndexTasks(t, database, graph, node.ID)
	secondCtx, second := indexRequestContext(t)
	var updated registrationTask
	result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(_ context.Context, task IndexTask) IndexTaskResult {
		if task.CorrelationID != first.String() {
			t.Fatalf("최초 확보의 상관 ID=%s", task.CorrelationID)
		}
		next := node
		next.Version++
		next.Body += " 새 요청"
		if _, err := database.UpdateContext(secondCtx, graph, node.Version, next); err != nil {
			t.Fatal(err)
		}
		updated = registrationTaskFor(t, database, node.ID)
		return IndexTaskResult{Failure: "무효 확보분의 합성 실패"}
	})
	if err != nil || !result.Found || result.Succeeded || !reflect.DeepEqual(registrationTaskFor(t, database, node.ID), updated) {
		t.Fatalf("이전 확보가 새 등록을 변경했다: %+v, %v", result, err)
	}
	readyIndexTasks(t, database, graph, node.ID)
	result, err = database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(_ context.Context, task IndexTask) IndexTaskResult {
		if task.CorrelationID != second.String() || task.Body != node.Body+" 새 요청" {
			t.Fatalf("재등록 확보의 요청·본문=%+v", task)
		}
		return IndexTaskResult{Embedding: []float64{1, 0, 0, 0, 0, 0, 0, 0}, ModelID: "idx01-fixture"}
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("새 요청 처리=%+v: %v", result, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIndexRegistrationCanceledReindexIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	if _, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://registration-cancel/"+graph.String()), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := database.pool.Exec(t.Context(), `CREATE FUNCTION public.registration_wait() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(30); RETURN NEW; END $$;
		CREATE TRIGGER registration_wait BEFORE INSERT ON public.index_task FOR EACH ROW EXECUTE FUNCTION public.registration_wait()`); err != nil {
		t.Fatal(err)
	}
	before := registrationStateFingerprint(t, database)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := database.ReindexGraph(ctx, graph); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("등록 중 요청 마감=%v", err)
	}
	if registrationStateFingerprint(t, database) != before {
		t.Fatal("취소된 등록이 업무 행을 변경했다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}
