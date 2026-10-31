package store

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

type reindexProgressPlanTracer struct {
	mu   sync.Mutex
	sql  string
	args []any
}

func (tracer *reindexProgressPlanTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH targets AS ") {
		tracer.mu.Lock()
		defer tracer.mu.Unlock()
		tracer.sql, tracer.args = data.SQL, slices.Clone(data.Args)
	}
	return ctx
}
func (*reindexProgressPlanTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (tracer *reindexProgressPlanTracer) query() (string, []any) {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	return tracer.sql, slices.Clone(tracer.args)
}

func TestReindexProgressUUIDJoinPlanIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		for _, size := range []int{1000, 10000} {
			t.Run(fmt.Sprintf("%s/%d", typ, size), func(t *testing.T) {
				database := newEmbeddingContractStore(t, typ, 8)
				actor := newTestIDWithAccount(t, database)
				graph, other := createTestGraph(t, database, actor), createTestGraph(t, database, actor)
				source := lifeAuditCreate(t, database, testSourceContext(t, graph, actor, "api://progress/source"), nil)
				old := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{source.ID})
				current := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{source.ID})
				lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{source.ID})
				foreign := lifeAuditCreate(t, database, testSourceContext(t, other, actor, "api://progress/foreign"), nil)
				for _, node := range []model.Context{source, old, foreign} {
					reindexVector(t, database, node, "previous")
				}
				reindexVector(t, database, current, "current")
				claimExec(t, database, `UPDATE public.context_embedding SET storage_tier='hot' WHERE context_id IN ($1,$2)`, old.ID.String(), foreign.ID.String())
				claimExec(t, database, `UPDATE public.index_task SET state='failed',attempts=5,last_error='fixture' WHERE context_id=$1`, old.ID.String())
				// 보관된 비대상 행을 두 파티션에 놓아 진행률에서 제외되는지 확인한다.
				noise := testSourceContext(t, other, actor, "api://progress/retained")
				at := time.Now().UTC()
				noise.DeletedAt = &at
				properties, err := json.Marshal(contextProperties(noise))
				if err != nil {
					t.Fatal(err)
				}
				claimExec(t, database, `INSERT INTO `+database.contextTable()+` (properties)
      SELECT ($1::jsonb || jsonb_build_object('context_id','019b0198-0000-7000-8000-'||lpad(n::text,12,'0'),
        'source_ref_locator','api://progress/retained/'||n))::text::ag_catalog.agtype FROM generate_series(1,$2::int) n`, string(properties), size)
				claimExec(t, database, `INSERT INTO public.context_embedding (context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
      SELECT (properties->>'context_id'::text)::uuid,$1::uuid,'[0,1,0,0,0,0,0,0]','previous',now(),now(),
        CASE WHEN right(properties->>'context_id'::text,12)::bigint % 2=0 THEN 'cold' ELSE 'hot' END
      FROM `+database.contextTable()+` WHERE properties->>'graph_id'::text=$1::uuid::text AND properties->>'deleted_at'::text IS NOT NULL`, other.String())
				for _, table := range []string{database.contextTable(), "public.context_embedding_hot", "public.context_embedding_cold", "public.context_embedding", "public.index_task"} {
					claimExec(t, database, "ANALYZE "+table)
				}
				tracer := new(reindexProgressPlanTracer)
				configureTierPool(t, database, tracer)
				for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
					database.indexTargets = targets
					for _, scope := range []struct {
						name string
						id   model.ID
					}{{"graph", graph}, {"global", model.ID{}}} {
						t.Run(string(targets)+"/"+scope.name, func(t *testing.T) {
							want := IndexProgress{OldModelEmbeddings: 2, Missing: 3, Pending: 3, Failed: 1}
							if scope.id.IsZero() {
								want.OldModelEmbeddings++
								want.Missing++
								want.Pending++
							}
							if targets == IndexTargetsWithoutSource {
								want.OldModelEmbeddings = 1
								want.Missing = 2
							}
							got, err := database.ReindexProgress(t.Context(), scope.id, "current")
							if err != nil || got != want {
								t.Fatalf("progress=%+v want=%+v: %v", got, want, err)
							}
							sql, args := tracer.query()
							if len(args) == 0 || args[0] != pgx.QueryExecModeExec {
								t.Fatal("progress query reused a cacheable plan mode")
							}
							if sql == "" {
								t.Fatal("actual progress query not captured")
							}
							plan := explainHop(t, database, sql, args...)
							scanned := plan.Root.scanned("context_embedding_hot") + plan.Root.scanned("context_embedding_cold")
							t.Logf("progress=%+v time=%.3fms embeddingRows=%g buffers=%d+%d ContextRows=%g", got, plan.Execution, scanned, plan.Root.Hit, plan.Root.Read, plan.Root.scanned("Context"))
							logHopPlan(t, "progress", plan.Root)
							if !planUsesIndex(plan.Root, "context_embedding_hot_context_idx") || !planUsesIndex(plan.Root, "context_embedding_cold_context_idx") {
								t.Error("typed embedding indexes not used")
							}
							if size >= 10000 && scanned >= float64(size) {
								t.Error("retained embedding rows scanned")
							}
						})
					}
				}
				for _, targetCount := range []int{63, 64, 65} {
					t.Run(fmt.Sprintf("boundary_%d", targetCount), func(t *testing.T) {
						database.indexTargets = IndexTargetsAllLayers
						claimExec(t, database, `UPDATE `+database.contextTable()+` SET properties=((properties::text)::jsonb - 'deleted_at')::text::ag_catalog.agtype
							WHERE properties->>'graph_id'::text=$1 AND properties->>'deleted_at'::text IS NOT NULL
							AND right(properties->>'context_id'::text,12)::bigint <= $2`, other.String(), targetCount-5)
						claimExec(t, database, "ANALYZE "+database.contextTable())
						got, err := database.ReindexProgress(t.Context(), model.ID{}, "current")
						want := IndexProgress{OldModelEmbeddings: targetCount - 2, Missing: targetCount - 1, Pending: 4, Failed: 1}
						if err != nil || got != want {
							t.Fatalf("boundary progress=%+v want=%+v: %v", got, want, err)
						}
						sql, args := tracer.query()
						if len(args) == 0 || args[0] != pgx.QueryExecModeExec {
							t.Fatal("progress query reused a cacheable plan mode")
						}
						plan := explainHop(t, database, sql, args...)
						t.Logf("boundary progress=%+v time=%.3fms embeddingRows=%g buffers=%d+%d", got, plan.Execution,
							plan.Root.scanned("context_embedding_hot")+plan.Root.scanned("context_embedding_cold"), plan.Root.Hit, plan.Root.Read)
						if targetCount <= 64 && (!planUsesIndex(plan.Root, "context_embedding_hot_context_idx") || !planUsesIndex(plan.Root, "context_embedding_cold_context_idx")) {
							t.Error("bounded UUID batch indexes not used")
						}
					})
				}
				t.Run("dense", func(t *testing.T) {
					database.indexTargets = IndexTargetsAllLayers
					claimExec(t, database, `UPDATE `+database.contextTable()+` SET properties=((properties::text)::jsonb - 'deleted_at')::text::ag_catalog.agtype
						WHERE properties->>'graph_id'::text=$1 AND properties->>'deleted_at'::text IS NOT NULL`, other.String())
					claimExec(t, database, "ANALYZE "+database.contextTable())
					got, err := database.ReindexProgress(t.Context(), model.ID{}, "current")
					want := IndexProgress{OldModelEmbeddings: size + 3, Missing: size + 4, Pending: 4, Failed: 1}
					if err != nil || got != want {
						t.Fatalf("dense progress=%+v want=%+v: %v", got, want, err)
					}
					// 1분마다 반복되어도 모든 실행이 캐시되지 않는 경로와 같은 값을 유지한다.
					for range 8 {
						repeated, err := database.ReindexProgress(t.Context(), model.ID{}, "current")
						if err != nil || repeated != want {
							t.Fatalf("repeated=%+v: %v", repeated, err)
						}
					}
					sql, args := tracer.query()
					if len(args) == 0 || args[0] != pgx.QueryExecModeExec {
						t.Fatal("progress query reused a cacheable plan mode")
					}
					plan := explainHop(t, database, sql, args...)
					t.Logf("dense progress=%+v time=%.3fms embeddingRows=%g buffers=%d+%d ContextRows=%g", got, plan.Execution,
						plan.Root.scanned("context_embedding_hot")+plan.Root.scanned("context_embedding_cold"), plan.Root.Hit, plan.Root.Read, plan.Root.scanned("Context"))
				})
				t.Run("empty", func(t *testing.T) {
					requireReindexProgress(t, database, createTestGraph(t, database, actor), "current", IndexProgress{})
				})
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}
