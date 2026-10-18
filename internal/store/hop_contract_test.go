package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func TestHopFullContractIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	grantAccount(t, database, graph, actor, model.GraphGradeOwner)
	nodes := map[string]model.Context{}
	sequence := 0
	next := func(value model.Context) model.Context {
		t.Helper()
		sequence++
		value.ID = testContractHopID(t, sequence)
		return value
	}
	create := func(name string, value model.Context, refs ...model.ID) model.Context {
		t.Helper()
		value = next(value)
		// relates_to의 정규 방향 c→a가 시간 관계 a→c와 순환을 이루게 한다.
		if name == "a" {
			value.ID = testContractHopID(t, 9)
		} else if name == "c" {
			value.ID = testContractHopID(t, 7)
		}
		created, err := database.CreateContext(t.Context(), graph, value, refs)
		if err != nil {
			t.Fatalf("%s 생성: %v", name, err)
		}
		nodes[name] = created
		return created
	}
	for index := range 3 {
		name := fmt.Sprintf("s%d", index)
		create(name, testSourceContext(t, graph, actor, "api://search04/"+graph.String()+"/"+name))
	}
	old := create("old", testDerivedContext(t, graph, actor), nodes["s0"].ID)
	value, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, next(testDerivedContext(t, graph, actor)), []model.ID{nodes["s0"].ID}, old.ID, nil, WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	nodes["new"] = value
	base := time.Now().UTC()
	expired := testDerivedContext(t, graph, actor)
	expired.Derived.ValidFrom, expired.Derived.ValidTo = new(base.Add(-2*time.Hour)), new(base.Add(-time.Hour))
	create("expired", expired, nodes["s1"].ID)
	for index, name := range []string{"a", "b", "c", "d"} {
		member := nodes["s0"].ID
		if index >= 2 {
			member = nodes[fmt.Sprintf("s%d", index-1)].ID
		}
		event := testEventContext(t, graph, actor, member)
		event.Event.Start, event.Event.End = base.Add(-time.Hour+time.Duration(index)*time.Minute), new(base.Add(time.Hour))
		if name == "b" {
			event.Event.Start, event.Event.End = base.Add(-2*time.Hour), new(base.Add(2*time.Hour))
			event.Event.MemberIDs = []model.ID{nodes["s0"].ID, nodes["s1"].ID}
		}
		create(name, event)
	}
	dead := create("dead", testDerivedContext(t, graph, actor), nodes["s2"].ID)
	create("behind_dead", testDerivedContext(t, graph, actor), dead.ID)
	edge := func(from, to, kind string) HopEdge {
		return HopEdge{FromID: nodes[from].ID, ToID: nodes[to].ID, Kind: kind}
	}
	edges := []HopEdge{
		edge("old", "s0", "derived_from"), edge("new", "s0", "derived_from"), edge("new", "old", "supersedes"), edge("expired", "s1", "derived_from"),
		edge("a", "s0", "has_member"), edge("b", "s0", "has_member"), edge("b", "s1", "has_member"), edge("c", "s1", "has_member"), edge("d", "s2", "has_member"),
	}
	for _, relation := range []struct{ from, to, kind string }{{"a", "c", "precedes"}, {"c", "d", "causes"}, {"a", "b", "part_of"}, {"c", "a", "relates_to"}} {
		if _, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationType(relation.kind), nodes[relation.from].ID, nodes[relation.to].ID), nil); err != nil {
			field, _ := errors.AsType[model.FieldError](err)
			t.Fatalf("%s %s→%s: %v (%s: %s)", relation.kind, relation.from, relation.to, err, field.Field, field.Message)
		}
		edges = append(edges, edge(relation.from, relation.to, relation.kind))
	}
	proposal := proposedRelation(graph, model.RelationTypeRelatesTo, nodes["a"].ID, nodes["d"].ID)
	proposal.ID = newTestID(t)
	if _, err := database.CreateRelation(t.Context(), graph, proposal); err != nil {
		t.Fatal(err)
	}
	dead, err = database.SetContextDeleted(t.Context(), graph, dead.ID, actor, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 별도 그래프의 정상 정점과, 그래프 속성만 다른 내부 간선을 둬 세 경계를 구분한다.
	foreign := createTestGraph(t, database, actor)
	foreignNode, err := database.CreateContext(t.Context(), foreign, testSourceContext(t, foreign, actor, "api://search04/foreign"), nil)
	if err != nil {
		t.Fatal(err)
	}
	properties := encodedHopRelation(t, foreign, model.RelationTypeRelatesTo, nodes["a"].ID, nodes["d"].ID, model.RelationStateConfirmed)
	query := fmt.Sprintf("MATCH (a:Context {context_id: %s}), (b:Context {context_id: %s}) CREATE (a)-[e:RELATES_TO]->(b) SET e = %s RETURN count(e)", cypherString(nodes["a"].ID.String()), cypherString(nodes["d"].ID.String()), properties)
	var count string
	if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
		t.Fatal(err)
	}
	properties = encodedHopRelation(t, graph, model.RelationTypeRelatesTo, nodes["a"].ID, foreignNode.ID, model.RelationStateConfirmed)
	query = fmt.Sprintf("MATCH (a:Context {context_id: %s}), (b:Context {context_id: %s}) CREATE (a)-[e:RELATES_TO]->(b) SET e = %s RETURN count(e)", cypherString(nodes["a"].ID.String()), cypherString(foreignNode.ID.String()), properties)
	if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
		t.Fatal(err)
	}
	ids := func(names []string) []model.ID {
		result := make([]model.ID, len(names))
		for index, name := range names {
			result[index] = nodes[name].ID
		}
		return result
	}
	cases := []struct {
		name, direction string
		starts, filter  []string
		hops            int
		want            []string
		distances       []int
	}{
		{"zero", "both", []string{"a"}, nil, 0, []string{"a"}, []int{0}},
		{"out_one", "out", []string{"a"}, nil, 1, []string{"a", "s0", "c", "b"}, []int{0, 1, 1, 1}},
		{"in_one", "in", []string{"a"}, nil, 1, []string{"a", "c"}, []int{0, 1}},
		{"both_one", "both", []string{"a"}, nil, 1, []string{"a", "s0", "c", "b"}, []int{0, 1, 1, 1}},
		{"out_two", "out", []string{"a"}, nil, 2, []string{"a", "s0", "c", "b", "s1", "d"}, []int{0, 1, 1, 1, 2, 2}},
		{"last_depth_edges", "both", []string{"s0"}, nil, 1, []string{"s0", "old", "new", "b", "a"}, []int{0, 1, 1, 1, 1}},
		{"multi_zero", "out", []string{"a", "s0", "a"}, nil, 0, []string{"a", "s0"}, []int{0, 0}},
		{"multi_one", "out", []string{"a", "new", "a"}, nil, 1, []string{"a", "new", "s0", "old", "c", "b"}, []int{0, 0, 1, 1, 1, 1}},
		{"cycle", "out", []string{"a"}, []string{"precedes", "causes", "relates_to"}, 4, []string{"a", "c", "d"}, []int{0, 1, 2}},
		{"part_of", "in", []string{"b"}, []string{"part_of"}, 1, []string{"b", "a"}, []int{0, 1}},
		{"deleted_barrier", "both", []string{"s2"}, []string{"derived_from"}, 3, []string{"s2"}, []int{0}},
		{"expired_active", "in", []string{"s1"}, []string{"derived_from"}, 1, []string{"s1", "expired"}, []int{0, 1}},
		{"supersedes_refs", "out", []string{"new"}, []string{"supersedes"}, 1, []string{"new", "old"}, []int{0, 1}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			starts := make([]model.Context, len(test.starts))
			for index, name := range test.starts {
				starts[index] = nodes[name]
			}
			result, err := database.HopContextsFrom(t.Context(), graph, starts, test.hops, test.direction, test.filter, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]model.ID, len(result.Contexts))
			if len(got) != len(test.want) {
				t.Fatalf("노드 수: got=%d want=%d", len(got), len(test.want))
			}
			for index, value := range result.Contexts {
				got[index] = value.ID
				if value.GraphID != graph || value.DeletedAt != nil || result.Distances[value.ID] != test.distances[index] {
					t.Fatalf("그래프·활성·최단 거리: %+v", result)
				}
				want := nodes[test.want[index]]
				if value.ID != want.ID || value.Layer != want.Layer {
					t.Fatalf("발견 순서 %d: got=%s want=%s", index, value.ID, want.ID)
				}
				if value.Derived != nil && (!slices.Equal(value.Derived.DerivedFrom, want.Derived.DerivedFrom) || !reflect.DeepEqual(value.Derived.Supersedes, want.Derived.Supersedes)) || value.Event != nil && !slices.Equal(value.Event.MemberIDs, want.Event.MemberIDs) {
					t.Fatalf("범위 밖을 포함한 참조가 잘렸다: %+v", value)
				}
			}
			if !slices.Equal(got, ids(test.want)) || len(result.Distances) != len(got) || result.Truncated || result.Boundary != 0 {
				t.Fatalf("발견 순서·중복·거리·절단: %+v", result)
			}
			wantEdges := make([]HopEdge, 0)
			for _, edge := range edges {
				if slices.Contains(got, edge.FromID) && slices.Contains(got, edge.ToID) && (len(test.filter) == 0 || slices.Contains(test.filter, edge.Kind)) {
					wantEdges = append(wantEdges, edge)
				}
			}
			slices.SortFunc(wantEdges, compareHopEdges)
			if !slices.Equal(result.Edges, wantEdges) {
				t.Fatalf("내부 간선의 완전성·방향·확정 상태: got=%v want=%v", result.Edges, wantEdges)
			}
			if len(starts) == 1 {
				single, err := database.HopContexts(t.Context(), graph, starts[0].ID, test.hops, test.direction, test.filter, 0)
				if err != nil {
					t.Fatal(err)
				}
				compareHopResults(t, test.name, result, single)
			}
		})
	}
	t.Run("start_state", func(t *testing.T) {
		for _, limit := range []int{0, 1} {
			for _, snapshot := range []bool{false, true} {
				read := func(ctx context.Context) error {
					result, err := database.HopContextsFrom(ctx, graph, []model.Context{dead}, 0, "both", nil, limit)
					if !errors.Is(err, ErrNotFound) || !reflect.DeepEqual(result, HopResult{}) {
						return fmt.Errorf("폐기 시작점 snapshot=%t limit=%d: %+v, %v", snapshot, limit, result, err)
					}
					return nil
				}
				var err error
				if snapshot {
					_, err = database.WithReadSnapshot(t.Context(), read)
				} else {
					err = read(t.Context())
				}
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, start := range []model.ID{newTestID(t), dead.ID, foreignNode.ID} {
			result, err := database.HopContexts(t.Context(), graph, start, 0, "both", nil, 0)
			if !errors.Is(err, ErrNotFound) || !reflect.DeepEqual(result, HopResult{}) {
				t.Fatalf("시작 부재·폐기·다른 그래프: %+v %v", result, err)
			}
		}
	})
	t.Run("full_boundary", func(t *testing.T) {
		for _, test := range []struct {
			limit, count, boundary int
			truncated              bool
		}{{1, 1, 1, true}, {3, 3, 1, true}, {4, 4, 0, false}} {
			result, err := hopFullBaseline(t.Context(), database, graph, []model.Context{nodes["a"]}, 1, "out", nil, test.limit)
			if err != nil || len(result.Contexts) != test.count || result.Truncated != test.truncated || result.Boundary != test.boundary {
				t.Fatalf("전체 이웃 기준선 절단: %+v %v", result, err)
			}
		}
		result, err := hopFullBaseline(t.Context(), database, graph, []model.Context{nodes["a"], nodes["new"], nodes["a"]}, 0, "both", nil, 1)
		if err != nil || len(result.Contexts) != 1 || !result.Truncated || result.Boundary != 0 {
			t.Fatalf("시작점 절단 경계: %+v %v", result, err)
		}
	})
	assertConnectionRecovery(t, database, nil, 0)
}

func testContractHopID(t *testing.T, sequence int) model.ID {
	t.Helper()
	id, err := model.ParseID(fmt.Sprintf("019a0000-0000-7000-8000-%012d", sequence))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHopFullMaxReferencesIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	graph, _, derived, event, members := createHopReferenceSample(t, database)
	for _, test := range []struct {
		name  string
		start model.Context
		count int
	}{{"derived100", derived, 100}, {"event1000", event, 1000}} {
		t.Run(test.name, func(t *testing.T) {
			result, err := database.HopContexts(t.Context(), graph, test.start.ID, 0, "both", nil, 0)
			if err != nil || len(result.Contexts) != 1 || len(result.Edges) != 0 || result.Truncated {
				t.Fatalf("0홉 참조 조회: %+v %v", result, err)
			}
			value := result.Contexts[0]
			var refs []model.ID
			if value.Derived != nil {
				refs = value.Derived.DerivedFrom
			} else {
				refs = value.Event.MemberIDs
			}
			want := slices.Clone(members[:test.count])
			slices.SortFunc(want, func(a, b model.ID) int { return cmp.Compare(a.String(), b.String()) })
			if !slices.Equal(refs, want) || result.work.referenceRows != test.count {
				t.Fatalf("최대 정상 참조: got=%d rows=%d want=%d", len(refs), result.work.referenceRows, test.count)
			}
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}
