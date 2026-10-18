package search

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestEvidenceEntryKindsContract(t *testing.T) {
	for _, sample := range []struct {
		name     string
		layer    model.Layer
		channels []string
		entry    bool
	}{
		{"source_without_channel", model.LayerSource, nil, true},
		{"source_from_graph", model.LayerSource, []string{"graph"}, true},
		{"semantic", model.LayerDerived, []string{"semantic"}, true},
		{"keyword", model.LayerEvent, []string{"keyword"}, true},
		{"time", model.LayerDerived, []string{"time"}, true},
		{"global_summary", model.LayerDerived, []string{"global_summary"}, true},
		{"mixed", model.LayerEvent, []string{"graph", "keyword"}, true},
		{"graph_only", model.LayerEvent, []string{"graph"}, false},
		{"without_channel", model.LayerDerived, nil, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			candidate := evidenceCandidate(t, "019a0000-0000-7000-8000-000000000b01", sample.layer, "본문", sample.channels...)
			selected, used, truncation, stats := selectEvidenceSets([]combinedCandidate{candidate}, buildCandidateSubgraph([]combinedCandidate{candidate}, nil), 2)
			want := Selection{Candidates: 1, Unreachable: 1}
			if sample.entry {
				want.Selected, want.Unreachable = 1, 0
			}
			if len(selected) != want.Selected || used != 2*want.Selected || truncation != nil || stats != want {
				t.Fatalf("selected=%v used=%d truncation=%+v stats=%+v, want %+v", candidateIDs(selected), used, truncation, stats, want)
			}
		})
	}
}

// 기준기는 단순 경로를 전부 열거한다. 운영 선택기의 BFS·동적 계획과 비용 비교 함수를
// 사용하지 않아 거리 우선·동률 기준의 결함을 독립적으로 검출한다.
func exhaustiveEvidencePath(ids []model.ID, adjacent [5][5]bool, entryMask, includedMask int) []model.ID {
	var best []int
	bestNew, bestDegree := 0, 0
	var visit func([]int)
	visit = func(path []int) {
		last := path[len(path)-1]
		if entryMask&(1<<last) != 0 {
			newNodes, degree := 0, 0
			for _, node := range path {
				if includedMask&(1<<node) == 0 {
					newNodes++
				}
				for _, connected := range adjacent[node] {
					if connected {
						degree++
					}
				}
			}
			less := best == nil || len(path) < len(best)
			if len(path) == len(best) {
				less = newNodes < bestNew || newNodes == bestNew && degree < bestDegree
				if newNodes == bestNew && degree == bestDegree {
					for index, node := range path {
						if ids[node] != ids[best[index]] {
							less = ids[node].String() < ids[best[index]].String()
							break
						}
					}
				}
			}
			if less {
				best, bestNew, bestDegree = slices.Clone(path), newNodes, degree
			}
			return
		}
		for next, connected := range adjacent[last] {
			if connected && !slices.Contains(path, next) {
				visit(append(slices.Clone(path), next))
			}
		}
	}
	visit([]int{0})
	var result []model.ID
	for _, node := range best {
		result = append(result, ids[node])
	}
	return result
}

func TestEvidenceShortestPathExhaustiveContract(t *testing.T) {
	var candidates []combinedCandidate
	for index := range 5 {
		candidates = append(candidates, evidenceCandidate(t, fmt.Sprintf("019a0000-0000-7000-8000-%012x", 0xb10+index), model.LayerEvent, "본", "graph"))
	}
	ids := candidateIDs(candidates)
	outside := testID(t, "019a0000-0000-7000-8000-000000000bff")
	kinds := []string{"derived_from", "supersedes", "has_member", "precedes", "causes", "part_of", "relates_to"}
	// 5개 정점의 무방향 단순 그래프 2^10개, 비어 있지 않은 진입점 15가지,
	// 이미 선택한 노드 집합 네 가지를 대조한다. 반복은 별도 시험 수에 더하지 않는다.
	comparisons := 0
	for mask := range 1 << 10 {
		var adjacent [5][5]bool
		var edges []model.HopEdge
		bit := 0
		for from := range 5 {
			for to := from + 1; to < 5; to++ {
				if mask&(1<<bit) != 0 {
					adjacent[from][to], adjacent[to][from] = true, true
					// 방향·중복·여러 label은 서로 다른 이웃의 차수를 늘리지 않는다.
					for _, kind := range kinds {
						edges = append(edges, model.HopEdge{FromID: ids[from], ToID: ids[to], Kind: kind}, model.HopEdge{FromID: ids[to], ToID: ids[from], Kind: kind})
					}
				}
				bit++
			}
		}
		edges = append(edges, model.HopEdge{FromID: ids[0], ToID: outside, Kind: "derived_from"}, model.HopEdge{FromID: outside, ToID: ids[4], Kind: "has_member"})
		if mask%2 != 0 {
			slices.Reverse(edges)
		}
		graph := buildCandidateSubgraph(candidates, edgeResult(edges...))
		for node, id := range ids {
			var wantNeighbors []model.ID
			for neighbor, connected := range adjacent[node] {
				if connected {
					wantNeighbors = append(wantNeighbors, ids[neighbor])
				}
			}
			if !slices.Equal(graph.adjacency[id], wantNeighbors) || graph.degrees[id] != len(wantNeighbors) {
				t.Fatalf("mask=%x node=%d: adjacency=%v degree=%d, want %v", mask, node, graph.adjacency[id], graph.degrees[id], wantNeighbors)
			}
		}
		for entryBits := 1; entryBits < 16; entryBits++ {
			entryMask := entryBits << 1 // 대표 후보인 0은 진입점이 아니다.
			entries := map[model.ID]struct{}{}
			for node, id := range ids {
				if entryMask&(1<<node) != 0 {
					entries[id] = struct{}{}
				}
			}
			for _, includedMask := range []int{0, 2, 16, 30} {
				included := map[model.ID]struct{}{}
				for node, id := range ids {
					if includedMask&(1<<node) != 0 {
						included[id] = struct{}{}
					}
				}
				want := exhaustiveEvidencePath(ids, adjacent, entryMask, includedMask)
				got := shortestEntryPath(ids[0], graph, entries, included)
				if !slices.Equal(got, want) {
					t.Fatalf("graph=%x entries=%x included=%x: path=%v, want %v", mask, entryMask, includedMask, got, want)
				}
				comparisons++
			}
		}
	}
	if comparisons != 61440 {
		t.Fatalf("경로 대조 수=%d", comparisons)
	}
}

func TestEvidenceSharedConnectorBudgetContract(t *testing.T) {
	makeCandidate := func(index int, body string, layer model.Layer) combinedCandidate {
		return evidenceCandidate(t, fmt.Sprintf("019a0000-0000-7000-8000-%012x", 0xb20+index), layer, body, "graph")
	}
	a, b := makeCandidate(0, "가😀", model.LayerEvent), makeCandidate(1, "e\u0301", model.LayerEvent)
	connector, entry := makeCandidate(2, "👩\u200d💻", model.LayerEvent), makeCandidate(3, "", model.LayerSource)
	short := makeCandidate(4, "짧", model.LayerSource)
	entry.value.Source = &model.SourceAttributes{Reference: model.SourceReference{Locator: strings.Repeat("x", 1000)}}
	candidates := []combinedCandidate{a, b, connector, entry, short}
	graph := buildCandidateSubgraph(candidates, edgeResult(link(a, connector), link(b, connector), link(connector, entry)))
	for _, sample := range []struct {
		budget, used, selected, connectors, excluded int
		ids                                          []model.ID
	}{
		{0, 8, 5, 2, 0, []model.ID{a.value.ID, connector.value.ID, entry.value.ID, b.value.ID, short.value.ID}},
		{1, 0, 0, 0, 5, nil},
		{4, 0, 0, 0, 5, nil},
		{5, 5, 3, 2, 2, []model.ID{a.value.ID, connector.value.ID, entry.value.ID}},
		{6, 5, 3, 2, 2, []model.ID{a.value.ID, connector.value.ID, entry.value.ID}},
		{7, 7, 4, 2, 1, []model.ID{a.value.ID, connector.value.ID, entry.value.ID, b.value.ID}},
		{8, 8, 5, 2, 0, []model.ID{a.value.ID, connector.value.ID, entry.value.ID, b.value.ID, short.value.ID}},
	} {
		t.Run(fmt.Sprint(sample.budget), func(t *testing.T) {
			selected, used, truncation, stats := selectEvidenceSets(candidates, graph, sample.budget)
			want := Selection{Candidates: 5, Selected: sample.selected, Connectors: sample.connectors}
			var wantTruncation *Truncation
			if sample.excluded > 0 {
				wantTruncation = &Truncation{Reason: "budget", Excluded: sample.excluded}
			}
			if !slices.Equal(candidateIDs(selected), sample.ids) || used != sample.used || stats != want || !reflect.DeepEqual(truncation, wantTruncation) {
				t.Fatalf("ids=%v used=%d stats=%+v truncation=%+v", candidateIDs(selected), used, stats, truncation)
			}
		})
	}
}

func TestEvidenceFlowFoldedBridgeAndRankContract(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000b30")
	outside := testID(t, "019a0000-0000-7000-8000-000000000b3f")
	entry := testContext(t, graphID, "019a0000-0000-7000-8000-000000000b31", "가😀", 0)
	entry.Layer, entry.Derived = model.LayerDerived, &model.DerivedAttributes{
		Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateOpinion, DerivedFrom: []model.ID{outside},
	}
	duplicate := entry
	duplicate.ID = testID(t, "019a0000-0000-7000-8000-000000000b32")
	duplicate.RecordedAt = entry.RecordedAt.Add(10 * time.Second)
	near := testContext(t, graphID, "019a0000-0000-7000-8000-000000000b33", "👩\u200d💻", 9)
	orphan := testContext(t, graphID, "019a0000-0000-7000-8000-000000000b34", "끊", 8)
	source := testContext(t, graphID, "019a0000-0000-7000-8000-000000000b35", "e\u0301", 7)
	tail := testContext(t, graphID, "019a0000-0000-7000-8000-000000000b36", "끝", 6)
	near.Layer, orphan.Layer, tail.Layer = model.LayerEvent, model.LayerEvent, model.LayerEvent
	edges := []model.HopEdge{
		{FromID: duplicate.ID, ToID: entry.ID, Kind: "supersedes"},
		{FromID: orphan.ID, ToID: duplicate.ID, Kind: "has_member"},
		{FromID: near.ID, ToID: entry.ID, Kind: "has_member"},
		{FromID: near.ID, ToID: source.ID, Kind: "has_member"},
		{FromID: near.ID, ToID: tail.ID, Kind: "precedes"},
		{FromID: entry.ID, ToID: outside, Kind: "derived_from"},
		{FromID: outside, ToID: orphan.ID, Kind: "has_member"},
	}
	// 접기 뒤 통합 순위는 entry·near·orphan·source·tail이다. orphan은 접힌
	// duplicate나 후보 밖 outside를 다시 가져오지 않는 한 진입점에 닿지 않는다.
	values := []model.Context{entry, duplicate, near, orphan, source, tail}
	postFold := []model.Context{entry, near, orphan, source, tail}
	for _, enabled := range []bool{false, true} {
		for _, budget := range []int{0, 1, 2, 5, 6, 7, 8, 9} {
			t.Run(fmt.Sprintf("enabled_%t/budget_%d", enabled, budget), func(t *testing.T) {
				wantRanks := []int{1, 2, 3, 4, 5}
				if enabled {
					wantRanks = []int{1, 2, 4, 5}
				}
				used, excluded := 0, 0
				var ranks []int
				// 기대 예산은 고정 본문 길이 표에서 계산하며 운영 절단기를 사용하지 않는다.
				lengths := []int{2, 3, 1, 2, 1}
				for _, rank := range wantRanks {
					if budget > 0 && used+lengths[rank-1] > budget {
						excluded = 5 - len(ranks)
						break
					}
					used += lengths[rank-1]
					ranks = append(ranks, rank)
				}
				unreachable := 0
				// 단절 수는 예산 절단 전에 실제로 검토한 대표 후보만 센다.
				if enabled && (budget == 0 || budget >= 5) {
					unreachable = 1
				}
				var baseline *Flow
				for _, execution := range []Execution{ExecutionParallel, ExecutionSequential} {
					for _, reverse := range []bool{false, true} {
						contexts, inputEdges := slices.Clone(values), slices.Clone(edges)
						if reverse {
							slices.Reverse(contexts)
							slices.Reverse(inputEdges)
						}
						database := &routingContractStore{fakeStore: &fakeStore{
							keyword: []model.SearchCandidate{{Context: entry}}, time: []model.SearchCandidate{{Context: entry}},
							hops: model.HopResult{Contexts: contexts, Edges: inputEdges, Distances: map[model.ID]int{
								entry.ID: 0, duplicate.ID: 1, near.ID: 1, orphan.ID: 2, source.ID: 2, tail.ID: 2,
							}, Truncated: true, Boundary: 2},
						}}
						config := Config{Execution: execution, CandidateLimit: 10, FoldThreshold: 0.9, GraphStage: GraphStageRelations, EvidencePathSelection: enabled}
						service, err := New(database, fakeEmbedder{}, config, nil)
						if err != nil {
							t.Fatal(err)
						}
						flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", Scope: "local", Budget: budget, MaxHops: 3, MaxHopNodes: 10})
						if err != nil {
							t.Fatal(err)
						}
						var gotRanks []int
						present := map[model.ID]bool{}
						for _, item := range flow.Contexts {
							gotRanks = append(gotRanks, item.Rank)
							wantContext := postFold[item.Rank-1]
							if !reflect.DeepEqual(item.Value, wantContext) || !slices.Equal(item.OriginKinds, []model.OriginKind{model.OriginKindUserUtterance}) {
								t.Fatalf("본문·출처가 바뀌었다: %+v", item)
							}
							present[item.Value.ID] = true
							wantDegree := []int{1, 3, 0, 1, 1}[item.Rank-1]
							if item.CandidateDegree != wantDegree || item.FoldedCount != boolInt(item.Rank == 1) {
								t.Fatalf("접기·차수=%+v", item)
							}
							wantChannels, wantDistance := []string{"graph"}, 2
							if item.Rank == 1 {
								wantChannels, wantDistance = []string{"keyword", "time"}, 0
							} else if item.Rank == 2 {
								wantDistance = 1
							}
							if !slices.Equal(item.MatchedChannels, wantChannels) || item.EntryDistance != wantDistance {
								t.Fatalf("기여 채널·거리=%+v", item)
							}
						}
						var wantTruncation *Truncation
						if excluded > 0 {
							wantTruncation = &Truncation{Reason: "budget", Excluded: excluded}
						}
						if !slices.Equal(gotRanks, ranks) || flow.BudgetUsed != used || !reflect.DeepEqual(flow.Truncation, wantTruncation) || flow.Selection != (Selection{Candidates: 5, Selected: len(ranks), Unreachable: unreachable}) {
							t.Fatalf("ranks=%v want=%v used=%d want=%d truncation=%+v want=%+v selection=%+v unreachable=%d", gotRanks, ranks, flow.BudgetUsed, used, flow.Truncation, wantTruncation, flow.Selection, unreachable)
						}
						var wantEdges []model.HopEdge
						for _, edge := range edges[2:5] {
							if present[edge.FromID] && present[edge.ToID] {
								wantEdges = append(wantEdges, edge)
								if !slices.Contains(flow.Edges, edge) {
									t.Fatalf("선택 경로의 간선 누락: %+v", edge)
								}
							}
						}
						if len(flow.Edges) != len(wantEdges) || !slices.Equal(flow.EntryPoints, []model.ID{entry.ID}) || flow.HopBoundary == nil || *flow.HopBoundary != 2 {
							t.Fatalf("평면 간선·진입점·홉 절단=%+v", flow)
						}
						if database.hopCalls != 1 || database.globalCalls != 0 || !slices.Equal(database.hopStarts, []model.ID{entry.ID}) || database.hopDepth != 3 || database.hopLimit != 10 {
							t.Fatal("선택이 후보 밖 재탐색이나 홉 조건 변경을 유발했다")
						}
						for index := range database.entryCalls {
							if database.entryCalls[index].Load() != 1 {
								t.Fatal("진입 채널 재조회")
							}
						}
						for name, channel := range flow.Channels {
							channel.Latency = 0
							flow.Channels[name] = channel
						}
						if baseline == nil {
							baseline = new(flow)
						} else if !reflect.DeepEqual(*baseline, flow) {
							t.Fatal("병렬·순차 또는 간선/후보 입력 순서에 따라 흐름이 바뀌었다")
						}
					}
				}
			})
		}
	}
}
