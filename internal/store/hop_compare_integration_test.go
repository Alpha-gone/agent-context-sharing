package store

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// hopSample은 「홉 탐색 구현 비교」의 결과 조건을 모두 건드리는 그래프 표본이다.
type hopSample struct {
	graphID model.ID
	nodes   map[string]model.Context
	deleted model.ID
}

// createHopSample은 원천·파생·사건, 확정·제안 관계, 순환, 삭제된 중간 노드를 가진 그래프를
// 만든다. 파생 d4는 삭제된 dx를 거쳐야만 닿으므로 삭제 노드 제외가 결과를 바꾼다.
//
//	s1 <- d1 -> s2      d2 -> d1      dx -> s3, d4 -> dx(삭제)
//	e1{s1} -precedes(확정)-> e2{s2} -causes(확정)-> e3{s3} -relates_to(확정)-> e1 (순환)
//	e3 -precedes(제안)-> e4{s4}
func createHopSample(t *testing.T, database *Store, label string) hopSample {
	t.Helper()
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	sample := hopSample{graphID: graphID, nodes: map[string]model.Context{}}
	create := func(name string, value model.Context, derivedFrom ...model.ID) model.Context {
		t.Helper()
		created, err := database.CreateContext(t.Context(), graphID, value, derivedFrom)
		if err != nil {
			t.Fatalf("%s 생성: %v", name, err)
		}
		sample.nodes[name] = created
		return created
	}
	for index := range 4 {
		name := fmt.Sprintf("s%d", index+1)
		create(name, testSourceContext(t, graphID, actorID, fmt.Sprintf("api://hop-compare/%s/%s/%s", label, graphID, name)))
	}
	d1 := create("d1", testDerivedContext(t, graphID, actorID), sample.nodes["s1"].ID, sample.nodes["s2"].ID)
	create("d2", testDerivedContext(t, graphID, actorID), d1.ID)
	dx := create("dx", testDerivedContext(t, graphID, actorID), sample.nodes["s3"].ID)
	create("d4", testDerivedContext(t, graphID, actorID), dx.ID)
	// 사건은 구성원의 발생 시각을 덮는 기본 구간을 쓴다. 차례로 만들어 시작 시각이 늘어나므로
	// precedes와 causes의 시간 순서 검증을 통과한다.
	for index := range 4 {
		name := fmt.Sprintf("e%d", index+1)
		create(name, testEventContext(t, graphID, actorID, sample.nodes[fmt.Sprintf("s%d", index+1)].ID))
	}
	for _, relation := range []struct {
		kind      model.RelationType
		from, to  string
		confirmed bool
	}{
		{model.RelationTypePrecedes, "e1", "e2", true},
		{model.RelationTypeCauses, "e2", "e3", true},
		{model.RelationTypeRelatesTo, "e3", "e1", true},
		{model.RelationTypePrecedes, "e3", "e4", false},
	} {
		from, to := sample.nodes[relation.from].ID, sample.nodes[relation.to].ID
		if relation.confirmed {
			if _, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, relation.kind, from, to), nil); err != nil {
				t.Fatalf("%s→%s 확정: %v", relation.from, relation.to, err)
			}
			continue
		}
		proposal := model.Relation{ID: newTestID(t), GraphID: graphID, Type: relation.kind, FromContextID: from, ToContextID: to,
			State: model.RelationStateProposed, ProposedBy: model.ProposalSourceSystem, ProposedAt: time.Now().UTC()}
		if _, err := database.CreateRelation(t.Context(), graphID, proposal); err != nil {
			t.Fatalf("%s→%s 제안: %v", relation.from, relation.to, err)
		}
	}
	if _, err := database.SetContextDeleted(t.Context(), graphID, dx.ID, actorID, true, 0); err != nil {
		t.Fatalf("중간 노드 삭제: %v", err)
	}
	sample.deleted = dx.ID
	return sample
}

// TestHopImplementationsReturnSameResultIntegration은 두 홉 탐색 구현이 「홉 탐색 구현
// 비교」의 결과 조건 여섯 가지를 모든 방향·필터·깊이·상한 조합에서 같게 만드는지 확인한다.
// 같은 모양의 다른 그래프를 함께 두어 graph_id 격리가 결과에 섞이지 않는지도 본다.
func TestHopImplementationsReturnSameResultIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	sample := createHopSample(t, database, "primary")
	foreign := createHopSample(t, database, "foreign")

	startSets := map[string][]string{"원천 하나": {"s1"}, "원천과 사건": {"s1", "e3"}, "셋": {"d2", "e4", "s4"}}
	filters := map[string][]string{
		"전체":       nil,
		"근거":       {"derived_from"},
		"사건 관계":    {"precedes", "causes", "part_of", "relates_to"},
		"섞인 label": {"derived_from", "has_member", "precedes", "relates_to"},
	}
	truncatedCases, deletedSeen, proposedCrossed := 0, false, false
	for startName, names := range startSets {
		starts := make([]model.Context, 0, len(names))
		for _, name := range names {
			starts = append(starts, sample.nodes[name])
		}
		for _, direction := range []string{"out", "in", "both"} {
			for filterName, filter := range filters {
				for _, hops := range []int{1, 2, 3, math.MaxInt} {
					for _, limit := range []int{0, 3, 6} {
						name := fmt.Sprintf("%s/%s/%s/%d홉/상한%d", startName, direction, filterName, hops, limit)
						baseline, err := database.HopContextsFrom(t.Context(), sample.graphID, starts, hops, direction, filter, limit)
						if err != nil {
							t.Fatalf("%s 기준선: %v", name, err)
						}
						candidate, err := database.hopContextsPrefetched(t.Context(), sample.graphID, starts, hops, direction, filter, limit)
						if err != nil {
							t.Fatalf("%s 후보: %v", name, err)
						}
						compareHopResults(t, name, baseline, candidate)
						assertHopIsolation(t, name, sample, foreign, candidate)
						if baseline.Truncated {
							truncatedCases++
						}
						if _, found := baseline.Distances[sample.deleted]; found {
							deletedSeen = true
						}
						if _, found := baseline.Distances[sample.nodes["e4"].ID]; found && !slices.Contains(names, "e4") && !slices.Contains(names, "s4") {
							proposedCrossed = true
						}
						if candidate.expansionQueries > 1 {
							t.Fatalf("%s 후보 확장 질의 = %d, want 1 이하", name, candidate.expansionQueries)
						}
					}
				}
			}
		}
	}
	// 표본이 실제로 절단을 만들고 삭제 노드와 제안 관계를 결과에서 빼는지 기준선으로 확인한다.
	// 그렇지 않으면 두 구현이 같아도 그 조건을 검증하지 못한 것이다.
	if truncatedCases == 0 || deletedSeen || proposedCrossed {
		t.Fatalf("표본 조건이 드러나지 않았다: 절단 %d건, 삭제 노드 반환 %v, 제안 관계 통과 %v", truncatedCases, deletedSeen, proposedCrossed)
	}
}

func compareHopResults(t *testing.T, name string, baseline, candidate HopResult) {
	t.Helper()
	ids := func(result HopResult) []model.ID {
		values := make([]model.ID, 0, len(result.Contexts))
		for _, value := range result.Contexts {
			values = append(values, value.ID)
		}
		return values
	}
	if !reflect.DeepEqual(ids(baseline), ids(candidate)) {
		t.Fatalf("%s 컨텍스트 순서가 다르다: %v != %v", name, ids(baseline), ids(candidate))
	}
	if !reflect.DeepEqual(baseline.Distances, candidate.Distances) {
		t.Fatalf("%s 거리가 다르다: %v != %v", name, baseline.Distances, candidate.Distances)
	}
	if !reflect.DeepEqual(baseline.Edges, candidate.Edges) {
		t.Fatalf("%s 간선이 다르다: %v != %v", name, baseline.Edges, candidate.Edges)
	}
	if baseline.Truncated != candidate.Truncated || baseline.Boundary != candidate.Boundary {
		t.Fatalf("%s 절단이 다르다: %v/%d != %v/%d", name, baseline.Truncated, baseline.Boundary, candidate.Truncated, candidate.Boundary)
	}
	for index := range baseline.Contexts {
		if !reflect.DeepEqual(baseline.Contexts[index], candidate.Contexts[index]) {
			t.Fatalf("%s 컨텍스트 %s의 속성이 다르다", name, baseline.Contexts[index].ID)
		}
	}
}

func assertHopIsolation(t *testing.T, name string, sample, foreign hopSample, result HopResult) {
	t.Helper()
	foreignIDs := map[model.ID]struct{}{}
	for _, value := range foreign.nodes {
		foreignIDs[value.ID] = struct{}{}
	}
	for _, value := range result.Contexts {
		if _, leaked := foreignIDs[value.ID]; leaked || value.GraphID != sample.graphID {
			t.Fatalf("%s 다른 그래프 노드 %s가 섞였다", name, value.ID)
		}
	}
	for _, edge := range result.Edges {
		for _, id := range []model.ID{edge.FromID, edge.ToID} {
			if _, found := result.Distances[id]; !found {
				t.Fatalf("%s 간선 끝 %s가 결과 밖이다", name, id)
			}
		}
	}
}
