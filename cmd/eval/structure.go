// 「검색 품질 평가」의 구조 지표를 흐름 응답의 원자료로 계산한다. 「검증을 가능하게
// 하는 설계」가 이 지표들을 평가 실행기가 계산하도록 정했다.
package main

import (
	"slices"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
)

// entryChannels는 진입점으로 세는 채널 이름이다. `search`의 근거 경로 보존 선택이 쓰는
// 목록과 같아야 한다. 그래프 경로 채널은 확장이므로 진입점이 아니다.
var entryChannels = []string{"semantic", "keyword", "time", "global_summary"}

// undefinedMetric은 분모가 없어 정의되지 않는 질의별 지표 값이다. 0은 가장 나쁘거나
// 좋은 값과 구분되지 않으므로 음수 표식으로 두고 집계와 판정에서 뺀다.
const undefinedMetric = -1.0

// structure는 질의 하나의 구조 지표다.
type structure struct {
	completeness float64
	continuity   float64
	disconnected float64
	hub          float64
	duplicate    float64
}

// measureStructure는 흐름 응답과 질의의 기대 근거로 구조 지표를 계산한다.
func measureStructure(flow search.Flow, query querySpec, keys map[string]model.ID) structure {
	returned := make(map[model.ID]struct{}, len(flow.Contexts))
	for _, item := range flow.Contexts {
		returned[item.Value.ID] = struct{}{}
	}
	edges := make(map[[2]model.ID]struct{}, len(flow.Edges))
	for _, edge := range flow.Edges {
		edges[pairOf(edge.FromID, edge.ToID)] = struct{}{}
	}
	completeness, continuity := evidenceScore(query, keys, returned, edges)
	return structure{
		completeness: completeness,
		continuity:   continuity,
		disconnected: disconnectedRatio(flow),
		hub:          hubConcentration(flow),
		duplicate:    duplicateRatio(flow),
	}
}

// pairOf는 방향 없는 관계 하나를 식별한다. 기대 경로는 방향을 정하지 않는다. 근거
// 경로는 derived_from처럼 대표에서 원천으로 가는 간선과 precedes처럼 반대로 가는
// 간선이 섞이기 때문이다.
func pairOf(left, right model.ID) [2]model.ID {
	if left.String() > right.String() {
		left, right = right, left
	}
	return [2]model.ID{left, right}
}

// evidenceScore는 근거 완전성과 경로 연속성을 낸다. 근거 완전성은 필수 컨텍스트와 기대
// 경로의 관계 중 반환된 비율이다. 경로 연속성은 첫 컨텍스트인 대표 후보가 반환된 기대
// 경로 가운데 모든 노드와 관계가 반환된 비율이다.
func evidenceScore(query querySpec, keys map[string]model.ID, returned map[model.ID]struct{}, edges map[[2]model.ID]struct{}) (float64, float64) {
	required, found := 0, 0
	for _, key := range query.Required {
		required++
		if _, ok := returned[keys[key]]; ok {
			found++
		}
	}
	started, intact := 0, 0
	for _, path := range query.Paths {
		whole := true
		for index := range len(path) - 1 {
			required++
			_, fromReturned := returned[keys[path[index]]]
			_, toReturned := returned[keys[path[index+1]]]
			_, linked := edges[pairOf(keys[path[index]], keys[path[index+1]])]
			if fromReturned && toReturned && linked {
				found++
				continue
			}
			whole = false
		}
		if _, ok := returned[keys[path[0]]]; ok {
			started++
			if whole {
				intact++
			}
		}
	}
	completeness, continuity := undefinedMetric, undefinedMetric
	if required > 0 {
		completeness = float64(found) / float64(required)
	}
	if started > 0 {
		continuity = float64(intact) / float64(started)
	}
	return completeness, continuity
}

// isEntry는 반환된 컨텍스트가 진입점이나 원천인지 판정한다.
func isEntry(item search.Context) bool {
	if item.Value.Layer == model.LayerSource {
		return true
	}
	return slices.ContainsFunc(item.MatchedChannels, func(channel string) bool {
		return slices.Contains(entryChannels, channel)
	})
}

// disconnectedRatio는 반환된 비진입 컨텍스트 중 반환 부분 그래프 안에서 진입점이나
// 원천에 닿지 못하는 비율이다. 비진입 컨텍스트가 없으면 0이다.
func disconnectedRatio(flow search.Flow) float64 {
	adjacency := map[model.ID][]model.ID{}
	for _, edge := range flow.Edges {
		adjacency[edge.FromID] = append(adjacency[edge.FromID], edge.ToID)
		adjacency[edge.ToID] = append(adjacency[edge.ToID], edge.FromID)
	}
	reached := map[model.ID]struct{}{}
	queue := []model.ID{}
	nonEntries := 0
	for _, item := range flow.Contexts {
		if isEntry(item) {
			reached[item.Value.ID] = struct{}{}
			queue = append(queue, item.Value.ID)
			continue
		}
		nonEntries++
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[current] {
			if _, seen := reached[next]; !seen {
				reached[next] = struct{}{}
				queue = append(queue, next)
			}
		}
	}
	if nonEntries == 0 {
		return 0
	}
	disconnected := 0
	for _, item := range flow.Contexts {
		if _, ok := reached[item.Value.ID]; !ok {
			disconnected++
		}
	}
	return float64(disconnected) / float64(nonEntries)
}

// hubConcentration은 반환 컨텍스트의 후보 부분 그래프 차수에 대한 지니 계수다. 반환
// 컨텍스트가 하나 이하이거나 차수가 모두 0이면 0이다.
func hubConcentration(flow search.Flow) float64 {
	if len(flow.Contexts) <= 1 {
		return 0
	}
	total, spread := 0, 0
	for _, left := range flow.Contexts {
		total += left.CandidateDegree
		for _, right := range flow.Contexts {
			spread += max(left.CandidateDegree-right.CandidateDegree, right.CandidateDegree-left.CandidateDegree)
		}
	}
	if total == 0 {
		return 0
	}
	return float64(spread) / float64(2*len(flow.Contexts)*total)
}

// duplicateRatio는 context_id 통합과 중복 파생 접기 전 후보 중 두 처리로 제거된 비율이다.
// 통합 전 후보 수는 채널별 후보 수의 합이고, 처리 뒤 후보 수는 근거 경로 선택의 입력이다.
func duplicateRatio(flow search.Flow) float64 {
	before := 0
	for _, channel := range flow.Channels {
		before += channel.Candidates
	}
	if before == 0 {
		return 0
	}
	return float64(before-flow.Selection.Candidates) / float64(before)
}
