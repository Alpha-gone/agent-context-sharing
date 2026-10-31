package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 출처 해석만 읽은 조상은 검색 후보·응답 조회로 취급하지 않는다.
func TestOriginLookupPreservesAncestorEmbeddingAccessIntegration(t *testing.T) {
	for _, vectorType := range []string{"vector", "halfvec"} {
		for _, mode := range []string{"origin", "flow", "snapshot_flow"} {
			t.Run(vectorType+"/"+mode, func(t *testing.T) {
				database := newEmbeddingContractStore(t, vectorType, 8)
				actor := newTestID(t)
				createTestAccount(t, database, actor)
				graph := createTestGraph(t, database, actor)
				source := lifeAuditCreate(t, database, testSourceContext(t, graph, actor, "api://origin-access/"+graph.String()), nil)
				first := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{source.ID})
				member := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{first.ID})
				event := testEventContext(t, graph, actor, member.ID)
				event.Event.Start = member.RecordedAt
				event.Event.End = new(member.RecordedAt.Add(time.Minute))
				event.Body = "originaccessneedle"
				event.RecordedAt = member.RecordedAt.Add(time.Second)
				returned := lifeAuditCreate(t, database, event, nil)
				nodes := []model.Context{source, first, member, returned}
				ids := make([]model.ID, 0, len(nodes))
				for _, node := range nodes {
					ids = append(ids, node.ID)
				}
				readyIndexTasks(t, database, graph, ids...)
				for range nodes {
					result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(_ context.Context, task IndexTask) IndexTaskResult {
						vector := []float64{0, 1, 0, 0, 0, 0, 0, 0}
						if task.ContextID == returned.ID {
							vector = []float64{1, 0, 0, 0, 0, 0, 0, 0}
						}
						return IndexTaskResult{Embedding: vector, ModelID: "origin-access"}
					})
					if err != nil || !result.Succeeded {
						t.Fatalf("index=%+v, %v", result, err)
					}
				}
				old := time.Now().UTC().Add(-31 * 24 * time.Hour).Truncate(time.Microsecond)
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET last_accessed_at=$2, storage_tier=CASE WHEN context_id=$3 THEN 'hot' ELSE 'cold' END WHERE graph_id=$1`, graph.String(), old, first.ID.String()); err != nil {
					t.Fatal(err)
				}
				assertAccess := func(id model.ID, wantTier string, wantOld bool) {
					t.Helper()
					var at time.Time
					var tier string
					if err := database.pool.QueryRow(t.Context(), `SELECT last_accessed_at,storage_tier FROM public.context_embedding WHERE graph_id=$1 AND context_id=$2`, graph.String(), id.String()).Scan(&at, &tier); err != nil {
						t.Fatal(err)
					}
					if tier != wantTier || wantOld && !at.Equal(old) || !wantOld && !at.After(old) {
						t.Errorf("%s: access=%v tier=%s, wantOld=%t tier=%s", id, at, tier, wantOld, wantTier)
					}
				}
				wantOrigins := []model.OriginKind{source.Source.OriginKind}
				if mode == "origin" {
					origins, err := database.ContextOriginKinds(t.Context(), graph, []model.ID{returned.ID, member.ID})
					if err != nil || (!slices.Equal(origins[returned.ID], wantOrigins) || !slices.Equal(origins[member.ID], wantOrigins)) {
						t.Fatalf("origins=%v, %v", origins, err)
					}
					assertAccess(returned.ID, "cold", true)
				} else {
					consistency := search.Consistency("")
					if mode == "snapshot_flow" {
						consistency = search.ConsistencySnapshot
					}
					service, err := search.New(database, originAccessEmbedder{}, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 1, SemanticThreshold: .6, FoldThreshold: .95, Consistency: consistency}, nil)
					if err != nil {
						t.Fatal(err)
					}
					flow, err := service.Flow(t.Context(), search.Input{GraphID: graph, WorkContext: returned.Body, AsOf: time.Now().UTC(), Scope: "local", Budget: 1000})
					if err != nil {
						t.Fatal(err)
					}
					if len(flow.Contexts) != 1 || flow.Contexts[0].Value.ID != returned.ID || !slices.Equal(flow.Contexts[0].OriginKinds, wantOrigins) {
						t.Fatalf("flow=%+v", flow)
					}
					for _, name := range []string{"semantic", "keyword", "time"} {
						channel := flow.Channels[name]
						if channel.Failure != "" || channel.Candidates != 1 {
							t.Errorf("channel %s: %+v", name, channel)
						}
					}
					assertAccess(returned.ID, "hot", false)
				}
				assertAccess(source.ID, "cold", true)
				assertAccess(first.ID, "hot", true)
				assertAccess(member.ID, "cold", true)
				// 공개 데이터 조회는 기존 접근 기록과 cold→hot 승격을 유지한다.
				values, err := database.ContextsByIDs(t.Context(), graph, []model.ID{returned.ID})
				if err != nil || len(values) != 1 || values[0].ID != returned.ID {
					t.Fatalf("returned=%v,%v", values, err)
				}
				assertAccess(returned.ID, "hot", false)
			})
		}
	}
}

type originAccessEmbedder struct{}

func (originAccessEmbedder) Embed(context.Context, string) ([]float64, error) {
	return []float64{1, 0, 0, 0, 0, 0, 0, 0}, nil
}
func (originAccessEmbedder) ModelID() string { return "origin-access" }

// 내부 조회로 바꿔도 기존 드라이버 장애 타입·원인 및 상세 비노출을 유지한다.
func TestOriginLookupPreservesStorageFailure(t *testing.T) {
	for _, cause := range []error{&pgconn.PgError{Code: "XX000", Message: "private origin query payload"}, context.Canceled} {
		ctx := context.WithValue(t.Context(), readTxKey{}, originAccessFailureTx{cause: cause})
		database := &Store{}
		origins, err := database.ContextOriginKinds(ctx, newTestID(t), []model.ID{newTestID(t)})
		if _, ok := errors.AsType[*StorageError](err); !ok || origins != nil || !errors.Is(err, cause) || strings.Contains(err.Error(), "private origin query payload") {
			t.Fatalf("origins=%v error=%v", origins, err)
		}
	}
}

type originAccessFailureTx struct {
	pgx.Tx
	cause error
}

func (tx originAccessFailureTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, tx.cause
}
