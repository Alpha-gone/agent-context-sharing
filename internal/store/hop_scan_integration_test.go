package store

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// hopScanCandidateSQL는 #110의 식별자 중심 질의를 보존한 시험 기준선이다.
// 본문까지 모두 읽던 이전 기준선과 혼동하지 않는다.
func hopScanCandidateSQL(s *Store, graph model.ID, hub model.Context, limit int) string {
	query := "MATCH (anchor:Context)<-[edge:DERIVED_FROM]-(neighbor:Context) WHERE anchor.context_id IN [" + cypherString(hub.ID.String()) + "]" +
		" AND anchor.graph_id = " + cypherString(graph.String()) + " AND neighbor.graph_id = " + cypherString(graph.String()) + " AND edge.graph_id = " + cypherString(graph.String()) +
		" AND neighbor.deleted_at IS NULL AND NOT neighbor.context_id IN [" + cypherString(hub.ID.String()) + "] RETURN anchor.context_id, neighbor.context_id"
	return fmt.Sprintf(`WITH pairs AS MATERIALIZED (%s)
SELECT neighbor, min(anchor::text COLLATE "C") AS first_anchor FROM pairs
GROUP BY neighbor ORDER BY first_anchor, neighbor LIMIT %d`, s.cypherSQL(query, "anchor agtype, neighbor agtype"), limit)
}

// hopHubScanBaseline는 단일 허브·1홉·derived_from 표본에 #110과 같은 조립을 수행한다.
// 운영 함수에 시험용 선택기를 추가하지 않고 후보 질의만 이전 것으로 대조한다.
func hopHubScanBaseline(ctx context.Context, s *Store, graph model.ID, hub model.Context, limit int) (HopResult, error) {
	rows, err := s.reader(ctx).Query(ctx, hopScanCandidateSQL(s, graph, hub, limit), pgx.QueryExecModeExec)
	if err != nil {
		return HopResult{}, err
	}
	defer rows.Close()
	ids := []model.ID{hub.ID}
	for rows.Next() {
		var raw, anchor string
		if err := rows.Scan(&raw, &anchor); err != nil {
			return HopResult{}, err
		}
		id, err := parseAnchorID(raw)
		if err != nil {
			return HopResult{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return HopResult{}, err
	}
	result := HopResult{Contexts: []model.Context{hub}, Distances: map[model.ID]int{hub.ID: 0}, Truncated: len(ids) > limit}
	if result.Truncated {
		result.Boundary = 1
		ids = ids[:limit]
	}
	values, err := s.contextVerticesByIDs(ctx, s.reader(ctx), graph, ids[1:])
	if err != nil {
		return HopResult{}, err
	}
	for _, id := range ids[1:] {
		result.Contexts = append(result.Contexts, values[id])
		result.Distances[id] = 1
	}
	list, err := cypherIDList(ids)
	if err != nil {
		return HopResult{}, err
	}
	query := "MATCH (a:Context)-[edge:DERIVED_FROM]->(b:Context) WHERE a.context_id IN " + list + " AND b.context_id IN " + list +
		" AND a.graph_id = " + cypherString(graph.String()) + " AND b.graph_id = " + cypherString(graph.String()) + " AND edge.graph_id = " + cypherString(graph.String()) +
		" RETURN DISTINCT a.context_id, b.context_id"
	edgeRows, err := s.reader(ctx).Query(ctx, s.cypherSQL(query, "source agtype, target agtype"), pgx.QueryExecModeExec)
	if err != nil {
		return HopResult{}, err
	}
	defer edgeRows.Close()
	for edgeRows.Next() {
		var source, target string
		if err := edgeRows.Scan(&source, &target); err != nil {
			return HopResult{}, err
		}
		from, err := parseAnchorID(source)
		if err != nil {
			return HopResult{}, err
		}
		to, err := parseAnchorID(target)
		if err != nil {
			return HopResult{}, err
		}
		result.Edges = append(result.Edges, HopEdge{FromID: from, ToID: to, Kind: "derived_from"})
	}
	if err := edgeRows.Err(); err != nil {
		return HopResult{}, err
	}
	slices.SortFunc(result.Edges, compareHopEdges)
	result.Edges = slices.Compact(result.Edges)
	result.work.referenceRows, err = s.fillHopReferences(ctx, graph, result.Contexts)
	if err != nil {
		return HopResult{}, err
	}
	if err := s.touchEmbeddings(ctx, graph, ids, nowUTC()); err != nil {
		return HopResult{}, err
	}
	return result, nil
}

type hopPlanNode struct {
	NodeType     string        `json:"Node Type"`
	Relation     string        `json:"Relation Name"`
	Index        string        `json:"Index Name"`
	Rows         float64       `json:"Actual Rows"`
	Loops        float64       `json:"Actual Loops"`
	Filtered     float64       `json:"Rows Removed by Filter"`
	Hit          int           `json:"Shared Hit Blocks"`
	Read         int           `json:"Shared Read Blocks"`
	TempRead     int           `json:"Temp Read Blocks"`
	TempWritten  int           `json:"Temp Written Blocks"`
	SortSpace    int           `json:"Sort Space Used"`
	SortLocation string        `json:"Sort Space Type"`
	Plans        []hopPlanNode `json:"Plans"`
}

type hopPlan struct {
	Root      hopPlanNode `json:"Plan"`
	Execution float64     `json:"Execution Time"`
}

func explainHop(t *testing.T, s *Store, query string, arguments ...any) hopPlan {
	t.Helper()
	var raw []byte
	if err := s.pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+query, append([]any{pgx.QueryExecModeExec}, arguments...)...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []hopPlan
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("실행 계획 해석: %v", err)
	}
	return plans[0]
}

func (p hopPlanNode) scanned(relation string) float64 {
	var total float64
	if p.Relation == relation {
		total = (p.Rows + p.Filtered) * p.Loops
	}
	for _, child := range p.Plans {
		total += child.scanned(relation)
	}
	return total
}

func logHopPlan(t *testing.T, name string, p hopPlanNode) {
	t.Helper()
	if p.Relation != "" || p.Index != "" || p.SortLocation != "" {
		t.Logf("%s plan: %s relation=%s index=%s rows=%g loops=%g filtered=%g sort=%s/%dKB", name, p.NodeType, p.Relation, p.Index, p.Rows, p.Loops, p.Filtered, p.SortLocation, p.SortSpace)
	}
	for _, child := range p.Plans {
		logHopPlan(t, name, child)
	}
}

func TestHopSelectiveScanIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	for _, size := range []int{1000, 20000, 50000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			graph, hub := createHopHub(t, database, size)
			for _, name := range []string{"Context", "DERIVED_FROM"} {
				if _, err := database.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{database.graphName, name}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}
			excessSQL, err := database.hopCandidateSQL(graph, []model.ID{hub.ID}, []model.ID{hub.ID}, traversalLabel{label: "DERIVED_FROM"}, "in", 0)
			if err != nil {
				t.Fatal(err)
			}
			excess := explainHop(t, database, excessSQL)
			t.Logf("neighbors=%d remaining=0 candidate=%.3fms edgeRows=%.0f vertexRows=%.0f buffers=%d+%d temp=%d/%d", size, excess.Execution,
				excess.Root.scanned("DERIVED_FROM"), excess.Root.scanned("Context"), excess.Root.Hit, excess.Root.Read, excess.Root.TempRead, excess.Root.TempWritten)
			logHopPlan(t, "excess", excess.Root)
			bothSQL, err := database.hopCandidateSQL(graph, []model.ID{hub.ID}, []model.ID{hub.ID}, traversalLabel{label: "DERIVED_FROM"}, "both", 9)
			if err != nil {
				t.Fatal(err)
			}
			both := explainHop(t, database, bothSQL)
			if both.Root.Rows != 10 || both.Root.scanned("DERIVED_FROM") >= float64(size) {
				t.Fatalf("양방향 전체 스캔: 행 %g, 간선 %g", both.Root.Rows, both.Root.scanned("DERIVED_FROM"))
			}
			t.Logf("neighbors=%d both candidate=%.3fms edgeRows=%.0f vertexRows=%.0f buffers=%d+%d temp=%d/%d", size, both.Execution,
				both.Root.scanned("DERIVED_FROM"), both.Root.scanned("Context"), both.Root.Hit, both.Root.Read, both.Root.TempRead, both.Root.TempWritten)
			logHopPlan(t, "both", both.Root)
			for _, limit := range []int{2, 10} {
				query, err := database.hopCandidateSQL(graph, []model.ID{hub.ID}, []model.ID{hub.ID}, traversalLabel{label: "DERIVED_FROM"}, "in", limit-1)
				if err != nil {
					t.Fatal(err)
				}
				for repeat := range 3 {
					for _, implementation := range []string{"scan", "selective"} {
						candidateSQL := query
						if implementation == "scan" {
							candidateSQL = hopScanCandidateSQL(database, graph, hub, limit)
						}
						plan := explainHop(t, database, candidateSQL)
						if implementation == "selective" && plan.Root.scanned("DERIVED_FROM") >= float64(size) {
							t.Fatalf("인접 간선 전체 스캔이 남았다: %g", plan.Root.scanned("DERIVED_FROM"))
						}
						started := time.Now()
						var result HopResult
						if implementation == "scan" {
							result, err = hopHubScanBaseline(t.Context(), database, graph, hub, limit)
						} else {
							result, err = database.HopContextsFrom(t.Context(), graph, []model.Context{hub}, 1, "in", []string{"derived_from"}, limit)
						}
						elapsed := time.Since(started)
						if err != nil {
							t.Fatal(err)
						}
						if len(result.Contexts) != limit || len(result.Edges) != limit-1 || !result.Truncated || result.Boundary != 1 {
							t.Fatalf("표본 응답 계약: %+v", result)
						}
						t.Logf("neighbors=%d limit=%d repeat=%d %s candidate=%.3fms total=%.3fms edgeRows=%.0f vertexRows=%.0f buffers=%d+%d temp=%d/%d", size, limit, repeat, implementation,
							plan.Execution, float64(elapsed)/float64(time.Millisecond), plan.Root.scanned("DERIVED_FROM"), plan.Root.scanned("Context"), plan.Root.Hit, plan.Root.Read, plan.Root.TempRead, plan.Root.TempWritten)
						if repeat == 0 {
							logHopPlan(t, implementation, plan.Root)
						}
					}
					baseline, err := hopHubScanBaseline(t.Context(), database, graph, hub, limit)
					if err != nil {
						t.Fatal(err)
					}
					selective, err := database.HopContextsFrom(t.Context(), graph, []model.Context{hub}, 1, "in", []string{"derived_from"}, limit)
					if err != nil {
						t.Fatal(err)
					}
					compareHopResults(t, "식별자 중심 기준선", baseline, selective)
				}
			}
			for _, implementation := range []string{"scan", "selective"} {
				started := time.Now()
				var group sync.WaitGroup
				errors := make(chan error, 4)
				for range 4 {
					group.Go(func() {
						for range 3 {
							var result HopResult
							var err error
							if implementation == "scan" {
								result, err = hopHubScanBaseline(t.Context(), database, graph, hub, 10)
							} else {
								result, err = database.HopContextsFrom(t.Context(), graph, []model.Context{hub}, 1, "in", []string{"derived_from"}, 10)
							}
							if err != nil {
								errors <- err
								return
							}
							if len(result.Contexts) != 10 || len(result.Edges) != 9 || !result.Truncated {
								errors <- fmt.Errorf("동시 조회 응답 불일치")
								return
							}
						}
					})
				}
				group.Wait()
				close(errors)
				for err := range errors {
					t.Fatal(err)
				}
				t.Logf("neighbors=%d %s concurrent=4 requests=12 elapsed=%.3fms", size, implementation, float64(time.Since(started))/float64(time.Millisecond))
			}
		})
	}
}

func TestHopCandidateSQLBoundaries(t *testing.T) {
	s := &Store{graphName: "agent_context"}
	graph, anchor := newTestID(t), newTestID(t)
	label := traversalLabel{label: "DERIVED_FROM"}
	for _, remaining := range []int{0, 1, 63, 64, 65, int(^uint(0) >> 1)} {
		query, err := s.hopCandidateSQL(graph, []model.ID{anchor}, nil, label, "both", remaining)
		if err != nil || strings.Contains(query, "LIMIT -") || strings.Contains(query, "NOT IN ()") {
			t.Fatalf("상한 %d: %v, %s", remaining, err, query)
		}
		if remaining == 0 && (strings.Contains(query, "ORDER BY") || strings.Contains(query, "GROUP BY")) {
			t.Fatal("초과 확인에 집계·정렬이 남았다")
		}
	}
}

func TestHopSelectiveProbeBoundaryIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	for _, size := range []int{63, 64, 65} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			graph, hub := createHopHub(t, database, size)
			for _, limit := range []int{1, 2, 64, 65, 66, 67} {
				for _, direction := range []string{"in", "both"} {
					full, err := hopFullBaseline(t.Context(), database, graph, []model.Context{hub}, 2, direction, []string{"derived_from"}, limit)
					if err != nil {
						t.Fatal(err)
					}
					selected, err := database.HopContextsFrom(t.Context(), graph, []model.Context{hub}, 2, direction, []string{"derived_from"}, limit)
					if err != nil {
						t.Fatal(err)
					}
					compareHopResults(t, fmt.Sprintf("probe/%d/%d/%s", size, limit, direction), full, selected)
				}
			}
		})
	}
}

// 인덱스 제거와 표본 쓰기는 트랜잭션을 반드시 롤백한다. 부하 비교용 전용
// 시험 DB에서 실행하며 운영·공유 개발 스키마를 바꾸는 절차로 사용하지 않는다.
func TestHopProbeIndexCostIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub := createHopHub(t, database, 50000)
	rows, err := database.pool.Query(t.Context(), `SELECT indexname, pg_relation_size((schemaname || '.' || quote_ident(indexname))::regclass)
FROM pg_indexes WHERE schemaname = $1 AND (indexname = 'context_active_graph_context_idx' OR indexname LIKE '%_hop_endpoints_idx') ORDER BY indexname`, database.graphName)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	count := 0
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		t.Logf("index=%s bytes=%d", name, size)
		total += size
		count++
	}
	rows.Close()
	if err := rows.Err(); err != nil || count != 8 {
		t.Fatalf("추가 인덱스 목록: 개수 %d, %v", count, err)
	}
	t.Logf("additional-index-bytes=%d", total)
	properties := make([]string, 0, 1000)
	for range 1000 {
		value := testDerivedContext(t, graph, hub.CreatedBy)
		value.Body = strings.Repeat("허브 표본 ", 200)
		encoded, err := encodeProperties(contextProperties(value))
		if err != nil {
			t.Fatal(err)
		}
		properties = append(properties, encoded)
	}
	query := database.cypherSQL("MATCH (hub:Context) WHERE hub.context_id = "+cypherString(hub.ID.String())+" AND hub.graph_id = "+cypherString(graph.String())+
		" WITH hub UNWIND ["+strings.Join(properties, ",")+"] AS props CREATE (leaf:Context) SET leaf = props CREATE (leaf)-[:DERIVED_FROM {graph_id: "+cypherString(graph.String())+"}]->(hub) RETURN count(leaf)", "count agtype")
	for repeat := range 6 {
		order := []bool{false, true}
		if repeat%2 == 1 {
			slices.Reverse(order)
		}
		for _, indexed := range order {
			func() {
				tx, err := database.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil {
						t.Errorf("인덱스 비용 시험 롤백: %v", err)
					}
				}()
				if !indexed {
					for _, name := range []string{"context_active_graph_context_idx", "derived_from_hop_endpoints_idx"} {
						if _, err := tx.Exec(t.Context(), "DROP INDEX "+pgx.Identifier{database.graphName, name}.Sanitize()); err != nil {
							t.Fatal(err)
						}
					}
				}
				started := time.Now()
				var count string
				if err := tx.QueryRow(t.Context(), query, pgx.QueryExecModeExec).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != "1000" {
					t.Fatalf("쓰기 표본 수: %s", count)
				}
				t.Logf("write-repeat=%d warmup=%v additional-indexes=%v vertices=1000 edges=1000 elapsed=%.3fms", repeat, repeat == 0, indexed, float64(time.Since(started))/float64(time.Millisecond))
			}()
		}
	}
}

func encodedHopRelation(t testing.TB, graph model.ID, kind model.RelationType, from, to model.ID, state model.RelationState) string {
	t.Helper()
	relation := confirmable(t, graph, kind, from, to)
	if state == model.RelationStateProposed {
		relation.State = state
		relation.ConfirmedBy, relation.ConfirmedByAgent, relation.ConfirmedAt = model.ID{}, model.ID{}, nil
	}
	if err := relation.Validate(); err != nil {
		t.Fatal(err)
	}
	properties, err := encodeProperties(relationProperties(relation))
	if err != nil {
		t.Fatal(err)
	}
	return properties
}

func TestHopSelectiveMultiAnchorAndDenseEdgesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, source := createHopHub(t, database, 0)
	var starts []model.Context
	for range 2 {
		value, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, source.CreatedBy, source.ID), nil)
		if err != nil {
			t.Fatal(err)
		}
		starts = append(starts, value)
	}
	properties := make([]string, 0, 100)
	for index := range 100 {
		value := testEventContext(t, graph, source.CreatedBy, source.ID)
		if index >= 90 {
			deleted := nowUTC()
			value.DeletedAt = &deleted
		}
		encoded, err := encodeProperties(contextProperties(value))
		if err != nil {
			t.Fatal(err)
		}
		properties = append(properties, "{node: "+encoded+
			", first: "+encodedHopRelation(t, graph, model.RelationTypePrecedes, starts[0].ID, value.ID, model.RelationStateConfirmed)+
			", second: "+encodedHopRelation(t, graph, model.RelationTypePrecedes, starts[1].ID, value.ID, model.RelationStateConfirmed)+
			", proposed: "+encodedHopRelation(t, graph, model.RelationTypeCauses, starts[1].ID, value.ID, model.RelationStateProposed)+"}")
	}
	// 같은 이웃을 두 시작 노드가 발견하며 삭제·제안 간선도 함께 둔다.
	query := "MATCH (a:Context), (b:Context), (s:Context) WHERE a.context_id = " + cypherString(starts[0].ID.String()) +
		" AND b.context_id = " + cypherString(starts[1].ID.String()) + " AND s.context_id = " + cypherString(source.ID.String()) +
		" WITH a,b,s UNWIND [" + strings.Join(properties, ",") + "] AS props" +
		" WITH a,b,s,props.node AS node,props.first AS first_props,props.second AS second_props,props.proposed AS proposed_props" +
		" CREATE (leaf:Context) SET leaf = node" +
		" CREATE (leaf)-[:HAS_MEMBER {graph_id: " + cypherString(graph.String()) + "}]->(s)" +
		" CREATE (a)-[first:PRECEDES]->(leaf) SET first = first_props" +
		" CREATE (b)-[second:PRECEDES]->(leaf) SET second = second_props" +
		" CREATE (b)-[proposed:CAUSES]->(leaf) SET proposed = proposed_props RETURN count(leaf)"
	var count string
	if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
		t.Fatal(err)
	}
	// 마지막 깊이의 선택 정점 사이를 조밀하게 잇는다. 이 간선들은 확장 중
	// 절단 여부와 무관하게 최종 부분 그래프에서 빠짐없이 반환되어야 한다.
	query = "MATCH (a:Context), (b:Context) WHERE a.graph_id = " + cypherString(graph.String()) + " AND b.graph_id = " + cypherString(graph.String()) +
		" AND a.layer = 'event' AND b.layer = 'event' AND a.context_id < b.context_id AND a.deleted_at IS NULL AND b.deleted_at IS NULL" +
		" RETURN a.context_id,b.context_id ORDER BY a.context_id,b.context_id LIMIT 300"
	rows, err := database.pool.Query(t.Context(), database.cypherSQL(query, "source agtype, target agtype"), pgx.QueryExecModeExec)
	if err != nil {
		t.Fatal(err)
	}
	var relations []string
	for rows.Next() {
		var fromRaw, toRaw string
		if err := rows.Scan(&fromRaw, &toRaw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		from, fromErr := parseAnchorID(fromRaw)
		to, toErr := parseAnchorID(toRaw)
		if fromErr != nil || toErr != nil {
			rows.Close()
			t.Fatalf("조밀한 간선 끝점: %v %v", fromErr, toErr)
		}
		relations = append(relations, encodedHopRelation(t, graph, model.RelationTypeRelatesTo, from, to, model.RelationStateConfirmed))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	query = "UNWIND [" + strings.Join(relations, ",") + "] AS props MATCH (a:Context), (b:Context)" +
		" WHERE a.context_id = props.from_context_id AND b.context_id = props.to_context_id" +
		" CREATE (a)-[edge:RELATES_TO]->(b) SET edge = props RETURN count(edge)"
	if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Context", "PRECEDES", "RELATES_TO"} {
		if _, err := database.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{database.graphName, label}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	starts = []model.Context{starts[1], starts[0], starts[1]}
	for _, direction := range []string{"in", "out", "both"} {
		for _, filter := range [][]string{{"precedes", "relates_to"}, {"causes"}, nil} {
			for _, limit := range []int{2, 3, 10, 16, 17, 92, 93, 100} {
				full, err := hopFullBaseline(t.Context(), database, graph, starts, 2, direction, filter, limit)
				if err != nil {
					t.Fatal(err)
				}
				selected, err := database.HopContextsFrom(t.Context(), graph, starts, 2, direction, filter, limit)
				if err != nil {
					t.Fatal(err)
				}
				compareHopResults(t, fmt.Sprintf("dense/%s/%v/%d", direction, filter, limit), full, selected)
			}
		}
	}
	// 내부 간선의 실제 계획도 측정한다. 작은 label 테이블의 순차 스캔이나
	// 큰 시작 차수의 잔여 스캔을 응답 상한으로 숨겨서 보고하지 않는다.
	ids := []model.ID{starts[0].ID, starts[1].ID, source.ID}
	selected, err := database.HopContextsFrom(t.Context(), graph, starts, 1, "out", []string{"precedes"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range selected.Contexts {
		ids = append(ids, node.ID)
	}
	ids = slices.Compact(slices.SortedFunc(slices.Values(ids), func(a, b model.ID) int { return strings.Compare(a.String(), b.String()) }))
	label := traversalLabel{label: "PRECEDES", kind: "precedes", confirmed: true}
	internal, err := database.hopIncludedEdgeSQL(graph, ids, label)
	if err != nil {
		t.Fatal(err)
	}
	plan := explainHop(t, database, internal)
	logHopPlan(t, "included", plan.Root)
	if plan.Root.Rows != float64(len(selected.Edges)) {
		t.Fatalf("내부 간선 누락: %g != %d", plan.Root.Rows, len(selected.Edges))
	}
	t.Logf("outgoing included edges=%g edge-access=%g buffers=%d+%d execution=%.3fms", plan.Root.Rows, plan.Root.scanned("PRECEDES"), plan.Root.Hit, plan.Root.Read, plan.Execution)
}
