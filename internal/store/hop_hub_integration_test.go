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
	"github.com/jackc/pgx/v5"
)

// createHopHub는 탐색 비용만 재도록 저장 정점·참조를 묶음 생성한다. 쓰기·색인 작업자
// 성능 표본이 아니며 정식 속성과 참조 방향을 쓰는 전용 시험 DB에서만 실행한다.
func createHopHub(t testing.TB, database *Store, size int) (model.ID, model.Context) {
	t.Helper()
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		tx, err := database.pool.Begin(ctx)
		if err != nil {
			t.Errorf("허브 시험 정리 시작: %v", err)
			return
		}
		defer tx.Rollback(ctx)
		// DETACH DELETE의 정점별 간선 재검사 대신 이 시험이 만든 graph_id만
		// 간선→정점 순서로 지운다. 운영 데이터의 삭제 경로로 사용하지 않는다.
		for _, label := range []string{"DERIVED_FROM", "Context"} {
			table := pgx.Identifier{database.graphName, label}.Sanitize()
			if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE properties ->> 'graph_id'::text = $1", graph.String()); err != nil {
				t.Errorf("허브 시험 %s 정리: %v", label, err)
				return
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM public.context_graph WHERE graph_id = $1", graph.String()); err != nil {
			t.Errorf("허브 시험 그래프 정리: %v", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("허브 시험 정리 커밋: %v", err)
		}
	})
	hub, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://hop-hub/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < size; offset += 100 {
		properties := make([]string, 0, 100)
		for range min(100, size-offset) {
			value := testDerivedContext(t, graph, actor)
			value.Body = strings.Repeat("허브 표본 ", 200)
			encoded, err := encodeProperties(contextProperties(value))
			if err != nil {
				t.Fatal(err)
			}
			properties = append(properties, encoded)
		}
		query := "MATCH (hub:Context) WHERE hub.graph_id = " + cypherString(graph.String()) + " AND hub.context_id = " + cypherString(hub.ID.String()) +
			" WITH hub UNWIND [" + strings.Join(properties, ",") + "] AS props CREATE (leaf:Context) SET leaf = props CREATE (leaf)-[:DERIVED_FROM {graph_id: " + cypherString(graph.String()) + "}]->(hub) RETURN count(leaf)"
		var count string
		if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
			t.Fatalf("허브 표본 생성: %v", err)
		}
	}
	return graph, hub
}

func BenchmarkHopHubIntegration(b *testing.B) {
	database := newIntegrationStore(b)
	for _, size := range []int{1000, 20000} {
		graph, hub := createHopHub(b, database, size)
		for _, limit := range []int{2, 10} {
			for _, implementation := range []string{"full", "bounded"} {
				b.Run(fmt.Sprintf("neighbors=%d/limit=%d/%s", size, limit, implementation), func(b *testing.B) {
					b.ReportAllocs()
					var result HopResult
					for b.Loop() {
						var err error
						if implementation == "full" {
							result, err = hopFullBaseline(b.Context(), database, graph, []model.Context{hub}, 1, "in", []string{"derived_from"}, limit)
						} else {
							result, err = database.HopContextsFrom(b.Context(), graph, []model.Context{hub}, 1, "in", []string{"derived_from"}, limit)
						}
						if err != nil {
							b.Fatal(err)
						}
						if len(result.Contexts) != limit || !result.Truncated || result.Boundary != 1 || len(result.Edges) != limit-1 {
							b.Fatalf("허브 절단 계약 불일치: 노드 %d, 간선 %d, 절단 %v/%d", len(result.Contexts), len(result.Edges), result.Truncated, result.Boundary)
						}
					}
					b.ReportMetric(float64(result.expansionQueries), "queries/op")
					b.ReportMetric(float64(result.work.identifierRows), "idrows/op")
					b.ReportMetric(float64(result.work.bodyRows), "bodyrows/op")
					b.ReportMetric(float64(result.work.bodyBytes), "bodybytes/op")
					b.ReportMetric(float64(result.work.edgeRows), "edgerows/op")
				})
			}
		}
	}
}

// hopFullBaseline는 변경 전 운영 BFS를 같은 표본에 실행하는 비교 기준이다.
func hopFullBaseline(ctx context.Context, database *Store, graph model.ID, starts []model.Context, hops int, direction string, filter []string, limit int) (HopResult, error) {
	queries := 0
	var work hopWork
	result, err := database.traverseHops(ctx, graph, starts, hops, filter, limit, &queries, func(frontier []model.ID, label traversalLabel) ([]hopNeighbor, error) {
		queries++
		neighbors, err := database.hopNeighbors(ctx, graph, frontier, label, direction)
		work.bodyRows += len(neighbors)
		work.edgeRows += len(neighbors)
		for _, neighbor := range neighbors {
			work.bodyBytes += len(neighbor.context.Body)
		}
		return neighbors, err
	})
	result.work = work
	return result, err
}

func TestHopBoundedMatchesFullIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	sample := createHopSample(t, database, "bounded")
	for _, starts := range [][]model.Context{{sample.nodes["s1"]}, {sample.nodes["e3"], sample.nodes["s1"], sample.nodes["e3"]}, {sample.nodes["d2"], sample.nodes["e4"], sample.nodes["s4"]}} {
		for _, direction := range []string{"out", "in", "both"} {
			for _, filter := range [][]string{nil, {"derived_from"}, {"relates_to", "has_member", "derived_from"}, {"precedes", "causes", "part_of", "relates_to"}, {"derived_from", "derived_from"}} {
				for _, hops := range []int{0, 1, 2, 6} {
					for _, limit := range []int{1, 2, 3, 6, 20} {
						name := fmt.Sprintf("%s/%v/hops=%d/limit=%d", direction, filter, hops, limit)
						full, err := hopFullBaseline(t.Context(), database, sample.graphID, starts, hops, direction, filter, limit)
						if err != nil {
							t.Fatal(err)
						}
						bounded, err := database.HopContextsFrom(t.Context(), sample.graphID, starts, hops, direction, filter, limit)
						if err != nil {
							t.Fatal(err)
						}
						compareHopResults(t, name, full, bounded)
						if bounded.work.bodyRows > len(bounded.Contexts) || len(bounded.Contexts) > limit {
							t.Fatal("본문 상한")
						}
					}
				}
			}
		}
	}
}

func TestHopHubBoundedWorkIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub := createHopHub(t, database, 20000)
	for _, limit := range []int{1, 2, 10} {
		full, err := hopFullBaseline(t.Context(), database, graph, []model.Context{hub}, 2, "both", nil, limit)
		if err != nil {
			t.Fatal(err)
		}
		bounded, err := database.HopContextsFrom(t.Context(), graph, []model.Context{hub}, 2, "both", nil, limit)
		if err != nil {
			t.Fatal(err)
		}
		compareHopResults(t, fmt.Sprint(limit), full, bounded)
		if bounded.work.bodyRows != limit-1 || bounded.work.identifierRows > limit+14 || bounded.work.edgeRows != limit-1 {
			t.Fatalf("적재 작업량: %+v", bounded.work)
		}
		if full.work.bodyRows < 20000 {
			t.Fatal("기준선이 허브를 읽지 않았다")
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := database.HopContextsFrom(canceled, graph, []model.Context{hub}, 1, "in", nil, 2)
	if err == nil || !reflect.DeepEqual(result, HopResult{}) {
		t.Fatalf("취소에 부분 성공을 반환했다: %+v, %v", result, err)
	}
}

func TestHopBoundedDeadlineReleasesConnectionIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub := createHopHub(t, database, 1)
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{database.graphName, "Context"}.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result, err := database.HopContextsFrom(ctx, graph, []model.Context{hub}, 1, "in", nil, 2)
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(result, HopResult{}) {
		t.Fatalf("마감 오류와 빈 결과: %+v, %v", result, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.HopContextsFrom(t.Context(), graph, []model.Context{hub}, 1, "in", nil, 2); err != nil {
		t.Fatalf("마감 후 연결 회복: %v", err)
	}
}

func TestHopHubQueryPlansIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub := createHopHub(t, database, 20000)
	label := traversalLabel{label: "DERIVED_FROM", kind: "derived_from"}
	bounded, err := database.hopCandidateSQL(graph, []model.ID{hub.ID}, []model.ID{hub.ID}, label, "in", 1)
	if err != nil {
		t.Fatal(err)
	}
	full := "MATCH (anchor:Context)-[edge:DERIVED_FROM]->(neighbor:Context) WHERE neighbor.context_id IN [" + cypherString(hub.ID.String()) + "] AND anchor.graph_id = " + cypherString(graph.String()) + " AND neighbor.graph_id = " + cypherString(graph.String()) + " AND edge.graph_id = " + cypherString(graph.String()) + " RETURN neighbor.context_id, anchor, id(neighbor) = id(startNode(edge)) ORDER BY neighbor.context_id, anchor.context_id"
	for _, plan := range []struct{ name, query string }{{"full", database.cypherSQL(full, "anchor agtype, node agtype, forward agtype")}, {"bounded", bounded}} {
		rows, err := database.pool.Query(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF) "+plan.query, pgx.QueryExecModeExec)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			// 집계값만 남기고 생성한 UUID를 포함한 조건식은 출력하지 않는다.
			if strings.Contains(line, "actual rows=") || strings.Contains(line, "Buffers:") || strings.Contains(line, "Sort Method:") || strings.Contains(line, "Execution Time:") {
				t.Logf("%s: %s", plan.name, line)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
}
