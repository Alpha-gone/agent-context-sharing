package search

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestTEST02RankingRoutingAndBudgetKnownSample(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-00000000c001")
	a := testContext(t, graph, "019a0000-0000-7000-8000-00000000c002", "가😀", 0)
	b := testContext(t, graph, "019a0000-0000-7000-8000-00000000c003", "나", 2)
	c := testContext(t, graph, "019a0000-0000-7000-8000-00000000c004", "다", 1)
	b.Layer, c.Layer = model.LayerEvent, model.LayerEvent
	entry := []channelResult{
		{name: "semantic", candidates: []model.SearchCandidate{{Context: a, Similarity: 0.875}, {Context: b, Similarity: 0.625}}},
		{name: "keyword", candidates: []model.SearchCandidate{{Context: b}, {Context: a}}},
		{name: "time", candidates: []model.SearchCandidate{{Context: c}}},
	}
	wantIDs := []model.ID{c.ID, b.ID, a.ID}
	// 진입 순위 B=A=3/2는 기록 시각으로 B가 앞선다. 그래프 채널을 더하면
	// C=2가 1위가 된다. 시간 채널 기여가 있는 C도 독립 진입점이다.
	for _, order := range channelOrders(3) {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			results := make([]channelResult, 0, 4)
			for _, index := range order {
				results = append(results, entry[index])
			}
			combined := combine(results)
			route, reason := routeSignals(results, combined, 0.5).Decide(0.875, 0.25)
			if !slices.Equal(candidateIDs(combined), []model.ID{b.ID, a.ID, c.ID}) || route != RouteLocal || reason != ReasonEventAnchor {
				t.Fatalf("진입 순위·경로 = %v %s/%s", candidateIDs(combined), route, reason)
			}
			for _, reverseEdge := range []bool{false, true} {
				edge := model.HopEdge{FromID: b.ID, ToID: c.ID, Kind: "relates_to"}
				if reverseEdge {
					edge.FromID, edge.ToID = edge.ToID, edge.FromID
				}
				all := append(slices.Clone(results), channelResult{name: "graph", candidates: []model.SearchCandidate{{Context: c}}, edges: []model.HopEdge{edge}})
				combined = combine(all)
				if !slices.Equal(candidateIDs(combined), wantIDs) {
					t.Fatalf("최종 순위 = %v", candidateIDs(combined))
				}
				for index, want := range []string{"2/1", "3/2", "3/2"} {
					if got := combined[index].score.String(); got != want {
						t.Fatalf("정확 순위 합 = %s, want %s", got, want)
					}
				}
				for _, sample := range []struct{ budget, selected, used, excluded int }{
					{0, 3, 4, 0}, {1, 1, 1, 2}, {2, 2, 2, 1}, {3, 2, 2, 1}, {4, 3, 4, 0},
				} {
					selected, used, cut, _ := selectEvidenceSets(combined, buildCandidateSubgraph(combined, all), sample.budget)
					if !slices.Equal(candidateIDs(selected), wantIDs[:sample.selected]) || used != sample.used {
						t.Fatalf("예산 %d의 선택·문자 수 = %v/%d", sample.budget, candidateIDs(selected), used)
					}
					if sample.excluded == 0 && cut != nil || sample.excluded > 0 && (cut == nil || cut.Reason != "budget" || cut.Excluded != sample.excluded) {
						t.Fatalf("예산 %d의 절단 = %+v", sample.budget, cut)
					}
				}
			}
		})
	}
}

func TestTEST02RoutingFloatingPointBoundaries(t *testing.T) {
	for _, sample := range []struct {
		name        string
		top, second float64
		route       Route
	}{
		{"equal_thresholds", 0.875, 0.625, RouteDirect},
		{"below_top", math.Nextafter(0.875, 0), 0.5, RouteLocal},
		{"below_margin", 0.875, math.Nextafter(0.625, 1), RouteLocal},
	} {
		t.Run(sample.name, func(t *testing.T) {
			signals := RouteSignals{RelevantLocal: true, SemanticCount: 2, SemanticTop: sample.top, SemanticSecond: sample.second}
			if route, _ := signals.Decide(0.875, 0.25); route != sample.route {
				t.Fatalf("경로 = %s, want %s", route, sample.route)
			}
		})
	}
}
