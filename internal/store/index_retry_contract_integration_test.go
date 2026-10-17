package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB 문장 시각과 직접 대조하여 호스트 시계·시험 실행 지연에 의존하지 않는다.
func TestIndexRetryBudgetIntegration(t *testing.T) {
	for _, fault := range []string{"provider", "lookup", "storage"} {
		t.Run(fault, func(t *testing.T) {
			database, graph, nodes := newIndexClaimFixture(t, 1)
			node := nodes[0]
			other := createIndexScopeGraph(t, database, newTestIDWithAccount(t, database), "retry-isolation")
			otherBefore := retryGraphTasks(t, database, other)
			vector := []float64{1, 0, 0, 0, 0, 0, 0, 0}
			claimExec(t, database, `INSERT INTO public.context_embedding
				(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
				VALUES ($1,$2,$3,'previous',now(),now(),'hot')`, node.ID.String(), graph.String(), vectorText(vector))
			if fault == "storage" {
				claimExec(t, database, `CREATE FUNCTION public.retry_storage_fault() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN RAISE EXCEPTION 'synthetic storage fault' USING ERRCODE='40001'; END $$;
					CREATE TRIGGER retry_storage_fault BEFORE INSERT ON public.context_embedding
					FOR EACH ROW EXECUTE FUNCTION public.retry_storage_fault()`)
			}
			claimExec(t, database, `CREATE TABLE public.retry_clock_sample(marked_at timestamptz);
				CREATE FUNCTION public.retry_clock_sample() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN INSERT INTO public.retry_clock_sample VALUES(statement_timestamp()); RETURN NEW; END $$;
				CREATE TRIGGER retry_clock_sample BEFORE UPDATE ON public.index_task
				FOR EACH ROW WHEN (NEW.attempts <> OLD.attempts) EXECUTE FUNCTION public.retry_clock_sample()`)
			var revision int64
			if err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			for attempt := 1; attempt <= 5; attempt++ {
				calls := 0
				originalGraph := database.graphName
				if fault == "lookup" {
					database.graphName = "idx04_missing_graph"
				}
				result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
					calls++
					if fault == "storage" {
						return IndexTaskResult{Embedding: vector, ModelID: "new-model"}
					}
					return IndexTaskResult{Failure: fmt.Sprintf("synthetic transient %d", attempt), Retryable: true}
				})
				database.graphName = originalGraph
				if !result.Found || result.Succeeded || (fault == "provider" && err != nil) || (fault != "provider" && err == nil) || (fault == "lookup" && calls != 0) || (fault != "lookup" && calls != 1) {
					t.Fatalf("%s/%d result=%+v, calls=%d: %v", fault, attempt, result, calls, err)
				}
				if fault == "storage" {
					if cause, ok := errors.AsType[*pgconn.PgError](err); !ok || cause.Code != "40001" {
						t.Fatalf("일시 저장 장애 주입 실패: %v", err)
					}
				}
				after := registrationTaskFor(t, database, node.ID)
				state, delay := "pending", []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}[attempt-1]
				if attempt == 5 {
					state = "failed"
				}
				wantError := fmt.Sprintf("synthetic transient %d", attempt)
				if fault == "lookup" {
					wantError = "색인 대상 조회 실패"
				} else if fault == "storage" {
					wantError = "색인 결과 저장 실패"
				}
				var marked time.Time
				if err := database.pool.QueryRow(t.Context(), `SELECT max(marked_at) FROM public.retry_clock_sample`).Scan(&marked); err != nil {
					t.Fatal(err)
				}
				if after.attempts != attempt || after.state != state || after.lastError == nil || *after.lastError != wantError || after.next.Sub(marked) != delay {
					t.Fatalf("attempt %d state=%+v, DB delta=%v, want=%v", attempt, after, after.next.Sub(marked), delay)
				}
				if notReady := database.CheckGraphIndexReady(t.Context(), graph, "previous"); notReady == nil || !strings.Contains(notReady.Error(), "최근 실패 사유=") {
					t.Fatalf("대기·실패를 준비 완료로 처리했다: %v", notReady)
				}
				// 대기/소진 모두 즉시 재호출되지 않는다. 이 뒤 자기 작업만 만료시킨다.
				if skipped, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
					t.Fatal("대기·failed 작업을 호출했다")
					return IndexTaskResult{}
				}); err != nil || skipped.Found {
					t.Fatalf("대기 건너뛰기=%+v: %v", skipped, err)
				}
				if attempt < 5 {
					claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second' WHERE context_id=$1`, node.ID.String())
				}
			}
			stored, err := database.Context(t.Context(), graph, node.ID)
			if err != nil || !reflect.DeepEqual(stored, node) {
				t.Fatalf("저장된 원본이 장애로 바뀌었다: %+v, %v", stored, err)
			}
			var afterRevision int64
			var embedding, modelID string
			if err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&afterRevision); err != nil || afterRevision != revision {
				t.Fatalf("내용 판=%d, want=%d: %v", afterRevision, revision, err)
			}
			if err := database.pool.QueryRow(t.Context(), `SELECT embedding::text,model_id FROM public.context_embedding WHERE context_id=$1`, node.ID.String()).Scan(&embedding, &modelID); err != nil || embedding != vectorText(vector) || modelID != "previous" {
				t.Fatalf("기존 임베딩 보존=%s/%s: %v", embedding, modelID, err)
			}
			if retryGraphTasks(t, database, other) != otherBefore || indexTaskCount(t, database, node.ID) != 1 {
				t.Fatal("다른 그래프를 바꾸거나 최종 실패 작업을 지웠다")
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func retryGraphTasks(t *testing.T, database *Store, graph model.ID) string {
	t.Helper()
	var fingerprint string
	if err := database.pool.QueryRow(t.Context(), `SELECT COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY task_id),'') FROM public.index_task t WHERE graph_id=$1`, graph.String()).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func TestIndexPermanentFailureIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		t.Run(typ, func(t *testing.T) {
			for _, test := range []struct {
				name, code string
				result     IndexTaskResult
			}{
				{"provider", "", IndexTaskResult{Failure: "permanent response contract"}},
				{"empty_vector", "", IndexTaskResult{ModelID: "model"}},
				{"missing_model", "", IndexTaskResult{Embedding: []float64{1}}},
				{"blank_model", "", IndexTaskResult{Embedding: []float64{1}, ModelID: " \t\n"}},
				{"dimension", "22000", IndexTaskResult{Embedding: []float64{1, 0, 0, 0, 0, 0, 0, 0, 0}, ModelID: "model"}},
				{"nonfinite", "22000", IndexTaskResult{Embedding: []float64{math.Inf(1), 0, 0, 0, 0, 0, 0, 0}, ModelID: "model"}},
				{"numeric_range", "22003", IndexTaskResult{Embedding: []float64{1e100, 0, 0, 0, 0, 0, 0, 0}, ModelID: "model"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					database := newEmbeddingContractStore(t, typ, 8)
					actor := newTestIDWithAccount(t, database)
					graph := createTestGraph(t, database, actor)
					node, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://permanent/"+graph.String()), nil)
					if err != nil {
						t.Fatal(err)
					}
					claimExec(t, database, `INSERT INTO public.context_embedding
						(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
						VALUES ($1,$2,'[1,0,0,0,0,0,0,0]','previous',now(),now(),'cold')`, node.ID.String(), graph.String())
					before := embeddingStateFingerprint(t, database, graph)
					result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult { return test.result })
					if !result.Found || result.Succeeded || (test.name == "provider" && err != nil) || (test.name != "provider" && err == nil) {
						t.Fatalf("영구 실패=%+v: %v", result, err)
					}
					if test.code != "" {
						if cause, ok := errors.AsType[*pgconn.PgError](err); !ok || cause.Code != test.code {
							t.Fatalf("저장 계약 위반 원인=%v, want SQLSTATE %s", err, test.code)
						}
					}
					after := registrationTaskFor(t, database, node.ID)
					if after.attempts != 1 || after.state != "failed" || after.lastError == nil || *after.lastError == "" {
						t.Fatalf("즉시 failed=%+v", after)
					}
					if test.code != "" && *after.lastError != "임베딩 차원·수치·형식의 저장 계약 위반" {
						t.Fatalf("안전한 저장 실패 사유=%q", *after.lastError)
					}
					if embeddingStateFingerprint(t, database, graph) != before {
						t.Fatal("영구 실패가 기존 벡터·모델·계층·내용 판을 바꿨다")
					}
					if skipped, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
						t.Fatal("영구 실패를 다시 호출했다")
						return IndexTaskResult{}
					}); err != nil || skipped.Found {
						t.Fatalf("failed 건너뛰기=%+v: %v", skipped, err)
					}
					if err := database.CheckGraphIndexReady(t.Context(), graph, "model"); err == nil || !strings.Contains(err.Error(), "failed=1") {
						t.Fatalf("실패 집계=%v", err)
					}
					assertConnectionRecovery(t, database, nil, 0)
				})
			}
		})
	}
}

func TestIndexFailureStaleClaimIntegration(t *testing.T) {
	for _, change := range []string{"reenqueued", "reclaimed", "removed"} {
		t.Run(change, func(t *testing.T) {
			database, graph, nodes := newIndexClaimFixture(t, 1)
			task, claim := requireIndexClaim(t, database, graph, nodes[0].ID)
			switch change {
			case "reenqueued":
				if err := database.ReindexGraph(t.Context(), graph); err != nil {
					t.Fatal(err)
				}
			case "reclaimed":
				claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second' WHERE task_id=$1`, task.ID.String())
				requireIndexClaim(t, database, graph, nodes[0].ID)
			case "removed":
				claimExec(t, database, `DELETE FROM public.index_task WHERE task_id=$1`, task.ID.String())
			}
			before := registrationStateFingerprint(t, database)
			for _, mode := range []string{"transient", "permanent", "exhausted"} {
				t.Run(mode, func(t *testing.T) {
					old := task
					if mode == "exhausted" {
						old.Attempts = 4
					}
					if err := database.storeIndexFailure(t.Context(), old, claim, IndexTaskResult{Failure: "stale", Retryable: mode != "permanent"}); err != nil {
						t.Fatal(err)
					}
					if registrationStateFingerprint(t, database) != before {
						t.Fatal("무효 확보분이 새 상태를 바꿨다")
					}
				})
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestIndexFailureRecordRollbackIntegration(t *testing.T) {
	for _, mode := range []string{"update_error", "commit_error", "deadline", "provider_canceled"} {
		t.Run(mode, func(t *testing.T) {
			database, graph, nodes := newIndexClaimFixture(t, 1)
			if mode != "provider_canceled" {
				body := "RAISE EXCEPTION 'retry fixture' USING ERRCODE='P0001';"
				trigger := "CREATE TRIGGER retry_fault BEFORE UPDATE ON public.index_task FOR EACH ROW WHEN (NEW.attempts <> OLD.attempts) EXECUTE FUNCTION public.retry_fault()"
				if mode == "commit_error" {
					trigger = "CREATE CONSTRAINT TRIGGER retry_fault AFTER UPDATE ON public.index_task DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.attempts <> OLD.attempts) EXECUTE FUNCTION public.retry_fault()"
				} else if mode == "deadline" {
					claimExec(t, database, `CREATE SEQUENCE public.retry_entered`)
					body = "PERFORM nextval('public.retry_entered'); PERFORM pg_sleep(30);"
				}
				claimExec(t, database, fmt.Sprintf(`CREATE FUNCTION public.retry_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN %s RETURN NEW; END $$; %s`, body, trigger))
			}
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			var leased registrationTask
			result, err := database.ProcessNextIndexTaskInGraph(ctx, graph, func(context.Context, IndexTask) IndexTaskResult {
				leased = registrationTaskFor(t, database, nodes[0].ID)
				if mode == "provider_canceled" {
					cancel()
				}
				return IndexTaskResult{Failure: "synthetic transient", Retryable: true}
			})
			if err == nil || !result.Found || result.Succeeded || result.Task.ContextID != nodes[0].ID {
				t.Fatalf("실패 기록 오류의 작업·원인=%+v: %v", result, err)
			}
			if mode == "deadline" || mode == "provider_canceled" {
				want := context.DeadlineExceeded
				if mode == "provider_canceled" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("취소 원인 누락: %v", err)
				}
				if mode == "deadline" {
					var entered bool
					if err := database.pool.QueryRow(t.Context(), `SELECT is_called FROM public.retry_entered`).Scan(&entered); err != nil || !entered {
						t.Fatalf("실제 DB 대기=%t: %v", entered, err)
					}
				}
			} else if cause, ok := errors.AsType[*pgconn.PgError](err); !ok || cause.Code != "P0001" {
				t.Fatalf("DB 원인 누락=%v", err)
			}
			if !reflect.DeepEqual(leased, registrationTaskFor(t, database, nodes[0].ID)) {
				t.Fatal("실패 기록의 부분 반영")
			}
			assertConnectionRecovery(t, database, nil, 0)
			if mode != "provider_canceled" {
				claimExec(t, database, `DROP TRIGGER retry_fault ON public.index_task`)
			}
			claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second' WHERE context_id=$1`, nodes[0].ID.String())
			result, err = database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
				return IndexTaskResult{Failure: "recovered", Retryable: true}
			})
			if err != nil || !result.Found || registrationTaskFor(t, database, nodes[0].ID).attempts != 1 {
				t.Fatalf("재확보 후 실패 기록=%+v: %v", result, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
