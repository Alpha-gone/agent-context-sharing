package store

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func compareInvariantScopes(t *testing.T, database *Store, graph invariantGraph, scope []model.ID) []InvariantViolation {
	t.Helper()
	audited, err := database.AuditGraph(t.Context(), graph.graphID, graph.expect)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), tx)
	written, err := database.writeInvariantViolations(t.Context(), tx, graph.graphID, scope, graph.expect)
	if err != nil || !reflect.DeepEqual(written, audited) {
		t.Fatalf("쓰기/전체 감사 불일치: %+v / %+v: %v", written, audited, err)
	}
	return audited
}

func TestInvariantStoredCardinalityBoundsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	for _, label := range []string{"DERIVED_FROM", "HAS_MEMBER"} {
		t.Run(label, func(t *testing.T) {
			graph := createInvariantGraph(t, database)
			owner, maximum := graph.derived.ID, 100
			if label == "HAS_MEMBER" {
				owner, maximum = graph.whole.ID, 1000
			}
			addMembers := func(count int) {
				t.Helper()
				properties := make([]string, 0, count)
				for range count {
					value := testSourceContext(t, graph.graphID, graph.source.CreatedBy, "api://data06/"+newTestID(t).String())
					encoded, err := encodeProperties(contextProperties(value))
					if err != nil {
						t.Fatal(err)
					}
					properties = append(properties, encoded)
				}
				query := "MATCH (owner:Context {context_id: " + cypherString(owner.String()) + "}) WITH owner UNWIND [" + strings.Join(properties, ",") + "] AS props CREATE (member:Context) SET member = props CREATE (owner)-[:" + label + " {graph_id: " + cypherString(graph.graphID.String()) + "}]->(member) RETURN count(member)"
				if _, err := database.pool.Exec(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec); err != nil {
					t.Fatal(err)
				}
			}
			addMembers(maximum - 1) // 기본 표본의 참조 하나를 포함한다.
			if got := compareInvariantScopes(t, database, graph, []model.ID{owner}); len(got) != 0 {
				t.Fatalf("기수성 상한 거부: %+v", got)
			}
			addMembers(1)
			got := compareInvariantScopes(t, database, graph, []model.ID{owner})
			want := []InvariantViolation{{Rule: RuleReferenceCardinality, GraphID: graph.graphID.String(), Target: TargetContext, IDs: []string{owner.String()}}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("기수성 초과=%+v", got)
			}
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestInvariantDuplicateEmbeddingIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph := createInvariantGraph(t, database)
	_, err := database.pool.Exec(t.Context(), `INSERT INTO public.context_embedding
		(context_id, graph_id, embedding, model_id, indexed_at, last_accessed_at, storage_tier)
		SELECT context_id, graph_id, embedding, model_id, indexed_at, last_accessed_at, 'cold'
		FROM public.context_embedding WHERE context_id=$1 AND storage_tier='hot'`, graph.source.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	got := compareInvariantScopes(t, database, graph, []model.ID{graph.source.ID})
	want := []InvariantViolation{{Rule: RuleEmbeddingLink, GraphID: graph.graphID.String(), Target: TargetEmbedding, IDs: []string{graph.source.ID.String()}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("중복 임베딩=%+v", got)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestInvariantDanglingEdgeIntegration(t *testing.T) {
	for _, direction := range []string{"outgoing", "incoming"} {
		t.Run(direction, func(t *testing.T) {
			database := newIntegrationStore(t)
			graph := createInvariantGraph(t, database)
			orphan, err := database.CreateContext(t.Context(), graph.graphID, testSourceContext(t, graph.graphID, graph.source.CreatedBy, "api://data06/orphan/"+graph.graphID.String()), nil)
			if err != nil {
				t.Fatal(err)
			}
			from, to := graph.derived.ID, orphan.ID
			if direction == "incoming" {
				from, to = to, from
			}
			corruptEdge(t, database, graph.graphID, "DERIVED_FROM", from, to, "")
			// 간선을 보존한 우회 SQL 삭제를 재현한다. Cypher MATCH만으로는 이 간선을 읽지 못한다.
			table := pgx.Identifier{database.graphName, "Context"}.Sanitize()
			if _, err := database.pool.Exec(t.Context(), "DELETE FROM "+table+" WHERE properties ->> 'context_id'::text=$1", orphan.ID.String()); err != nil {
				t.Fatal(err)
			}
			// 삭제된 정점은 제외하고, 정상 간선의 양 끝을 함께 범위에 넣어도 고아 간선만 한 번 보충한다.
			snapshot := auditSnapshot{graphID: graph.graphID.String(), scope: map[string]struct{}{
				graph.derived.ID.String(): {}, graph.source.ID.String(): {},
			}, contexts: map[string]auditContext{}}
			if err := database.loadAuditContexts(t.Context(), database.pool, "n.context_id IN ["+cypherString(graph.derived.ID.String())+", "+cypherString(graph.source.ID.String())+"]", snapshot.contexts); err != nil {
				t.Fatal(err)
			}
			if err := database.loadDanglingAuditEdges(t.Context(), database.pool, &snapshot); err != nil || len(snapshot.edges) != 1 {
				t.Fatalf("고아 간선 보충=%+v: %v", snapshot.edges, err)
			}
			got := compareInvariantScopes(t, database, graph, []model.ID{graph.derived.ID, graph.source.ID})
			if len(got) == 0 || !slices.ContainsFunc(got, func(value InvariantViolation) bool {
				return value.Rule == RuleEndpointLayer && slices.Contains(value.IDs, graph.derived.ID.String())
			}) {
				t.Fatalf("고아 간선을 놓쳤다: %+v", got)
			}
			line, err := json.Marshal(got)
			if err != nil || strings.Contains(string(line), graph.source.Body) || strings.Contains(string(line), "embedding\":") {
				t.Fatalf("감사 출력에 본문/벡터 노출: %v", err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestInvariantCycleOutsideWriteScopeIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph := createInvariantGraph(t, database)
	nodes := []model.ID{graph.part.ID, graph.whole.ID}
	for range 2 {
		value := testEventContext(t, graph.graphID, graph.source.CreatedBy, graph.source.ID)
		value.Event.Start, value.Event.End = graph.whole.Event.Start, graph.whole.Event.End
		created, err := database.CreateContext(t.Context(), graph.graphID, value, nil)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, created.ID)
	}
	for index := 1; index < len(nodes); index++ {
		corruptEdge(t, database, graph.graphID, "PART_OF", nodes[index], nodes[(index+1)%len(nodes)], newTestID(t).String())
	}
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), tx)
	local, err := database.writeInvariantViolations(t.Context(), tx, graph.graphID, nodes[:1], graph.expect)
	if err != nil || len(local) != 2 {
		t.Fatalf("주변부 밖을 지나는 순환: %+v: %v", local, err)
	}
	audited, err := database.AuditGraph(t.Context(), graph.graphID, graph.expect)
	if err != nil || len(audited) != 4 {
		t.Fatalf("전체 순환: %+v: %v", audited, err)
	}
	for _, value := range local {
		if value.Rule != RuleRelationAcyclic || !slices.ContainsFunc(audited, func(other InvariantViolation) bool { return reflect.DeepEqual(value, other) }) {
			t.Fatalf("순환의 규칙·위치 불일치: %+v", value)
		}
	}
}

func TestInvariantAuditMissingAndCanceledIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	if _, err := database.AuditGraph(t.Context(), newTestID(t), EmbeddingExpectation{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("없는 그래프=%v", err)
	}
	graph := createInvariantGraph(t, database)
	cause := errors.New("감사 요청 취소")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	if _, err := database.AuditGraph(ctx, graph.graphID, graph.expect); !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("감사 취소 원인=%v", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
	ids, err := database.AuditGraphIDs(t.Context())
	if err != nil || !slices.Contains(ids, graph.graphID) {
		t.Fatalf("기본 감사 범위=%v", err)
	}
	// 종료 시각은 검사 결과가 아니며 요청 취소 이후 연결이 반환됐는지 직접 대조한다.
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if _, err := database.AuditGraph(ctx, graph.graphID, graph.expect); err != nil {
		t.Fatal(err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

type cancelAuditQueryTracer struct {
	cancel context.CancelCauseFunc
	cause  error
}

func (tracer cancelAuditQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "RETURN id(e)") {
		tracer.cancel(tracer.cause)
	}
	return ctx
}

func (cancelAuditQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestInvariantAuditTransactionCleanupIntegration(t *testing.T) {
	for _, scenario := range []string{"query_cancel", "rollback_failure"} {
		t.Run(scenario, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph := createInvariantGraph(t, database)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("감사 도중 취소")
			config := database.pool.Config()
			database.pool.Close()
			config.ConnConfig.Tracer = rollbackCancelTracer{}
			if scenario == "query_cancel" {
				config.ConnConfig.Tracer = cancelAuditQueryTracer{cancel: cancel, cause: cause}
			}
			pool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			database.pool = pool
			got, err := database.AuditGraph(ctx, graph.graphID, graph.expect)
			if !errors.Is(err, context.Canceled) || got != nil || scenario == "query_cancel" && !errors.Is(err, cause) {
				t.Fatalf("감사·종료 실패=%+v: %v", got, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
