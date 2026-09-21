// 한 구성에서 질의 집합을 한 회차 돌려 「검색 품질 평가」의 네 지표를 잰다.
package main

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
)

// useCaseMetrics는 사용 사례 하나의 한 회차 측정값이다.
//
// 예산 효율을 비율이 아니라 문자 수로 두는 이유는 「검색 품질 평가」가 그 지표를
// "같은 재현율에 쓴 문자 수"로 정의했기 때문이다. 값이 작을수록 좋다.
type useCaseMetrics struct {
	Queries        int                `json:"queries"`
	Recall         float64            `json:"recall"`
	ReciprocalRank float64            `json:"reciprocal_rank"`
	BudgetPerHit   float64            `json:"budget_per_hit"`
	Channels       map[string]float64 `json:"channel_contribution"`
	Failures       map[string]int     `json:"channel_failures"`
}

// channelDisabled는 비교 단계 구성으로 채널을 끈 상태다. `search` 패키지가 응답에
// 싣는 값과 같아야 하며, 그 패키지도 이 상태를 모든 채널 실패 판정에서 제외한다.
const channelDisabled = "disabled"

// runMetrics는 한 회차 전체의 측정값이다. 소요 시간을 밀리초 정수로 두는 이유는
// time.Duration에 JSON 표현이 정해져 있지 않기 때문이다.
type runMetrics struct {
	Stage        string                    `json:"stage"`
	Repeat       int                       `json:"repeat"`
	DurationMS   int64                     `json:"duration_ms"`
	UseCases     map[string]useCaseMetrics `json:"use_cases"`
	QuerySamples []queryMetrics            `json:"query_samples"`
}

// queryMetrics는 반복이 결정적인 실행에서도 질의 사이의 차이로 단계 효과를 판정할 수
// 있게 하는 표본이다. 본문은 싣지 않고 데이터셋 식별자와 지표만 남긴다.
type queryMetrics struct {
	ID             string  `json:"id"`
	UseCase        string  `json:"use_case"`
	Recall         float64 `json:"recall"`
	ReciprocalRank float64 `json:"reciprocal_rank"`
	BudgetPerHit   float64 `json:"budget_per_hit"`
}

type measurement struct {
	UseCases     map[string]useCaseMetrics
	QuerySamples []queryMetrics
}

// measure는 질의를 하나씩 돌려 사용 사례별로 묶는다. 질의문과 컨텍스트 본문은
// 어디에도 남기지 않는다. 「로그에서 제외하는 것」이 `body`를 지표에 넣지 못하게
// 했고 「업무 효과 평가」가 같은 제외를 평가에도 요구한다.
func measure(ctx context.Context, service *search.Service, graph loadedGraph, queries querySet, budget, maxHops, maxHopNodes int) (measurement, error) {
	type accumulator struct {
		queries        int
		recall         float64
		reciprocalRank float64
		budgetUsed     int
		hits           int
		channels       map[string]int
		failures       map[string]int
	}
	totals := map[string]*accumulator{}
	for _, useCase := range useCases {
		totals[useCase] = &accumulator{channels: map[string]int{}, failures: map[string]int{}}
	}
	querySamples := make([]queryMetrics, 0, len(queries.Queries))
	for _, query := range queries.Queries {
		input := search.Input{
			GraphID:     graph.GraphID,
			WorkContext: query.WorkContext,
			Budget:      budget,
			Scope:       query.Scope,
			MaxHops:     maxHops,
			MaxHopNodes: maxHopNodes,
		}
		if query.AsOf != nil {
			input.AsOf = query.AsOf.UTC()
		}
		flow, err := service.Flow(ctx, input)
		if err != nil {
			return measurement{}, fmt.Errorf("질의 %q 검색: %w", query.ID, err)
		}
		total := totals[query.UseCase]
		total.queries++
		answers := answerIDs(query, graph.Keys)
		hits, best := scoreFlow(flow, answers)
		total.hits += hits
		total.recall += float64(hits) / float64(len(answers))
		if best > 0 {
			total.reciprocalRank += 1 / float64(best)
		}
		total.budgetUsed += flow.BudgetUsed
		sample := queryMetrics{ID: query.ID, UseCase: query.UseCase, Recall: float64(hits) / float64(len(answers)), BudgetPerHit: -1}
		if best > 0 {
			sample.ReciprocalRank = 1 / float64(best)
		}
		if hits > 0 {
			sample.BudgetPerHit = float64(flow.BudgetUsed) / float64(hits)
		}
		querySamples = append(querySamples, sample)
		for _, name := range slices.Sorted(maps.Keys(flow.Channels)) {
			channel := flow.Channels[name]
			total.channels[name] += channel.Contribution
			if countsAsFailure(channel.Failure) {
				total.failures[name]++
			}
		}
	}
	result := map[string]useCaseMetrics{}
	for useCase, total := range totals {
		if total.queries == 0 {
			continue
		}
		metrics := useCaseMetrics{
			Queries:        total.queries,
			Recall:         total.recall / float64(total.queries),
			ReciprocalRank: total.reciprocalRank / float64(total.queries),
			Channels:       contributionRatio(total.channels),
			Failures:       total.failures,
		}
		// 정답을 하나도 못 찾으면 "같은 재현율에 쓴 문자 수"가 정의되지 않는다.
		// 0으로 두면 가장 좋은 값과 구분되지 않으므로 음수 표식으로 남긴다.
		metrics.BudgetPerHit = -1
		if total.hits > 0 {
			metrics.BudgetPerHit = float64(total.budgetUsed) / float64(total.hits)
		}
		result[useCase] = metrics
	}
	return measurement{UseCases: result, QuerySamples: querySamples}, nil
}

// countsAsFailure는 채널 상태를 실패로 셀지 정한다. 비교 단계가 끈 채널은 실패가
// 아니다. 조회가 실패한 것과 같게 세면 기준선의 그래프 채널이 매 질의 실패한 것처럼
// 보여 단계별 실패율을 읽을 수 없다.
func countsAsFailure(failure string) bool {
	return failure != "" && failure != channelDisabled
}

// answerIDs는 데이터셋의 정답 key를 적재된 식별자로 바꾼다.
func answerIDs(query querySpec, keys map[string]model.ID) map[model.ID]struct{} {
	answers := make(map[model.ID]struct{}, len(query.Answers))
	for _, key := range query.Answers {
		answers[keys[key]] = struct{}{}
	}
	return answers
}

// scoreFlow는 반환 결과에 들어간 정답 수와 가장 앞선 정답의 통합 순위를 낸다.
// 순위는 1부터이며 정답이 하나도 없으면 0이다.
func scoreFlow(flow search.Flow, answers map[model.ID]struct{}) (int, int) {
	hits := 0
	best := 0
	for _, value := range flow.Contexts {
		if _, correct := answers[value.Value.ID]; !correct {
			continue
		}
		hits++
		if best == 0 || value.Rank < best {
			best = value.Rank
		}
	}
	return hits, best
}

// contributionRatio는 채널 기여를 전체 대비 비율로 바꾼다. 절대 건수는 질의 수에
// 따라 달라져 단계 사이 비교에 쓸 수 없다.
func contributionRatio(counts map[string]int) map[string]float64 {
	total := 0
	for _, value := range counts {
		total += value
	}
	ratio := make(map[string]float64, len(counts))
	for name, value := range counts {
		if total == 0 {
			ratio[name] = 0
			continue
		}
		ratio[name] = float64(value) / float64(total)
	}
	return ratio
}
