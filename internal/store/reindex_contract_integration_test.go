package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func reindexVector(t *testing.T, database *Store, node model.Context, modelID string) {
	t.Helper()
	claimExec(t, database, `INSERT INTO public.context_embedding
		(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
		VALUES ($1,$2,'[1,0,0,0,0,0,0,0]',$3,now(),now(),'cold')`, node.ID.String(), node.GraphID.String(), modelID)
}

func requireReindexProgress(t *testing.T, database *Store, graph model.ID, modelID string, want IndexProgress) {
	t.Helper()
	got, err := database.ReindexProgress(t.Context(), graph, modelID)
	if err != nil || got != want {
		t.Fatalf("재색인 진행=%+v, want=%+v: %v", got, want, err)
	}
}

// 같은 차원의 모델·입력 판 변경을 완전히 다른 좌표계로 취급한다.
func TestReindexModelIsolationIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		for _, previous := range []string{"previous-model:retrieval-v1", "gemini-embedding-2:retrieval-v0"} {
			t.Run(typ+"/"+previous, func(t *testing.T) {
				database := newEmbeddingContractStore(t, typ, 8)
				actor := newTestIDWithAccount(t, database)
				graph, other := createTestGraph(t, database, actor), createTestGraph(t, database, actor)
				current := "gemini:gemini-embedding-2:retrieval-v1:" + typ + ":8"
				source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://reindex/model"), nil)
				if err != nil {
					t.Fatal(err)
				}
				nodes := []model.Context{source}
				for range 2 {
					event, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
					if err != nil {
						t.Fatal(err)
					}
					nodes = append(nodes, event)
				}
				otherNode, err := database.CreateContext(t.Context(), other, testSourceContext(t, other, actor, "api://reindex/other"), nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, node := range nodes {
					reindexVector(t, database, node, previous+":"+typ+":8")
				}
				reindexVector(t, database, otherNode, current)
				claimExec(t, database, `DELETE FROM public.index_task WHERE graph_id=$1`, other.String())
				claimExec(t, database, `UPDATE public.index_task SET state='failed',attempts=5,last_error='fixture' WHERE graph_id=$1`, graph.String())
				oldTask := registrationTaskFor(t, database, source.ID)
				untouched := embeddingStateFingerprint(t, database, other)
				if err := database.ReindexOutdatedEmbeddings(t.Context(), current); err != nil {
					t.Fatal(err)
				}
				if task := registrationTaskFor(t, database, source.ID); task.id != oldTask.id || task.attempts != 0 || task.state != "pending" || task.lastError != nil || task.correlation != nil {
					t.Fatalf("같은 작업의 재등록=%+v", task)
				}
				requireReindexProgress(t, database, graph, current, IndexProgress{3, 3, 3, 0})
				requireReindexProgress(t, database, other, current, IndexProgress{})
				vector := []float64{1, 0, 0, 0, 0, 0, 0, 0}
				candidates, err := database.SemanticCandidates(t.Context(), graph, current, vector, time.Now().UTC(), 10)
				if err != nil || len(candidates) != 0 {
					t.Fatalf("옛 좌표계 검색=%d: %v", len(candidates), err)
				}
				database.relationProposals = &RelationProposalConfig{SimilarityThreshold: .9, Limit: 10}
				assertRelations := func(want int) {
					t.Helper()
					if err := database.ProposeSimilarEventRelations(t.Context(), graph, nodes[1].ID, current); err != nil {
						t.Fatal(err)
					}
					relations, _, err := database.ListContextRelations(t.Context(), graph, nodes[1].ID, nil, []model.RelationType{model.RelationTypeRelatesTo}, "", 10)
					if err != nil || len(relations) != want {
						t.Fatalf("현재 모델 사건 관계=%d want=%d: %v", len(relations), want, err)
					}
				}
				assertRelations(0)
				for i, node := range nodes {
					readyIndexTasks(t, database, graph, node.ID)
					result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
						return IndexTaskResult{Embedding: vector, ModelID: current}
					})
					if err != nil || !result.Succeeded || result.Task.ContextID != node.ID {
						t.Fatalf("현재 모델 공개=%+v: %v", result, err)
					}
					if i == 1 {
						assertRelations(0)
					} // 대상만 현재 모델이면 옛 후보는 배제한다.
				}
				assertRelations(1)
				requireReindexProgress(t, database, graph, current, IndexProgress{})
				candidates, err = database.SemanticCandidates(t.Context(), graph, current, vector, time.Now().UTC(), 10)
				if err != nil || len(candidates) != 3 {
					t.Fatalf("새 모델 검색=%d: %v", len(candidates), err)
				}
				if embeddingStateFingerprint(t, database, other) != untouched {
					t.Fatal("다른 그래프를 변경했다")
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestReindexTargetsRestoreAndProgressIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
			t.Run(typ+"/"+string(targets), func(t *testing.T) {
				database, graph, source, derived := publicationFixture(t, typ)
				deleted, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, source.CreatedBy), []model.ID{source.ID})
				if err != nil {
					t.Fatal(err)
				}
				for _, node := range []model.Context{source, derived, deleted} {
					reindexVector(t, database, node, "current")
				}
				if _, err := database.DiscardContext(t.Context(), graph, deleted.ID, registrationOperation(deleted, OperationDiscard), WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				// 폐기 이전의 pending도 제공자에게 보내지 않고 같은 확보 세대만 제거한다.
				readyIndexTasks(t, database, graph, deleted.ID)
				calls := 0
				result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult { calls++; return IndexTaskResult{} })
				if err != nil || !result.Found || result.Succeeded || calls != 0 {
					t.Fatalf("삭제 대상 호출=%d: %v", calls, err)
				}
				claimExec(t, database, `DELETE FROM public.index_task WHERE graph_id=$1`, graph.String())
				database.indexTargets = targets
				if err := database.ReindexGraph(t.Context(), graph); err != nil {
					t.Fatal(err)
				}
				wantTasks := 2
				if targets == IndexTargetsWithoutSource {
					wantTasks = 1
				}
				requireReindexProgress(t, database, graph, "current", IndexProgress{Pending: wantTasks})
				if indexTaskCount(t, database, deleted.ID) != 0 {
					t.Fatal("삭제 컨텍스트를 재등록했다")
				}
				stored, err := database.Context(t.Context(), graph, derived.ID)
				if err != nil || !reflect.DeepEqual(stored.Derived.DerivedFrom, []model.ID{source.ID}) {
					t.Fatalf("근거 보존: %v", err)
				}
				claimExec(t, database, `DELETE FROM public.context_embedding WHERE graph_id=$1`, graph.String())
				claimExec(t, database, `UPDATE public.index_task SET state='failed',attempts=5 WHERE context_id=$1`, derived.ID.String())
				// 차원 전환처럼 옛 벡터가 없어도 현재 모델 누락과 실패가 남는다.
				requireReindexProgress(t, database, graph, "current", IndexProgress{Missing: wantTasks, Pending: wantTasks - 1, Failed: 1})
				if _, err := database.RestoreContext(t.Context(), graph, deleted.ID, registrationOperation(deleted, OperationUpdate), WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				if indexTaskCount(t, database, deleted.ID) != 1 {
					t.Fatal("복구한 현재 대상의 색인 작업이 없다")
				}
				requireReindexProgress(t, database, graph, "current", IndexProgress{Missing: wantTasks + 1, Pending: wantTasks, Failed: 1})
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestReindexExcludedRollbackIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		for _, stage := range []string{"delete", "commit"} {
			t.Run(typ+"/"+stage, func(t *testing.T) {
				database, graph, source, _ := publicationFixture(t, typ)
				reindexVector(t, database, source, "current")
				database.indexTargets = IndexTargetsWithoutSource
				trigger := `CREATE TRIGGER reindex_fault BEFORE DELETE ON public.context_embedding_cold FOR EACH ROW EXECUTE FUNCTION public.reindex_fault()`
				if stage == "commit" {
					trigger = `CREATE CONSTRAINT TRIGGER reindex_fault AFTER DELETE ON public.context_embedding_cold DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.reindex_fault()`
				}
				claimExec(t, database, `CREATE FUNCTION public.reindex_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic reindex failure' USING ERRCODE='40001'; END $$; `+trigger)
				before := registrationStateFingerprint(t, database)
				err := database.ReindexGraph(t.Context(), graph)
				cause, ok := errors.AsType[*pgconn.PgError](err)
				if !ok || cause.Code != "40001" || registrationStateFingerprint(t, database) != before {
					t.Fatalf("재색인 전체 롤백: %v", err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestReindexExcludedStartupAndStaleClaimIntegration(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			database, graph, source, _ := publicationFixture(t, "vector")
			reindexVector(t, database, source, "current")
			readyIndexTasks(t, database, graph, source.ID)
			result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(ctx context.Context, task IndexTask) IndexTaskResult {
				excluded := *database
				excluded.indexTargets = IndexTargetsWithoutSource
				// 모델은 그대로인 계층 전환도 자동 재색인 경로가 인식한다.
				if err := excluded.ReindexOutdatedEmbeddings(ctx, "current"); err != nil {
					t.Fatal(err)
				}
				if failure {
					return IndexTaskResult{Failure: "stale", Retryable: true}
				}
				return IndexTaskResult{Embedding: []float64{1, 0, 0, 0, 0, 0, 0, 0}, ModelID: "current"}
			})
			if err != nil || !result.Found || result.Succeeded || indexTaskCount(t, database, source.ID) != 0 || embeddingCount(t, database, graph) != 0 {
				t.Fatalf("제외 뒤 이전 결과=%+v: %v", result, err)
			}
			revision := publicationRevision(t, database, graph)
			if err := database.ReindexGraph(t.Context(), graph); err != nil {
				t.Fatal(err)
			}
			if publicationRevision(t, database, graph) != revision {
				t.Fatal("제거할 벡터 없는 재등록이 내용 판을 바꿨다")
			}
			// 원천 재포함은 모델 식별자가 그대로여도 누락을 찾아 다시 등록한다.
			if err := database.ReindexOutdatedEmbeddings(t.Context(), "current"); err != nil {
				t.Fatal(err)
			}
			if indexTaskCount(t, database, source.ID) != 1 {
				t.Fatal("재포함한 원천의 작업이 없다")
			}
		})
	}
}

// 기존 전환 SQL 표본을 새 번호로 적용한다. 배포 이력·SQL은 변경하지 않는다.
func TestReindexShapeTransitionIntegration(t *testing.T) {
	for _, cfg := range []struct {
		from, to  string
		dimension int
	}{
		{"vector", "vector", 16}, {"vector", "halfvec", 8}, {"halfvec", "vector", 16},
	} {
		for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
			t.Run(fmt.Sprintf("%s_to_%s%d/%s", cfg.from, cfg.to, cfg.dimension, targets), func(t *testing.T) {
				database, graph, source, derived := publicationFixture(t, cfg.from)
				nodes := []model.Context{source, derived}
				for range 2 {
					event, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, source.CreatedBy, source.ID), nil)
					if err != nil {
						t.Fatal(err)
					}
					nodes = append(nodes, event)
				}
				if _, err := database.CreateRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypeRelatesTo, nodes[2].ID, nodes[3].ID)); err != nil {
					t.Fatal(err)
				}
				deleted, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, source.CreatedBy), []model.ID{source.ID})
				if err != nil {
					t.Fatal(err)
				}
				for _, node := range append(nodes, deleted) {
					reindexVector(t, database, node, "previous")
				}
				claimExec(t, database, `DELETE FROM public.index_task WHERE graph_id=$1`, graph.String())
				if _, err := database.DiscardContext(t.Context(), graph, deleted.ID, registrationOperation(deleted, OperationDiscard), WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				var originals string
				originalSQL := `SELECT md5((SELECT string_agg(row_to_json(n)::text,'' ORDER BY id) FROM ` + database.contextTable() + ` n)
					|| COALESCE((SELECT string_agg(row_to_json(e)::text,'' ORDER BY id) FROM agent_context._ag_label_edge e),'')
					|| COALESCE((SELECT string_agg(row_to_json(o)::text,'' ORDER BY operation_id) FROM public.operation_log o),''))`
				if err := database.pool.QueryRow(t.Context(), originalSQL).Scan(&originals); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile("../migrate/testdata/007_embedding_dimensions.sql")
				if err != nil {
					t.Fatal(err)
				}
				files := fstest.MapFS{"012_idx07_transition.sql": {Data: data}}
				target := migrate.Config{GraphName: "agent_context", VectorType: cfg.to, VectorDim: cfg.dimension, IndexTargetLayers: string(targets)}
				loaded, err := migrate.Load(files, ".", target)
				if err != nil {
					t.Fatal(err)
				}
				before := registrationStateFingerprint(t, database)
				err = migrate.WithLock(t.Context(), database.pool, func(ctx context.Context, conn *pgxpool.Conn) error {
					broken := loaded[0]
					broken.Rendered += "; SELECT 1/0;"
					if err := migrate.Apply(ctx, conn, broken); err == nil {
						return fmt.Errorf("전환 장애 주입이 성공했다")
					}
					if registrationStateFingerprint(t, database) != before {
						return fmt.Errorf("전환 실패가 원본·벡터·큐·내용 판을 바꿨다")
					}
					if err := database.CheckEmbeddingSchema(ctx, cfg.from, 8); err != nil {
						return err
					}
					if err := migrate.Apply(ctx, conn, loaded[0]); err != nil {
						return err
					}
					applied, err := migrate.AppliedVersions(ctx, conn)
					if err != nil {
						return err
					}
					pending, err := migrate.Pending(loaded, applied)
					if err != nil || len(pending) != 0 {
						return fmt.Errorf("전환 반복 실행=%d: %w", len(pending), err)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := database.CheckEmbeddingSchema(t.Context(), cfg.to, cfg.dimension); err != nil {
					t.Fatal(err)
				}
				if err := database.CheckEmbeddingSchema(t.Context(), cfg.from, 8); err == nil {
					t.Fatal("옛 형상 구성을 수용했다")
				}
				var after string
				if err := database.pool.QueryRow(t.Context(), originalSQL).Scan(&after); err != nil || after != originals {
					t.Fatalf("원본 정점·간선 보존: %v", err)
				}
				if embeddingCount(t, database, graph) != 0 {
					t.Fatal("호환되지 않는 벡터를 보존했다")
				}
				database.indexTargets = targets
				want := 4
				if targets == IndexTargetsWithoutSource {
					want = 3
				}
				current := fmt.Sprintf("current:%s:%d", cfg.to, cfg.dimension)
				requireReindexProgress(t, database, graph, current, IndexProgress{Missing: want, Pending: want})
				if indexTaskCount(t, database, deleted.ID) != 0 {
					t.Fatal("차원 전환이 삭제된 컨텍스트를 등록했다")
				}
				if _, err := database.RestoreContext(t.Context(), graph, deleted.ID, registrationOperation(deleted, OperationUpdate), WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				requireReindexProgress(t, database, graph, current, IndexProgress{Missing: want + 1, Pending: want + 1})
				vector := make([]float64, cfg.dimension)
				vector[0] = 1
				claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second' WHERE graph_id=$1`, graph.String())
				for range want + 1 {
					result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
						return IndexTaskResult{Embedding: vector, ModelID: current}
					})
					if err != nil || !result.Succeeded {
						t.Fatalf("새 형상 공개=%+v: %v", result, err)
					}
				}
				requireReindexProgress(t, database, graph, current, IndexProgress{})
				var plan string
				rows, err := database.pool.Query(t.Context(), `SELECT indexdef FROM pg_indexes WHERE tablename IN ('context_embedding_hot','context_embedding_cold') AND indexdef LIKE '%hnsw%'`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				count := 0
				for rows.Next() {
					if err := rows.Scan(&plan); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(plan, cfg.to+"_cosine_ops") {
						t.Fatal("HNSW 연산자 형상이 다르다")
					}
					count++
				}
				if err := rows.Err(); err != nil || count != 2 {
					t.Fatalf("양쪽 HNSW=%d: %v", count, err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestReindexProgressScopeAndCancellationIntegration(t *testing.T) {
	database, graph, source, derived := publicationFixture(t, "vector")
	other := createTestGraph(t, database, source.CreatedBy)
	otherNode, err := database.CreateContext(t.Context(), other, testSourceContext(t, other, source.CreatedBy, "api://progress/other"), nil)
	if err != nil {
		t.Fatal(err)
	}
	reindexVector(t, database, source, "current")
	reindexVector(t, database, derived, "previous")
	reindexVector(t, database, otherNode, "previous")
	claimExec(t, database, `UPDATE public.index_task SET state='failed',attempts=5 WHERE context_id=ANY($1::uuid[])`, []string{derived.ID.String(), otherNode.ID.String()})
	claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()+interval '1 day' WHERE context_id=$1`, source.ID.String())
	before := registrationStateFingerprint(t, database)
	requireReindexProgress(t, database, graph, "current", IndexProgress{1, 1, 1, 1})
	requireReindexProgress(t, database, other, "current", IndexProgress{1, 1, 0, 1})
	requireReindexProgress(t, database, model.ID{}, "current", IndexProgress{2, 2, 1, 2})
	if registrationStateFingerprint(t, database) != before {
		t.Fatal("진행 조회가 데이터를 변경했다")
	}
	// 새 벡터가 전혀 없어도 이미 실패한 작업은 기동만으로 다시 pending이 되지 않는다.
	claimExec(t, database, `DELETE FROM public.context_embedding`)
	before = registrationStateFingerprint(t, database)
	if err := database.ReindexOutdatedEmbeddings(t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	if registrationStateFingerprint(t, database) != before {
		t.Fatal("기동이 기존 리스·실패 예산을 초기화했다")
	}
	if _, err := database.ReindexProgress(t.Context(), graph, "  "); err == nil {
		t.Fatal("빈 모델을 수용했다")
	}
	if err := database.ReindexOutdatedEmbeddings(t.Context(), " "); err == nil {
		t.Fatal("빈 모델의 재등록을 수용했다")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := database.ReindexProgress(ctx, graph, "current"); !errors.Is(err, context.Canceled) {
		t.Fatalf("진행 조회 취소=%v", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}
