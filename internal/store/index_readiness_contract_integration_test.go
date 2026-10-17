package store

import (
	"agent_context_sharing/internal/model"
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndexReadinessStateContractIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
			t.Run(typ+"/"+string(targets), func(t *testing.T) {
				database, graph, source, target := publicationFixture(t, typ)
				database.indexTargets = targets
				for _, node := range []model.Context{source, target} {
					reindexVector(t, database, node, "current")
				}
				claimExec(t, database, `DELETE FROM public.index_task WHERE graph_id=$1`, graph.String())
				if err := database.CheckGraphIndexReady(t.Context(), graph, "current"); err != nil {
					t.Fatal(err)
				}
				claimExec(t, database, `DELETE FROM public.context_embedding WHERE context_id=$1`, target.ID.String())
				if database.CheckGraphIndexReady(t.Context(), graph, "current") == nil {
					t.Fatal("빈 큐에서 미색인을 완료로 판정했다")
				}
				reindexVector(t, database, target, "previous")
				if database.CheckGraphIndexReady(t.Context(), graph, "current") == nil {
					t.Fatal("옛 모델을 현재 모델로 인정했다")
				}
				claimExec(t, database, `UPDATE public.context_embedding SET model_id='current' WHERE context_id=$1`, target.ID.String())
				if err := database.ReindexGraph(t.Context(), graph); err != nil {
					t.Fatal(err)
				}
				claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()+interval '1 day' WHERE graph_id=$1`, graph.String())
				if database.CheckGraphIndexReady(t.Context(), graph, "current") == nil {
					t.Fatal("현재 벡터가 있는 pending을 무시했다")
				}
				claimExec(t, database, `UPDATE public.index_task SET state='failed',attempts=5 WHERE graph_id=$1`, graph.String())
				if database.CheckGraphIndexReady(t.Context(), graph, "current") == nil {
					t.Fatal("failed를 준비 완료로 판정했다")
				}
				claimExec(t, database, `DELETE FROM public.index_task WHERE graph_id=$1`, graph.String())
				if _, err := database.DiscardContext(t.Context(), graph, target.ID, registrationOperation(target, OperationDiscard), WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				claimExec(t, database, `DELETE FROM public.context_embedding WHERE context_id=$1`, target.ID.String())
				if targets == IndexTargetsWithoutSource {
					claimExec(t, database, `DELETE FROM public.context_embedding WHERE context_id=$1`, source.ID.String())
				}
				if err := database.CheckGraphIndexReady(t.Context(), graph, "current"); err != nil {
					t.Fatalf("삭제·제외 대상의 색인을 요구했다: %v", err)
				}
				if _, err := database.RestoreContext(t.Context(), graph, target.ID, registrationOperation(target, OperationUpdate), WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				if database.CheckGraphIndexReady(t.Context(), graph, "current") == nil {
					t.Fatal("복구 뒤 미색인을 준비 완료로 판정했다")
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := database.CheckGraphIndexReady(ctx, graph, "current"); err == nil {
					t.Fatal("취소된 준비 조회를 허용했다")
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

type readinessSQLTracer struct{ calls atomic.Int64 }

func (tracer *readinessSQLTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "WITH tasks AS (") {
		tracer.calls.Add(1)
	}
	return ctx
}
func (*readinessSQLTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestIndexReadinessAtomicPublicationIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		t.Run(typ, func(t *testing.T) {
			database, graph, source, target := publicationFixture(t, typ)
			database.indexTargets = IndexTargetsWithoutSource
			claimExec(t, database, `DELETE FROM public.index_task WHERE context_id=$1`, source.ID.String())
			tracer := &readinessSQLTracer{}
			cfg := database.pool.Config()
			database.pool.Close()
			cfg.ConnConfig.Tracer = tracer
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			database.pool = pool
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
			if err := database.CheckGraphIndexReady(ctx, graph, "current"); err == nil || !strings.Contains(err.Error(), "pending=1") || !strings.Contains(err.Error(), "임베딩 누락=1") {
				t.Fatalf("not ready before commit: %v", err)
			}
			if tracer.calls.Load() != 1 {
				t.Fatal("readiness used multiple SQL reads")
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
			if err := database.CheckGraphIndexReady(ctx, graph, "current"); err != nil {
				t.Fatal(err)
			}
			if tracer.calls.Load() != 2 {
				t.Fatal("readiness used multiple SQL reads")
			}
			lock.Release()
			released = true
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
