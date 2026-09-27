package search

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// evidenceCandidate는 계층과 기여 채널을 지정한 결합 후보를 만든다. 진입점 판정이
// 계층과 채널만 보므로 계층별 속성 묶음은 채우지 않는다.
func evidenceCandidate(t *testing.T, rawID string, layer model.Layer, body string, channels ...string) combinedCandidate {
	t.Helper()
	return combinedCandidate{value: model.Context{ID: testID(t, rawID), Layer: layer, Body: body}, channels: channels}
}

func edgeResult(edges ...store.HopEdge) []channelResult {
	return []channelResult{{name: "graph", edges: edges}}
}

func link(from, to combinedCandidate) store.HopEdge {
	return store.HopEdge{FromID: from.value.ID, ToID: to.value.ID, Kind: "relates_to"}
}

func candidateIDs(candidates []combinedCandidate) []model.ID {
	ids := make([]model.ID, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.value.ID)
	}
	return ids
}

func TestSelectEvidenceSetsAddsShortestPathToEntry(t *testing.T) {
	target := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000101", model.LayerEvent, "대상", "graph")
	entry := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000102", model.LayerEvent, "진입", "time")
	connector := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000103", model.LayerEvent, "연결", "graph")
	candidates := []combinedCandidate{target, entry, connector}
	graph := buildCandidateSubgraph(candidates, edgeResult(link(entry, connector), link(connector, target)))

	selected, used, truncation, stats := selectEvidenceSets(candidates, graph, 0)
	if got, want := candidateIDs(selected), []model.ID{target.value.ID, connector.value.ID, entry.value.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("연결 근거 집합 = %v, want %v", got, want)
	}
	if used != 6 || truncation != nil {
		t.Fatalf("예산 사용 = %d, 절단 = %+v", used, truncation)
	}
	if stats != (Selection{Candidates: 3, Selected: 3, Connectors: 2}) {
		t.Fatalf("선택 측정 = %+v", stats)
	}
}

func TestSelectEvidenceSetsExcludesDisconnectedCandidate(t *testing.T) {
	entry := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000111", model.LayerDerived, "진입", "semantic")
	isolated := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000112", model.LayerDerived, "고립", "graph")
	source := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000113", model.LayerSource, "원천", "graph")
	candidates := []combinedCandidate{isolated, entry, source}

	selected, _, truncation, stats := selectEvidenceSets(candidates, buildCandidateSubgraph(candidates, nil), 0)
	// 그래프 채널이 낸 원천은 진입 채널 기여가 없어도 진입점으로 취급한다.
	if got, want := candidateIDs(selected), []model.ID{entry.value.ID, source.value.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("선택 = %v, want %v", got, want)
	}
	if truncation != nil || stats.Unreachable != 1 || stats.Connectors != 0 {
		t.Fatalf("단절 후보 처리: 절단 %+v, 측정 %+v", truncation, stats)
	}
}

// TestShortestEntryPathBreaksTiesDeterministically는 동률 기준 셋을 차례로 확인한다.
// 간선 입력 순서를 뒤집어도 같은 경로를 골라야 반복 측정이 성립한다.
func TestShortestEntryPathBreaksTiesDeterministically(t *testing.T) {
	entry := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000121", model.LayerEvent, "진입", "keyword")
	target := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000122", model.LayerEvent, "대상", "graph")
	low := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000123", model.LayerEvent, "앞", "graph")
	high := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000124", model.LayerEvent, "뒤", "graph")
	leaf := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000125", model.LayerEvent, "잎", "graph")
	candidates := []combinedCandidate{target, entry, low, high, leaf}
	base := []store.HopEdge{link(target, low), link(target, high), link(low, entry), link(high, entry)}

	cases := map[string]struct {
		edges    []store.HopEdge
		included []model.ID
		want     model.ID
	}{
		// 새 연결 노드 수와 차수 합이 같으면 context_id 사전순으로 고른다.
		"식별자 사전순": {base, nil, low.value.ID},
		// low가 허브라 차수 합이 크면 high를 거친다.
		"차수 합": {append(slices.Clone(base), link(low, leaf)), nil, high.value.ID},
		// 이미 담긴 high를 거치면 새 연결 노드가 적으므로 high가 허브여도 high를 거친다.
		"새 연결 노드 수": {append(slices.Clone(base), link(high, leaf)), []model.ID{high.value.ID}, high.value.ID},
	}
	entries := map[model.ID]struct{}{entry.value.ID: {}}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			included := map[model.ID]struct{}{}
			for _, id := range test.included {
				included[id] = struct{}{}
			}
			want := []model.ID{target.value.ID, test.want, entry.value.ID}
			reversed := slices.Clone(test.edges)
			slices.Reverse(reversed)
			for _, edges := range [][]store.HopEdge{test.edges, reversed} {
				graph := buildCandidateSubgraph(candidates, edgeResult(edges...))
				if got := shortestEntryPath(target.value.ID, graph, entries, included); !reflect.DeepEqual(got, want) {
					t.Fatalf("경로 = %v, want %v", got, want)
				}
			}
		})
	}
}

// TestSelectEvidenceSetsTruncatesWholeSet은 연결 근거 집합을 나누지 않고, 처음 넘친
// 집합에서 멈춰 뒤 순위의 짧은 집합으로 채우지 않는지 확인한다.
func TestSelectEvidenceSetsTruncatesWholeSet(t *testing.T) {
	entry := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000131", model.LayerEvent, "가나", "semantic")
	target := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000132", model.LayerEvent, "다라", "graph")
	connector := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000133", model.LayerEvent, "마바사아", "graph")
	short := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000134", model.LayerEvent, "자", "time")
	farEntry := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000135", model.LayerEvent, "차카타파", "time")
	candidates := []combinedCandidate{entry, target, short, connector, farEntry}
	graph := buildCandidateSubgraph(candidates, edgeResult(link(target, connector), link(connector, farEntry)))

	// entry(2)는 들고, target 집합은 target·connector·farEntry 합 10자라 남은 6자를 넘는다.
	selected, used, truncation, stats := selectEvidenceSets(candidates, graph, 8)
	if got, want := candidateIDs(selected), []model.ID{entry.value.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("선택 = %v, want %v", got, want)
	}
	if used != 2 || truncation == nil || truncation.Reason != "budget" || truncation.Excluded != 4 {
		t.Fatalf("절단: used %d, %+v", used, truncation)
	}
	if stats.Selected != 1 || stats.Connectors != 0 {
		t.Fatalf("선택 측정 = %+v", stats)
	}
}

// TestFlowDisabledEvidenceSelectionKeepsRankTruncation은 비활성 구성이 기존 통합 순위
// 절단과 같은 앞부분·순위·간선을 내는지 확인한다.
func TestFlowDisabledEvidenceSelectionKeepsRankTruncation(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000141")
	database, entry, near, far := evidenceFlowStore(t, graphID)
	flow := evidenceFlow(t, database, graphID, false, 6)
	combined := []combinedCandidate{{value: entry}, {value: near}, {value: far}}
	expected, used, truncation := applyBudget(combined, 6)
	if got, want := flowIDs(flow), idStrings(candidateIDs(expected)); !reflect.DeepEqual(got, want) {
		t.Fatalf("비활성 결과 = %v, want %v", got, want)
	}
	if flow.BudgetUsed != used || !reflect.DeepEqual(flow.Truncation, truncation) {
		t.Fatalf("비활성 예산 = %d %+v, want %d %+v", flow.BudgetUsed, flow.Truncation, used, truncation)
	}
	for index, item := range flow.Contexts {
		if item.Rank != index+1 {
			t.Fatalf("비활성 순위 = %d, want %d", item.Rank, index+1)
		}
	}
	if flow.Selection != (Selection{Candidates: 3, Selected: 2}) {
		t.Fatalf("비활성 선택 측정 = %+v", flow.Selection)
	}
}

// TestFlowEvidenceSelectionKeepsRankOrderAndIncludedEdges는 활성 구성에서 경로 노드를
// 함께 담아도 응답이 통합 순위 순서이고, 담긴 노드 사이 간선만 남는지 확인한다.
func TestFlowEvidenceSelectionKeepsRankOrderAndIncludedEdges(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000151")
	database, entry, near, far := evidenceFlowStore(t, graphID)
	flow := evidenceFlow(t, database, graphID, true, 0)
	if got, want := flowIDs(flow), idStrings([]model.ID{entry.ID, near.ID, far.ID}); !reflect.DeepEqual(got, want) {
		t.Fatalf("활성 결과 = %v, want %v", got, want)
	}
	for index, item := range flow.Contexts {
		if item.Rank != index+1 {
			t.Fatalf("활성 순위 = %d, want %d", item.Rank, index+1)
		}
	}
	if len(flow.Edges) != 2 {
		t.Fatalf("응답 간선 = %+v", flow.Edges)
	}
	if degrees := []int{flow.Contexts[0].CandidateDegree, flow.Contexts[1].CandidateDegree, flow.Contexts[2].CandidateDegree}; !reflect.DeepEqual(degrees, []int{1, 2, 1}) {
		t.Fatalf("후보 부분 그래프 차수 = %v", degrees)
	}

	// 예산이 진입점 하나만 허용하면 far 집합(far·near)은 통째로 빠지고 간선도 남지 않는다.
	flow = evidenceFlow(t, database, graphID, true, 3)
	if got, want := flowIDs(flow), idStrings([]model.ID{entry.ID}); !reflect.DeepEqual(got, want) || len(flow.Edges) != 0 {
		t.Fatalf("집합 절단 결과 = %v, 간선 %+v", got, flow.Edges)
	}
	if flow.Truncation == nil || flow.Truncation.Excluded != 2 {
		t.Fatalf("집합 절단 표시 = %+v", flow.Truncation)
	}
}

// evidenceFlowStore는 진입점 하나에서 두 홉까지 이어지는 사건 경로를 만든다.
func evidenceFlowStore(t *testing.T, graphID model.ID) (*fakeStore, model.Context, model.Context, model.Context) {
	t.Helper()
	// 진입점과 한 홉 후보는 각 채널 1위라 통합 점수가 같으므로 더 늦게 기록한 진입점이 앞선다.
	entry := testContext(t, graphID, "019a0000-0000-7000-8000-0000000001f1", "진입점", 9)
	near := testContext(t, graphID, "019a0000-0000-7000-8000-0000000001f2", "한홉", 2)
	far := testContext(t, graphID, "019a0000-0000-7000-8000-0000000001f3", "두홉", 3)
	near.Layer, far.Layer = model.LayerEvent, model.LayerEvent
	database := &fakeStore{
		time: []store.SearchCandidate{{Context: entry}},
		hops: store.HopResult{
			Contexts:  []model.Context{entry, near, far},
			Distances: map[model.ID]int{entry.ID: 0, near.ID: 1, far.ID: 2},
			Edges: []store.HopEdge{
				{FromID: entry.ID, ToID: near.ID, Kind: "precedes"},
				{FromID: near.ID, ToID: far.ID, Kind: "part_of"},
			},
		},
	}
	return database, entry, near, far
}

func evidenceFlow(t *testing.T, database Store, graphID model.ID, enabled bool, budget int) Flow {
	t.Helper()
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageRelations, EvidencePathSelection: enabled}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: budget, MaxHops: 2, MaxHopNodes: 10})
	if err != nil {
		t.Fatalf("흐름 검색: %v", err)
	}
	return flow
}

func idStrings(ids []model.ID) []string {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, id.String())
	}
	return values
}
