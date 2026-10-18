package store

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 작은 표본/존재 확인 전환 양쪽에서 물리 삽입 순서와 발견 순서를 다르게 둔다.
func TestHopBoundedSelectionContractIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	for _, size := range []int{63, 64, 65, 130} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			graph, source := createHopHub(t, database, 0)
			id := func(sequence int) model.ID { return testContractHopID(t, size*1000+sequence) }
			createStart := func(sequence int) model.Context {
				t.Helper()
				value := testEventContext(t, graph, source.CreatedBy, source.ID)
				value.ID = id(sequence)
				value.Event.Start = source.Source.OccurredAt
				created, err := database.CreateContext(t.Context(), graph, value, nil)
				if err != nil {
					t.Fatal(err)
				}
				return created
			}
			early, late := createStart(10), createStart(900)
			starts := []model.Context{late, early, late}
			var nodes []model.Context
			sequences := []int{1, 2, 30}
			for i := range size {
				sequences = append(sequences, 100+i)
			}
			for _, sequence := range sequences {
				value := testEventContext(t, graph, source.CreatedBy, source.ID)
				value.ID = id(sequence)
				// 시작 시각을 같게 두어 삽입 순서와 무관하게 precedes의 시간 제약을 지킨다.
				value.Event.Start = source.Source.OccurredAt
				value.Body = strings.Repeat("선택 본문🙂", sequence%11+1)
				if sequence == 1 {
					value.DeletedAt = new(nowUTC())
				}
				nodes = append(nodes, value)
			}
			var properties []string
			// 큰 식별자를 먼저 저장해 heap 순서를 정렬의 대용으로 쓸 수 없게 한다.
			for _, value := range slices.Backward(nodes) {
				encoded, err := encodeProperties(contextProperties(value))
				if err != nil {
					t.Fatal(err)
				}
				properties = append(properties, encoded)
			}
			query := "MATCH (source:Context) WHERE source.context_id = " + cypherString(source.ID.String()) +
				" WITH source UNWIND [" + strings.Join(properties, ",") + "] AS props CREATE (node:Context) SET node = props" +
				" CREATE (node)-[:HAS_MEMBER {graph_id: " + cypherString(graph.String()) + "}]->(source) RETURN count(node)"
			var count string
			if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil || count != fmt.Sprint(size+3) {
				t.Fatalf("정점 표본: count=%s err=%v", count, err)
			}
			var edges []HopEdge
			var relations []string
			add := func(from, to model.ID, kind model.RelationType, state model.RelationState) {
				t.Helper()
				relations = append(relations, encodedHopRelation(t, graph, kind, from, to, state))
				if state == model.RelationStateConfirmed && from != nodes[0].ID && to != nodes[0].ID {
					edges = append(edges, HopEdge{FromID: from, ToID: to, Kind: string(kind)})
				}
			}
			for _, value := range nodes[3:] {
				add(early.ID, value.ID, model.RelationTypePrecedes, model.RelationStateConfirmed)
				add(value.ID, late.ID, model.RelationTypePrecedes, model.RelationStateConfirmed)
			}
			add(early.ID, nodes[0].ID, model.RelationTypePrecedes, model.RelationStateConfirmed)
			add(early.ID, nodes[1].ID, model.RelationTypePrecedes, model.RelationStateProposed)
			add(late.ID, nodes[2].ID, model.RelationTypePrecedes, model.RelationStateConfirmed)
			for i := range 8 {
				for j := i + 1; j < 8; j++ {
					add(nodes[3+i].ID, nodes[3+j].ID, model.RelationTypeRelatesTo, model.RelationStateConfirmed)
				}
			}
			for _, kind := range []struct{ label, name string }{{"PRECEDES", "precedes"}, {"RELATES_TO", "relates_to"}} {
				query = "UNWIND [" + strings.Join(relations, ",") + "] AS props MATCH (a:Context), (b:Context)" +
					" WHERE props.relation_type = " + cypherString(kind.name) + " AND a.graph_id = " + cypherString(graph.String()) +
					" AND b.graph_id = " + cypherString(graph.String()) + " AND a.context_id = props.from_context_id AND b.context_id = props.to_context_id" +
					" CREATE (a)-[edge:" + kind.label + "]->(b) SET edge = props RETURN count(edge)"
				if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
					t.Fatal(err)
				}
			}
			for _, label := range []string{"Context", "PRECEDES", "RELATES_TO"} {
				if _, err := database.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{database.graphName, label}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}
			for _, direction := range []string{"out", "in", "both"} {
				for _, limit := range []int{1, 2, 3, 10, size + 1, size + 2, size + 3, math.MaxInt} {
					t.Run(fmt.Sprintf("%s/%d", direction, limit), func(t *testing.T) {
						want := []model.Context{late, early}
						want = append(want, nodes[3:]...)
						if direction != "in" {
							// exclusive의 ID 30은 공통 이웃보다 작아도 뒤 기준 정점에 속한다.
							want = append(want, nodes[2])
						}
						truncated := len(want) > limit
						boundary, extra := 0, 0
						if truncated && limit >= 2 {
							boundary, extra = 1, 1
						}
						want = want[:min(len(want), limit)]
						filter := []string{"precedes", "relates_to"}
						result, err := database.HopContextsFrom(t.Context(), graph, starts, 1, direction, filter, limit)
						if err != nil {
							t.Fatal(err)
						}
						if len(result.Contexts) != len(want) || result.Truncated != truncated || result.Boundary != boundary {
							t.Fatalf("노드·최초 절단: count=%d/%d truncated=%t/%t boundary=%d", len(result.Contexts), len(want), result.Truncated, truncated, result.Boundary)
						}
						selected := make(map[model.ID]bool)
						bodyBytes := 0
						for i, value := range want {
							got := result.Contexts[i]
							distance := 0
							if i >= 2 {
								distance = 1
							}
							if got.ID != value.ID || got.Body != value.Body || !slices.Equal(got.Event.MemberIDs, []model.ID{source.ID}) || result.Distances[got.ID] != distance {
								t.Fatalf("발견 순서·본문·참조·거리 index=%d id=%s want=%s", i, got.ID, value.ID)
							}
							selected[got.ID] = true
							if i >= 2 {
								bodyBytes += len(value.Body)
							}
						}
						var wantEdges []HopEdge
						for _, edge := range edges {
							if selected[edge.FromID] && selected[edge.ToID] {
								wantEdges = append(wantEdges, edge)
							}
						}
						slices.SortFunc(wantEdges, compareHopEdges)
						if !slices.Equal(result.Edges, wantEdges) || result.work.edgeRows != len(wantEdges) || result.work.referenceRows != len(want) || result.work.bodyRows != max(0, len(want)-2) || result.work.bodyBytes != bodyBytes {
							t.Fatalf("내부 간선·참조·본문 적재: edges=%d/%d work=%+v", len(result.Edges), len(wantEdges), result.work)
						}
						queries := 2
						if truncated {
							queries = 1
							if limit == 1 {
								queries = 0
							}
						}
						if result.expansionQueries != queries || result.work.identifierRows != max(0, len(want)-2)+extra {
							t.Fatalf("초과 한 행·마지막 홉 조회 생략: queries=%d want=%d work=%+v", result.expansionQueries, queries, result.work)
						}
						full, err := hopFullBaseline(t.Context(), database, graph, starts, 1, direction, filter, limit)
						if err != nil {
							t.Fatal(err)
						}
						compareHopResults(t, t.Name(), full, result)
					})
				}
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
