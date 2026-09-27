// 근거 경로 보존 선택은 통합 순위 산정과 중복 파생 접기 뒤, 예산 적용 전에 실행하는
// 선택 단계다. 「근거 경로 보존 선택」이 정한 순서를 그대로 따르며 선택을 위해 저장소를
// 다시 탐색하지 않는다.
package search

import (
	"cmp"
	"maps"
	"slices"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
)

// Selection은 「측정」이 요구한 근거 경로 선택 전후 후보 수와 연결 노드 수다. 선택이
// 비활성이면 선택 단위가 후보 하나이므로 연결 노드와 단절 후보는 항상 0이다.
type Selection struct {
	// Candidates 필드에는 중복 파생 접기 뒤 선택에 들어간 후보 수를 둔다.
	Candidates int
	// Selected 필드에는 예산 적용까지 마친 뒤 남은 후보 수를 둔다.
	Selected int
	// Connectors 필드에는 연결 근거 집합을 완성하려고 대표 후보와 함께 담은 경로 노드 수를 둔다.
	Connectors int
	// Unreachable 필드에는 진입점까지 경로가 없어 선택하지 않은 대표 후보 수를 둔다.
	Unreachable int
}

// entryChannels은 진입점으로 세는 채널 이름이다. 그래프 경로 채널은 확장이므로
// 진입점이 아니다.
var entryChannels = []string{"semantic", "keyword", "time", "global_summary"}

// isEntryCandidate는 후보가 진입점 집합에 속하는지 판정한다. 진입 채널이 낸 후보와
// 후보에 포함된 원천이 진입점이다.
func isEntryCandidate(candidate combinedCandidate) bool {
	if candidate.value.Layer == model.LayerSource {
		return true
	}
	return slices.ContainsFunc(entryChannels, func(channel string) bool {
		return slices.Contains(candidate.channels, channel)
	})
}

// candidateSubgraph는 후보 부분 그래프의 인접 목록과 차수다. 차수는 서로 다른 이웃
// 후보의 수로 센다.
type candidateSubgraph struct {
	adjacency map[model.ID][]model.ID
	degrees   map[model.ID]int
}

// buildCandidateSubgraph는 채널이 조회한 간선 가운데 양 끝이 모두 후보인 것만으로
// 부분 그래프를 만든다. 후보 밖 노드를 가져오지 않으므로 저장 질의의 폭이 그대로다.
func buildCandidateSubgraph(candidates []combinedCandidate, results []channelResult) candidateSubgraph {
	present := make(map[model.ID]struct{}, len(candidates))
	degrees := make(map[model.ID]int, len(candidates))
	for _, candidate := range candidates {
		present[candidate.value.ID] = struct{}{}
		degrees[candidate.value.ID] = 0
	}
	neighbors := map[model.ID]map[model.ID]struct{}{}
	for _, result := range results {
		for _, edge := range result.edges {
			if _, found := present[edge.FromID]; !found {
				continue
			}
			if _, found := present[edge.ToID]; !found {
				continue
			}
			if neighbors[edge.FromID] == nil {
				neighbors[edge.FromID] = map[model.ID]struct{}{}
			}
			if neighbors[edge.ToID] == nil {
				neighbors[edge.ToID] = map[model.ID]struct{}{}
			}
			neighbors[edge.FromID][edge.ToID] = struct{}{}
			neighbors[edge.ToID][edge.FromID] = struct{}{}
		}
	}
	adjacency := make(map[model.ID][]model.ID, len(neighbors))
	for id, adjacent := range neighbors {
		adjacency[id] = slices.SortedFunc(maps.Keys(adjacent), compareIDs)
		degrees[id] = len(adjacent)
	}
	return candidateSubgraph{adjacency: adjacency, degrees: degrees}
}

func compareIDs(left, right model.ID) int {
	return cmp.Compare(left.String(), right.String())
}

// pathCost는 같은 길이의 최단 경로 후보를 고르는 비용이다. 새로 필요한 연결 노드
// 수, 경로 노드의 차수 합, 경로의 context_id 배열 사전순으로 비교한다.
type pathCost struct {
	newNodes  int
	degreeSum int
	ids       []model.ID
}

// comparePathCost는 비용이 작은 경로를 고르기 위한 순서를 정한다.
func comparePathCost(left, right pathCost) int {
	if order := cmp.Compare(left.newNodes, right.newNodes); order != 0 {
		return order
	}
	if order := cmp.Compare(left.degreeSum, right.degreeSum); order != 0 {
		return order
	}
	return slices.CompareFunc(left.ids, right.ids, compareIDs)
}

// shortestEntryPath는 진입점이 아닌 대표 후보에서 가장 가까운 진입점까지의 최단 경로를
// 대표 후보부터 순서대로 돌려준다. 경로가 없으면 nil이다.
//
// 너비 우선 탐색은 처음 만난 진입점의 거리에서 멈춘다. 그보다 가까운 진입점이 경로
// 중간에 있을 수 없으므로 종료 노드는 정확히 최단 거리의 진입점뿐이다. 비용의 앞 두
// 항은 노드별 합이고 같은 노드에서 갈라지는 경로는 앞부분 식별자가 같으므로, 먼 거리의
// 노드부터 이어지는 최소 비용 경로를 확정해 올라오면 전체 최소 비용 경로가 된다.
func shortestEntryPath(representative model.ID, graph candidateSubgraph, entries, included map[model.ID]struct{}) []model.ID {
	distances := map[model.ID]int{representative: 0}
	layers := [][]model.ID{{representative}}
	nearest := -1
	for distance := 0; nearest < 0 && distance < len(layers); distance++ {
		var next []model.ID
		for _, current := range layers[distance] {
			for _, neighbor := range graph.adjacency[current] {
				if _, visited := distances[neighbor]; visited {
					continue
				}
				distances[neighbor] = distance + 1
				next = append(next, neighbor)
				if _, isEntry := entries[neighbor]; isEntry {
					nearest = distance + 1
				}
			}
		}
		if len(next) > 0 {
			layers = append(layers, next)
		}
	}
	if nearest < 0 {
		return nil
	}
	newCount := func(id model.ID) int {
		if _, used := included[id]; used {
			return 0
		}
		return 1
	}
	best := make(map[model.ID]pathCost, len(distances))
	for _, id := range layers[nearest] {
		if _, isEntry := entries[id]; isEntry {
			best[id] = pathCost{newNodes: newCount(id), degreeSum: graph.degrees[id], ids: []model.ID{id}}
		}
	}
	for distance := nearest - 1; distance >= 0; distance-- {
		for _, id := range layers[distance] {
			chosen, found := pathCost{}, false
			for _, next := range graph.adjacency[id] {
				rest, reachable := best[next]
				if !reachable || distances[next] != distance+1 {
					continue
				}
				cost := pathCost{newNodes: rest.newNodes + newCount(id), degreeSum: rest.degreeSum + graph.degrees[id], ids: append([]model.ID{id}, rest.ids...)}
				if !found || comparePathCost(cost, chosen) < 0 {
					chosen, found = cost, true
				}
			}
			if found {
				best[id] = chosen
			}
		}
	}
	return best[representative].ids
}

// selectEvidenceSets는 후보를 통합 순위 순서로 순회하며 연결 근거 집합을 만들고 집합
// 전체가 남은 예산에 들 때만 담는다. 처음 들지 않는 집합에서 멈추고 뒤 집합으로 채우지
// 않는다. 진입점까지 경로가 없는 대표 후보는 선택에서 제외한다. 예산 절단 표시의 제외
// 수는 비활성 구성과 같이 응답에 들지 못한 후보 수다. 선택 결과는 집합 순서대로 담기며
// 통합 순위 정렬은 호출자가 한다.
func selectEvidenceSets(candidates []combinedCandidate, graph candidateSubgraph, budget int) ([]combinedCandidate, int, *Truncation, Selection) {
	byID := make(map[model.ID]combinedCandidate, len(candidates))
	entries := make(map[model.ID]struct{}, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.value.ID] = candidate
		if isEntryCandidate(candidate) {
			entries[candidate.value.ID] = struct{}{}
		}
	}
	included := make(map[model.ID]struct{}, len(candidates))
	selected := make([]combinedCandidate, 0, len(candidates))
	stats := Selection{Candidates: len(candidates)}
	used := 0
	for _, representative := range candidates {
		if _, already := included[representative.value.ID]; already {
			continue
		}
		set := []combinedCandidate{representative}
		if _, isEntry := entries[representative.value.ID]; !isEntry {
			path := shortestEntryPath(representative.value.ID, graph, entries, included)
			if path == nil {
				stats.Unreachable++
				continue
			}
			for _, id := range path[1:] {
				if _, already := included[id]; !already {
					set = append(set, byID[id])
				}
			}
		}
		total := 0
		for _, candidate := range set {
			total += utf8.RuneCountInString(candidate.value.Body)
		}
		if budget > 0 && used+total > budget {
			stats.Selected = len(selected)
			return selected, used, &Truncation{Reason: "budget", Excluded: len(candidates) - len(selected)}, stats
		}
		used += total
		for _, candidate := range set {
			included[candidate.value.ID] = struct{}{}
		}
		selected = append(selected, set...)
		stats.Connectors += len(set) - 1
	}
	stats.Selected = len(selected)
	return selected, used, nil, stats
}
