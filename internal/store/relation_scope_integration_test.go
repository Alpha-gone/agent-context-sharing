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

type relationPlanTx struct {
	pgx.Tx
	queries []string
	args    [][]any
}

type relationScopeTracer struct {
	mu      sync.Mutex
	queries []string
	args    [][]any
}

func (tracer *relationScopeTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	tracer.queries = append(tracer.queries, data.SQL)
	tracer.args = append(tracer.args, slices.Clone(data.Args))
	return ctx
}
func (*relationScopeTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRelationProposalInputBoundIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, actor, member := lifeProposalFixture(t, database)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	end := start.Add(time.Minute)
	template := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, start, &end)
	for offset := 0; offset < 1005; offset += 100 {
		var properties []string
		for range min(100, 1005-offset) {
			node := template
			node.ID = newTestID(t)
			node.Body = strings.Repeat("사건 후보 입력 ", 100)
			encoded, err := encodeProperties(contextProperties(node))
			if err != nil {
				t.Fatal(err)
			}
			properties = append(properties, encoded)
		}
		query := "MATCH (member:Context) WHERE member.context_id = " + cypherString(member.ID.String()) +
			" WITH member UNWIND [" + strings.Join(properties, ",") + "] AS props CREATE (event:Context) SET event = props CREATE (event)-[:HAS_MEMBER {graph_id: " + cypherString(graph.String()) + "}]->(member) RETURN count(event)"
		var count string
		if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
			t.Fatal(err)
		}
	}
	target := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, end.Add(time.Minute), new(end.Add(2*time.Minute)))
	if _, err := database.pool.Exec(t.Context(), "ANALYZE "+database.contextTable()); err != nil {
		t.Fatal(err)
	}
	tracer := &relationScopeTracer{}
	configureTierPool(t, database, tracer)
	// 전용 DB 도우미는 최초 풀만 정리하므로 교체한 풀도 DB 삭제 전에 닫는다.
	t.Cleanup(database.Close)
	if err := database.pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	tracer.mu.Lock()
	tracer.queries, tracer.args = nil, nil
	tracer.mu.Unlock()
	values, err := database.activeEventContexts(t.Context(), graph, target.ID)
	if err != nil || len(values) != 1000 {
		t.Fatalf("제안 입력=%d: %v", len(values), err)
	}
	previous := target.ID.String()
	for _, value := range values {
		if value.ID == target.ID || value.GraphID != graph || value.Event == nil || len(value.Event.MemberIDs) != 1 || value.ID.String() >= previous {
			t.Fatal("제안 입력 범위·내림차순·참조 조립 위반")
		}
		previous = value.ID.String()
	}
	tracer.mu.Lock()
	queries, args := slices.Clone(tracer.queries), slices.Clone(tracer.args)
	tracer.queries, tracer.args = nil, nil
	tracer.mu.Unlock()
	if len(queries) != 5 {
		t.Fatalf("상한 뒤 본문·참조 조립 질의=%d", len(queries))
	}
	plan := explainHop(t, database, queries[0], args[0]...)
	logQueryIndexPlan(t, "proposal_input", plan)
	if plan.Root.Rows != 1000 || !planUsesIndex(plan.Root, "context_active_event_proposal_idx") || plan.Root.scanned("Context") > 1001 {
		t.Fatal("입력 상한·사건 부분 인덱스 미사용")
	}
	database.relationProposals = lifeProposalConfig(2)
	if err := database.ProposeEventRelations(t.Context(), graph, target.ID); err != nil {
		t.Fatal(err)
	}
	tracer.mu.Lock()
	locks := 0
	for _, query := range tracer.queries {
		if strings.Contains(query, "FROM public.context_graph WHERE graph_id = $1 AND deleted_at IS NULL FOR UPDATE") {
			locks++
		}
	}
	tracer.mu.Unlock()
	if locks != 1 {
		t.Fatalf("신호별 그래프 잠금=%d", locks)
	}
	relations := proposalRelations(t, database, graph)
	if len(relations) != 2 {
		t.Fatalf("생성 수 상한=%d", len(relations))
	}
	for _, relation := range relations {
		if _, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, relation.Type, relation.FromContextID, relation.ToContextID), nil); err != nil {
			t.Fatal(err)
		}
	}
	base, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Rollback(context.WithoutCancel(t.Context()))
	tx := &relationPlanTx{Tx: base}
	if err := database.lockContext(t.Context(), tx, graph, target.ID); err != nil {
		t.Fatal(err)
	}
	lockPlan := explainRelationPlan(t, base, tx.queries[0], tx.args[0]...)
	logQueryIndexPlan(t, "context_lock", lockPlan)
	if !planUsesIndex(lockPlan.Root, "context_graph_context_lookup_idx") || lockPlan.Root.scanned("Context") != 1 {
		t.Fatal("정점 잠금이 그래프·식별자 text/C 인덱스를 사용하지 않음")
	}
	tx.queries, tx.args = nil, nil
	if err := database.validateEventRelationTimes(t.Context(), tx, graph, target); err != nil {
		t.Fatal(err)
	}
	if len(tx.queries) != 5 {
		t.Fatalf("시간 검증이 상대 사건을 묶음 조회하지 않음: %d", len(tx.queries))
	}
}

func explainRelationPlan(t *testing.T, tx pgx.Tx, query string, args ...any) hopPlan {
	t.Helper()
	var raw []byte
	if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+query, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []hopPlan
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("관계 실행 계획 해석: %v", err)
	}
	return plans[0]
}

func (tx *relationPlanTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.queries = append(tx.queries, sql)
	tx.args = append(tx.args, args)
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *relationPlanTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.queries = append(tx.queries, sql)
	tx.args = append(tx.args, args)
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func TestRelationQueryScopeIntegration(t *testing.T) {
	for _, size := range []int{1000, 10000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			database := newIsolatedIntegrationStore(t)
			graph, from, to := relationTestEvents(t, database)
			foreign, foreignFrom, foreignTo := relationTestEvents(t, database)
			var relations []model.Relation
			for label, kind := range relationLabels {
				stored, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, kind, from.ID, to.ID), nil)
				if err != nil {
					t.Fatal(err)
				}
				relations = append(relations, stored)
				for offset := 0; offset < size; offset += 100 {
					var properties []string
					for range min(100, size-offset) {
						relation := proposedRelation(foreign, kind, foreignFrom.ID, foreignTo.ID)
						relation.ID = newTestID(t)
						encoded, err := encodeProperties(relationProperties(relation))
						if err != nil {
							t.Fatal(err)
						}
						properties = append(properties, encoded)
					}
					query := "MATCH (a:Context), (b:Context) WHERE a.context_id = " + cypherString(foreignFrom.ID.String()) + " AND b.context_id = " + cypherString(foreignTo.ID.String()) +
						" WITH a, b UNWIND [" + strings.Join(properties, ",") + "] AS props CREATE (a)-[e:" + label + "]->(b) SET e = props RETURN count(e)"
					var count string
					if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := database.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{database.graphName, label}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}
			for _, operation := range []string{"lookup", "identity", "list", "update", "cleanup", "cycle"} {
				t.Run(operation, func(t *testing.T) {
					base, err := database.pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer base.Rollback(context.WithoutCancel(t.Context()))
					if _, err := base.Exec(t.Context(), "SAVEPOINT relation_plan"); err != nil {
						t.Fatal(err)
					}
					tx := &relationPlanTx{Tx: base}
					for _, relation := range relations {
						switch operation {
						case "lookup":
							_, err = database.relationByID(t.Context(), tx, graph, relation.ID)
						case "identity":
							_, err = database.relationByIdentity(t.Context(), tx, graph, relation.Type, relation.FromContextID, relation.ToContextID)
						case "list":
							var values []model.Relation
							values, _, err = database.listRelations(t.Context(), tx, graph, from.ID, nil, nil, "", 10)
							if len(values) != 4 {
								t.Fatalf("목록 건수=%d", len(values))
							}
						case "update":
							_, err = database.updateRelation(t.Context(), tx, graph, relation)
						case "cleanup":
							relation.State = model.RelationStateProposed
							relation.ConfirmedBy, relation.ConfirmedByAgent, relation.ConfirmedAt = model.ID{}, model.ID{}, nil
							_, err = database.updateRelation(t.Context(), tx, graph, relation)
							if err == nil {
								var deleted bool
								deleted, err = database.deleteRelationProposal(t.Context(), tx, graph, relation)
								if !deleted {
									t.Fatal("제안 정리 미적용")
								}
							}
						case "cycle":
							var found bool
							found, err = database.hasConfirmedRelationPath(t.Context(), tx, graph, relation.Type, relation.FromContextID, relation.ToContextID)
							if !found {
								t.Fatal("확정 경로 누락")
							}
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					for i, query := range tx.queries {
						// 정리 질의도 대상 행이 존재하는 상태에서 실제 쓰기 계획을 실행한다.
						if i == 0 {
							if _, err := base.Exec(t.Context(), "ROLLBACK TO SAVEPOINT relation_plan"); err != nil {
								t.Fatal(err)
							}
						}
						plan := explainRelationPlan(t, base, query, tx.args[i]...)
						logQueryIndexPlan(t, operation, plan)
						for label := range relationLabels {
							if scanned := plan.Root.scanned(label); scanned > 32 {
								t.Errorf("%s가 다른 그래프의 %s 간선까지 읽음: %g/%d", operation, label, scanned, size)
							}
						}
					}
				})
			}
		})
	}
}
