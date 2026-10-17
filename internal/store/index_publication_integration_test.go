package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

type publicationReply struct {
	result IndexProcessResult
	err    error
}

func publicationFixture(t *testing.T, typ string) (*Store, model.ID, model.Context, model.Context) {
	t.Helper()
	database := newEmbeddingContractStore(t, typ, 8)
	actor := newTestIDWithAccount(t, database)
	graph := createTestGraph(t, database, actor)
	source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://publication/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	return database, graph, source, derived
}

func seedPublication(t *testing.T, database *Store, graph model.ID, node model.Context) {
	t.Helper()
	claimExec(t, database, `INSERT INTO public.context_embedding
		(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
		VALUES ($1,$2,'[1,0,0,0,0,0,0,0]','previous','2000-01-01','2000-01-01','cold')`, node.ID.String(), graph.String())
	readyIndexTasks(t, database, graph, node.ID)
}

func publicationRevision(t *testing.T, database *Store, graph model.ID) int64 {
	t.Helper()
	var revision int64
	if err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func requirePublished(t *testing.T, database *Store, node model.Context, revision int64) {
	t.Helper()
	var rows, hot int
	var vector, modelID string
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE storage_tier='hot'),
		min(embedding::text),min(model_id) FROM public.context_embedding WHERE context_id=$1`, node.ID.String()).Scan(&rows, &hot, &vector, &modelID); err != nil || rows != 1 || hot != 1 || vector != "[0,1,0,0,0,0,0,0]" || modelID != "current" {
		t.Fatalf("공개 결과 rows=%d hot=%d vector=%s model=%s: %v", rows, hot, vector, modelID, err)
	}
	if indexTaskCount(t, database, node.ID) != 0 || publicationRevision(t, database, node.GraphID) != revision+1 {
		t.Fatal("작업 삭제·벡터 교체·내용 판 증가가 함께 반영되지 않았다")
	}
}

// 제공자에서 대기한 작업자와 새 세대 작업자를 실제로 겹쳐 실행한다.
func TestIndexPublicationGenerationIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		for _, mode := range []string{"reenqueue_pending", "reenqueue_completed", "reclaim_pending", "reclaim_completed", "excluded", "removed"} {
			for _, failure := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/failure=%t", typ, mode, failure), func(t *testing.T) {
					database, graph, source, target := publicationFixture(t, typ)
					if mode == "excluded" {
						seedPublication(t, database, graph, target)
						target = source
					}
					seedPublication(t, database, graph, target)
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					entered, release := make(chan IndexTask, 1), make(chan struct{})
					done := make(chan publicationReply, 1)
					defer func() { cancel(); <-done }()
					go func() {
						result, err := database.ProcessNextIndexTaskInGraph(ctx, graph, func(ctx context.Context, task IndexTask) IndexTaskResult {
							entered <- task
							select {
							case <-release:
							case <-ctx.Done():
								return IndexTaskResult{Failure: "canceled", Retryable: true}
							}
							if failure {
								return IndexTaskResult{Failure: "stale failure", Retryable: true}
							}
							return IndexTaskResult{Embedding: []float64{0, 0, 1, 0, 0, 0, 0, 0}, ModelID: "stale"}
						})
						done <- publicationReply{result, err}
					}()
					var old IndexTask
					select {
					case old = <-entered:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if old.Body != target.Body || old.Layer != target.Layer {
						t.Fatalf("이전 세대 입력=%+v", old)
					}
					switch {
					case strings.HasPrefix(mode, "reenqueue"):
						next := target
						next.Body, next.Version = "new publication body", target.Version+1
						if _, err := database.UpdateContext(ctx, graph, target.Version, next); err != nil {
							t.Fatal(err)
						}
					case strings.HasPrefix(mode, "reclaim"):
						claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second' WHERE task_id=$1`, old.ID.String())
					case mode == "excluded":
						// 두 작업자는 각자 구성을 가지며 실행 중인 저장소를 변경하지 않는다.
						other := *database
						other.indexTargets = IndexTargetsWithoutSource
						if err := other.ReindexOutdatedEmbeddings(ctx, "current"); err != nil {
							t.Fatal(err)
						}
					case mode == "removed":
						claimExec(t, database, `DELETE FROM public.index_task WHERE task_id=$1`, old.ID.String())
					}
					if strings.Contains(mode, "pending") || strings.Contains(mode, "completed") {
						readyIndexTasks(t, database, graph, target.ID)
						newer, err := database.ProcessNextIndexTaskInGraph(ctx, graph, func(_ context.Context, task IndexTask) IndexTaskResult {
							wantBody := target.Body
							if strings.HasPrefix(mode, "reenqueue") {
								wantBody = "new publication body"
							}
							if task.ID != old.ID || task.Body != wantBody {
								t.Errorf("새 세대 입력=%+v", task)
							}
							if strings.Contains(mode, "pending") {
								return IndexTaskResult{Failure: "new retry", Retryable: true}
							}
							return IndexTaskResult{Embedding: []float64{0, 1, 0, 0, 0, 0, 0, 0}, ModelID: "current"}
						})
						if err != nil || !newer.Found || newer.Succeeded != strings.Contains(mode, "completed") {
							t.Fatalf("새 작업자=%+v: %v", newer, err)
						}
					}
					before := embeddingStateFingerprint(t, database, graph)
					tasks := retryGraphTasks(t, database, graph)
					close(release)
					select {
					case reply := <-done:
						// defer에서도 작업자 종료를 확인할 수 있도록 결과를 돌려놓는다.
						done <- reply
						if reply.err != nil || !reply.result.Found || reply.result.Succeeded {
							t.Fatalf("이전 작업자=%+v: %v", reply.result, reply.err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if embeddingStateFingerprint(t, database, graph) != before || !reflect.DeepEqual(retryGraphTasks(t, database, graph), tasks) {
						t.Fatal("이전 결과가 새 벡터·내용 판·작업 상태를 변경했다")
					}
					if mode == "excluded" && indexTaskCount(t, database, target.ID) != 0 {
						t.Fatal("제외 계층 작업이 되살아났다")
					}
					if mode == "excluded" {
						var count int
						if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM public.context_embedding WHERE context_id=$1`, target.ID.String()).Scan(&count); err != nil || count != 0 {
							t.Fatalf("제외 계층 벡터가 되살아났다: %d, %v", count, err)
						}
					}
					assertConnectionRecovery(t, database, nil, 0)
				})
			}
		}
	}
}

func TestIndexPublicationAtomicVisibilityIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		t.Run(typ, func(t *testing.T) {
			database, graph, _, target := publicationFixture(t, typ)
			seedPublication(t, database, graph, target)
			revision := publicationRevision(t, database, graph)
			before := embeddingStateFingerprint(t, database, graph)
			claimExec(t, database, `CREATE FUNCTION public.publication_wait() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_advisory_xact_lock(5051); RETURN NEW; END $$;
				CREATE TRIGGER publication_wait BEFORE INSERT ON public.context_embedding
				FOR EACH ROW EXECUTE FUNCTION public.publication_wait()`)
			lock, err := database.pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			released := false
			defer func() {
				if !released {
					cleanup, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
					defer stop()
					lock.Exec(cleanup, `SELECT pg_advisory_unlock_all()`)
					lock.Release()
				}
			}()
			if _, err := lock.Exec(t.Context(), `SELECT pg_advisory_lock(5051)`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan publicationReply, 1)
			go func() {
				result, err := database.ProcessNextIndexTaskInGraph(ctx, graph, func(context.Context, IndexTask) IndexTaskResult {
					return IndexTaskResult{Embedding: []float64{0, 1, 0, 0, 0, 0, 0, 0}, ModelID: "current"}
				})
				done <- publicationReply{result, err}
			}()
			defer func() { cancel(); <-done }()
			for {
				var waiting bool
				if err := database.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=5051 AND NOT granted)`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			// INSERT 대기 지점은 같은 트랜잭션의 작업 삭제·기존 벡터 삭제 뒤다.
			if indexTaskCount(t, database, target.ID) != 1 || embeddingStateFingerprint(t, database, graph) != before {
				t.Fatal("커밋 전에 작업 삭제·벡터 교체 일부가 공개됐다")
			}
			if _, err := lock.Exec(ctx, `SELECT pg_advisory_unlock(5051)`); err != nil {
				t.Fatal(err)
			}
			select {
			case reply := <-done:
				done <- reply
				if reply.err != nil || !reply.result.Succeeded {
					t.Fatalf("공개=%+v: %v", reply.result, reply.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			requirePublished(t, database, target, revision)
			lock.Release()
			released = true
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestIndexPublicationRollbackIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		for _, stage := range []string{"revision", "commit"} {
			t.Run(typ+"/"+stage, func(t *testing.T) {
				database, graph, _, target := publicationFixture(t, typ)
				seedPublication(t, database, graph, target)
				before := embeddingStateFingerprint(t, database, graph)
				trigger := `CREATE TRIGGER publication_fault BEFORE UPDATE ON public.context_graph FOR EACH ROW EXECUTE FUNCTION public.publication_fault()`
				if stage == "commit" {
					trigger = `CREATE CONSTRAINT TRIGGER publication_fault AFTER INSERT ON public.context_embedding_hot DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.publication_fault()`
				}
				claimExec(t, database, `CREATE FUNCTION public.publication_fault() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN RAISE EXCEPTION 'synthetic publication failure' USING ERRCODE='40001'; END $$; `+trigger)
				result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
					return IndexTaskResult{Embedding: []float64{0, 1, 0, 0, 0, 0, 0, 0}, ModelID: "current"}
				})
				cause, ok := errors.AsType[*pgconn.PgError](err)
				if !ok || cause.Code != "40001" || !result.Found || result.Succeeded {
					t.Fatalf("장애 원인·결과=%+v: %v", result, err)
				}
				task := registrationTaskFor(t, database, target.ID)
				if task.state != "pending" || task.attempts != 1 || embeddingStateFingerprint(t, database, graph) != before {
					t.Fatal("공개 실패가 작업·기존 벡터·내용 판을 부분 반영했다")
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}
