// 한 구성에서 질의 집합을 한 회차 돌려 「검색 품질 평가」의 검색·구조·비용 지표를 잰다.
package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
)

// useCaseMetrics는 사용 사례 하나의 한 회차 측정값이다.
//
// 예산 효율을 비율이 아니라 문자 수로 두는 이유는 「검색 품질 평가」가 그 지표를
// "같은 재현율에 쓴 문자 수"로 정의했기 때문이다. 값이 작을수록 좋다.
//
// 구조 지표와 지연은 질의별 값의 평균이며 정의되지 않은 질의는 뺀다. 모든 질의에서
// 정의되지 않으면 음수 표식이다. 선택 측정값은 질의당 평균이다.
type useCaseMetrics struct {
	Queries              int                `json:"queries"`
	Recall               float64            `json:"recall"`
	ReciprocalRank       float64            `json:"reciprocal_rank"`
	BudgetPerHit         float64            `json:"budget_per_hit"`
	EvidenceCompleteness float64            `json:"evidence_completeness"`
	PathContinuity       float64            `json:"path_continuity"`
	DisconnectedRatio    float64            `json:"disconnected_ratio"`
	HubConcentration     float64            `json:"hub_concentration"`
	DuplicateRatio       float64            `json:"duplicate_ratio"`
	LatencyMS            float64            `json:"latency_ms"`
	LatencyP95MS         float64            `json:"latency_p95_ms"`
	SelectionCandidates  float64            `json:"selection_candidates"`
	SelectionSelected    float64            `json:"selection_selected"`
	SelectionConnectors  float64            `json:"selection_connectors"`
	SelectionUnreachable float64            `json:"selection_unreachable"`
	MisrouteRate         float64            `json:"misroute_rate"`
	Routes               map[string]float64 `json:"route_ratio"`
	RouteReasons         map[string]int     `json:"route_reasons"`
	Channels             map[string]float64 `json:"channel_contribution"`
	Failures             map[string]int     `json:"channel_failures"`
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
	// recovered에는 질의별로 반환된 정답·필수 근거 key를 둔다. 확장 단계의 한계 기여를
	// 계산할 때만 쓰며 결과 파일에는 싣지 않는다.
	recovered map[string]map[string]struct{}
}

// queryMetrics는 반복이 결정적인 실행에서도 질의 사이의 차이로 단계 효과를 판정할 수
// 있게 하는 표본이다. 본문은 싣지 않고 데이터셋 식별자와 지표만 남긴다.
type queryMetrics struct {
	ID                   string  `json:"id"`
	UseCase              string  `json:"use_case"`
	Recall               float64 `json:"recall"`
	ReciprocalRank       float64 `json:"reciprocal_rank"`
	BudgetPerHit         float64 `json:"budget_per_hit"`
	BudgetUsed           int     `json:"budget_used"`
	EvidenceCompleteness float64 `json:"evidence_completeness"`
	PathContinuity       float64 `json:"path_continuity"`
	DisconnectedRatio    float64 `json:"disconnected_ratio"`
	HubConcentration     float64 `json:"hub_concentration"`
	DuplicateRatio       float64 `json:"duplicate_ratio"`
	LatencyMS            float64 `json:"latency_ms"`
	// Route와 RouteReason 필드에는 실행한 검색 경로와 이유 코드만 둔다. 경로 원문은 싣지 않는다.
	Route       string `json:"route"`
	RouteReason string `json:"route_reason"`
	// Misrouted 필드는 오프라인 최적 경로와 다르면 1, 같으면 0이다. 적응형 비교가 아니면
	// 정의되지 않은 값이다.
	Misrouted float64 `json:"misrouted"`
}

type measurement struct {
	UseCases     map[string]useCaseMetrics
	QuerySamples []queryMetrics
	Recovered    map[string]map[string]struct{}
	// Signals에는 질의별 라우터 신호를 둔다. 적응형 비교가 임계값 후보를 오프라인으로 견줄 때만 쓴다.
	Signals map[string]search.RouteSignals
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
		samples        []queryMetrics
		selection      search.Selection
	}
	totals := map[string]*accumulator{}
	for _, useCase := range useCases {
		totals[useCase] = &accumulator{channels: map[string]int{}, failures: map[string]int{}}
	}
	querySamples := make([]queryMetrics, 0, len(queries.Queries))
	recovered := make(map[string]map[string]struct{}, len(queries.Queries))
	signals := make(map[string]search.RouteSignals, len(queries.Queries))
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
		started := time.Now()
		flow, err := service.Flow(ctx, input)
		latency := time.Since(started)
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
		shape := measureStructure(flow, query, graph.Keys)
		sample := queryMetrics{ID: query.ID, UseCase: query.UseCase, Recall: float64(hits) / float64(len(answers)), BudgetPerHit: -1,
			BudgetUsed: flow.BudgetUsed, EvidenceCompleteness: shape.completeness, PathContinuity: shape.continuity,
			DisconnectedRatio: shape.disconnected, HubConcentration: shape.hub, DuplicateRatio: shape.duplicate,
			LatencyMS: float64(latency.Microseconds()) / 1000,
			Route:     string(flow.Route), RouteReason: flow.RouteReason, Misrouted: undefinedMetric}
		if best > 0 {
			sample.ReciprocalRank = 1 / float64(best)
		}
		if hits > 0 {
			sample.BudgetPerHit = float64(flow.BudgetUsed) / float64(hits)
		}
		querySamples = append(querySamples, sample)
		total.samples = append(total.samples, sample)
		total.selection.Candidates += flow.Selection.Candidates
		total.selection.Selected += flow.Selection.Selected
		total.selection.Connectors += flow.Selection.Connectors
		total.selection.Unreachable += flow.Selection.Unreachable
		recovered[query.ID] = recoveredKeys(flow, query, graph.Keys)
		signals[query.ID] = flow.RouteSignals
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
		count := float64(total.queries)
		metrics := useCaseMetrics{
			Queries:              total.queries,
			Recall:               total.recall / count,
			ReciprocalRank:       total.reciprocalRank / count,
			EvidenceCompleteness: definedMean(total.samples, func(sample queryMetrics) float64 { return sample.EvidenceCompleteness }),
			PathContinuity:       definedMean(total.samples, func(sample queryMetrics) float64 { return sample.PathContinuity }),
			DisconnectedRatio:    definedMean(total.samples, func(sample queryMetrics) float64 { return sample.DisconnectedRatio }),
			HubConcentration:     definedMean(total.samples, func(sample queryMetrics) float64 { return sample.HubConcentration }),
			DuplicateRatio:       definedMean(total.samples, func(sample queryMetrics) float64 { return sample.DuplicateRatio }),
			LatencyMS:            definedMean(total.samples, func(sample queryMetrics) float64 { return sample.LatencyMS }),
			LatencyP95MS:         latencyP95(total.samples),
			SelectionCandidates:  float64(total.selection.Candidates) / count,
			SelectionSelected:    float64(total.selection.Selected) / count,
			SelectionConnectors:  float64(total.selection.Connectors) / count,
			SelectionUnreachable: float64(total.selection.Unreachable) / count,
			MisrouteRate:         definedMean(total.samples, func(sample queryMetrics) float64 { return sample.Misrouted }),
			Routes:               routeRatio(total.samples),
			RouteReasons:         routeReasons(total.samples),
			Channels:             contributionRatio(total.channels),
			Failures:             total.failures,
		}
		// 정답을 하나도 못 찾으면 "같은 재현율에 쓴 문자 수"가 정의되지 않는다.
		// 0으로 두면 가장 좋은 값과 구분되지 않으므로 음수 표식으로 남긴다.
		metrics.BudgetPerHit = -1
		if total.hits > 0 {
			metrics.BudgetPerHit = float64(total.budgetUsed) / float64(total.hits)
		}
		result[useCase] = metrics
	}
	return measurement{UseCases: result, QuerySamples: querySamples, Recovered: recovered, Signals: signals}, nil
}

// routeRatio는 사용 사례 안에서 경로별 선택 비율을 낸다.
func routeRatio(samples []queryMetrics) map[string]float64 {
	counts := map[string]int{}
	for _, sample := range samples {
		counts[sample.Route]++
	}
	return contributionRatio(counts)
}

// routeReasons는 사용 사례 안에서 이유 코드별 건수를 낸다.
func routeReasons(samples []queryMetrics) map[string]int {
	counts := map[string]int{}
	for _, sample := range samples {
		counts[sample.RouteReason]++
	}
	return counts
}

// definedMean은 정의된 질의별 값만 평균낸다. 하나도 없으면 정의되지 않은 값이다.
func definedMean(samples []queryMetrics, value func(queryMetrics) float64) float64 {
	total, count := 0.0, 0
	for _, sample := range samples {
		if current := value(sample); current >= 0 {
			total += current
			count++
		}
	}
	if count == 0 {
		return undefinedMetric
	}
	return total / float64(count)
}

// latencyP95는 질의별 지연의 95번째 백분위수를 최근접 순위로 낸다. 「검색 품질 평가」가
// 지연 분포를 기록하라고 했으므로 평균만으로 꼬리를 가리지 않는다.
func latencyP95(samples []queryMetrics) float64 {
	if len(samples) == 0 {
		return undefinedMetric
	}
	latencies := make([]float64, 0, len(samples))
	for _, sample := range samples {
		latencies = append(latencies, sample.LatencyMS)
	}
	slices.Sort(latencies)
	rank := (95*len(latencies) + 99) / 100
	return latencies[rank-1]
}

// recoveredKeys는 반환된 정답과 필수 근거의 key를 모은다.
func recoveredKeys(flow search.Flow, query querySpec, keys map[string]model.ID) map[string]struct{} {
	returned := make(map[model.ID]struct{}, len(flow.Contexts))
	for _, item := range flow.Contexts {
		returned[item.Value.ID] = struct{}{}
	}
	found := map[string]struct{}{}
	for _, key := range slices.Concat(query.Answers, query.Required) {
		if _, ok := returned[keys[key]]; ok {
			found[key] = struct{}{}
		}
	}
	return found
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
