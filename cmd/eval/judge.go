// 반복 회차를 모아 기준선 대비 개선폭과 유의성으로 판정한다.
//
// 절대 점수로 판정하지 않는 이유는 「검증 원칙」이 그렇게 확정했기 때문이다. 지금
// 목표 점수를 정할 측정 근거가 없으므로, 단계마다 이전 단계와의 차이와 그 차이의
// 신뢰구간만 본다.
package main

import (
	"maps"
	"math"
	"slices"
)

// 기존 업무 집계 입력의 최소 대응 표본 수다. 초기 평가 실행의 10회 반복은
// initialEvaluationRepeats로 별도 검증하며 표본 수와 반복 수를 혼동하지 않는다.
const minimumRepeats = 3

// metricNames는 판정에 쓰는 지표와 그 방향이다. 예산 효율은 문자 수, 단절 근거·허브
// 편중·중복 컨텍스트는 비율, 지연은 시간이므로 값이 작을수록 좋고 나머지는 클수록
// 좋다. 모든 지표가 음수가 될 수 없으므로 음수는 정의되지 않은 값으로 보고 뺀다.
var metricNames = []struct {
	name          string
	lowerIsBetter bool
	sample        func(queryMetrics) float64
}{
	{"recall", false, func(q queryMetrics) float64 { return q.Recall }},
	{"reciprocal_rank", false, func(q queryMetrics) float64 { return q.ReciprocalRank }},
	{"budget_used", true, func(q queryMetrics) float64 { return float64(q.BudgetUsed) }},
	{"budget_per_hit", true, func(q queryMetrics) float64 { return q.BudgetPerHit }},
	{"evidence_completeness", false, func(q queryMetrics) float64 { return q.EvidenceCompleteness }},
	{"path_continuity", false, func(q queryMetrics) float64 { return q.PathContinuity }},
	{"disconnected_ratio", true, func(q queryMetrics) float64 { return q.DisconnectedRatio }},
	{"hub_concentration", true, func(q queryMetrics) float64 { return q.HubConcentration }},
	{"duplicate_ratio", true, func(q queryMetrics) float64 { return q.DuplicateRatio }},
	{"latency_ms", true, func(q queryMetrics) float64 { return q.LatencyMS }},
	{"misroute_rate", true, func(q queryMetrics) float64 { return q.Misrouted }},
}

// estimate는 주어진 유효 표본의 집계다. 단계 판정에서는 독립 단위를 센다.
type estimate struct {
	Samples        int     `json:"samples"`
	DegreesFreedom int     `json:"degrees_of_freedom"`
	Status         string  `json:"status"`
	Mean           float64 `json:"mean"`
	StdDev         float64 `json:"std_dev"`
	Interval       float64 `json:"interval_half_width"`
}

// comparison은 한 단계를 이전 단계와 견준 결과다.
type comparison struct {
	Metric         string  `json:"metric"`
	DegreesFreedom int     `json:"degrees_of_freedom,omitzero"`
	Status         string  `json:"status,omitempty"`
	Samples        int     `json:"samples"`
	Baseline       float64 `json:"baseline_mean"`
	Current        float64 `json:"current_mean"`
	Difference     float64 `json:"difference"`
	Interval       float64 `json:"interval_half_width"`
	Improved       bool    `json:"improved"`
	Significant    bool    `json:"significant"`
}

// stageJudgement는 한 단계의 사용 사례별 집계와 이전 단계 대비 판정이다.
type stageJudgement struct {
	Stage       string                         `json:"stage"`
	ComparedTo  string                         `json:"compared_to,omitzero"`
	Estimates   map[string]map[string]estimate `json:"estimates"`
	Comparisons map[string][]comparison        `json:"comparisons,omitzero"`
}

// judge는 단계 목록을 받은 순서대로 이전 단계와 견준다. 「그래프 효과 비교」가
// 기준선에서 채널을 하나씩 더하는 누적 비교로 확정했으므로 비교 대상은 항상 바로
// 앞 단계다.
func judge(stages []string, runs map[string][]runMetrics) []stageJudgement {
	judgements := make([]stageJudgement, 0, len(stages))
	samples := map[string]map[string]map[string][]float64{}
	querySamples := map[string]map[string]map[string]map[string]float64{}
	for _, stage := range stages {
		samples[stage] = collectSamples(runs[stage])
		querySamples[stage] = collectQuerySamples(runs[stage])
	}
	for index, stage := range stages {
		judgement := stageJudgement{Stage: stage, Estimates: map[string]map[string]estimate{}}
		for useCase, metrics := range samples[stage] {
			judgement.Estimates[useCase] = map[string]estimate{}
			for name, values := range metrics {
				judgement.Estimates[useCase][name] = estimateOf(values)
			}
		}
		if index > 0 {
			previous := stages[index-1]
			judgement.ComparedTo = previous
			left, right := pairedIndependentSamples(querySamples[previous], querySamples[stage], queryUnits(runs[stage]))
			judgement.Comparisons = compareStages(left, right)
		}
		judgements = append(judgements, judgement)
	}
	return judgements
}

// collectQuerySamples는 같은 질의의 반복값을 먼저 평균낸다. 독립 단위 묶기는 다음 단계다.
// 반복값을 그대로 쌓으면 결정적인 검색에서 같은 값을 여러 번 복제해 표본 수를 부풀린다.
func collectQuerySamples(runs []runMetrics) map[string]map[string]map[string]float64 {
	totals := map[string]map[string]map[string][]float64{}
	for _, run := range runs {
		for _, sample := range run.QuerySamples {
			if totals[sample.UseCase] == nil {
				totals[sample.UseCase] = map[string]map[string][]float64{}
			}
			if totals[sample.UseCase][sample.ID] == nil {
				totals[sample.UseCase][sample.ID] = map[string][]float64{}
			}
			for _, metric := range metricNames {
				if v := metric.sample(sample); validMetric(v) {
					totals[sample.UseCase][sample.ID][metric.name] = append(totals[sample.UseCase][sample.ID][metric.name], v)
				}
			}
		}
	}
	result := map[string]map[string]map[string]float64{}
	for useCase, queries := range totals {
		result[useCase] = map[string]map[string]float64{}
		for id, metrics := range queries {
			result[useCase][id] = map[string]float64{}
			for name, values := range metrics {
				if len(values) == len(runs) {
					result[useCase][id][name] = mean(values)
				}
			}
		}
	}
	return result
}

// collectSamples의 표본은 회차가 아니라 반복 평균을 묶은 독립 대화·과업이다.
func collectSamples(runs []runMetrics) map[string]map[string][]float64 {
	units := independentSamples(collectQuerySamples(runs), queryUnits(runs))
	result := map[string]map[string][]float64{}
	for useCase, groups := range units {
		result[useCase] = map[string][]float64{}
		for _, metric := range metricNames {
			result[useCase][metric.name] = []float64{}
			for _, id := range slices.Sorted(maps.Keys(groups)) {
				if value, ok := groups[id][metric.name]; ok {
					result[useCase][metric.name] = append(result[useCase][metric.name], value)
				}
			}
		}
	}
	return result
}

func compareStages(baseline, current map[string]map[string]map[string]float64) map[string][]comparison {
	comparisons := map[string][]comparison{}
	for useCase, queries := range current {
		previous, present := baseline[useCase]
		if !present {
			continue
		}
		for _, metric := range metricNames {
			left, right := make([]float64, 0, len(queries)), make([]float64, 0, len(queries))
			for _, id := range slices.Sorted(maps.Keys(queries)) {
				values := queries[id]
				currentValue, currentPresent := values[metric.name]
				previousValues, previousPresent := previous[id]
				previousValue, metricPresent := previousValues[metric.name]
				if !currentPresent || !previousPresent || !metricPresent {
					continue
				}
				left = append(left, previousValue)
				right = append(right, currentValue)
			}
			difference, interval := pairedInterval(left, right)
			improved := len(left) > 0 && difference > 0
			if metric.lowerIsBetter {
				improved = len(left) > 0 && difference < 0
			}
			comparisons[useCase] = append(comparisons[useCase], comparison{
				Metric:         metric.name,
				DegreesFreedom: max(0, len(left)-1), Status: statisticalStatus(len(left)),
				Samples:    len(left),
				Baseline:   mean(left),
				Current:    mean(right),
				Difference: difference,
				Interval:   interval,
				Improved:   improved,
				// 차이의 신뢰구간이 0을 품지 않을 때만 유의한 것으로 본다.
				Significant: interval >= 0 && math.Abs(difference) > interval,
			})
		}
	}
	return comparisons
}

func estimateOf(values []float64) estimate {
	values = slices.DeleteFunc(slices.Clone(values), func(v float64) bool { return !validMetric(v) })
	result := estimate{Samples: len(values), DegreesFreedom: max(0, len(values)-1), Status: statisticalStatus(len(values)), Mean: mean(values), StdDev: undefinedMetric, Interval: undefinedMetric}
	if len(values) < 2 {
		return result
	}
	result.StdDev = math.Sqrt(variance(values))
	if critical := criticalValue(len(values) - 1); critical >= 0 {
		result.Interval = critical * result.StdDev / math.Sqrt(float64(len(values)))
	}
	return result
}

// pairedInterval은 같은 질의의 단계별 차이로 평균 개선폭과 95% 신뢰구간 반폭을 낸다.
func pairedInterval(baseline, current []float64) (float64, float64) {
	if len(baseline) != len(current) || len(baseline) == 0 {
		return undefinedMetric, undefinedMetric
	}
	differences := make([]float64, len(baseline))
	for index := range baseline {
		if !validMetric(baseline[index]) || !validMetric(current[index]) {
			return undefinedMetric, undefinedMetric
		}
		differences[index] = current[index] - baseline[index]
	}
	difference := mean(differences)
	if len(differences) < 2 {
		return difference, undefinedMetric
	}
	standardError := math.Sqrt(variance(differences) / float64(len(differences)))
	critical := criticalValue(len(differences) - 1)
	if critical < 0 {
		return difference, undefinedMetric
	}
	return difference, critical * standardError
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return undefinedMetric
	}
	total := 0.0
	for _, value := range slices.Sorted(slices.Values(values)) {
		total += value
	}
	return total / float64(len(values))
}

func variance(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	average := mean(values)
	total := 0.0
	for _, value := range slices.Sorted(slices.Values(values)) {
		total += (value - average) * (value - average)
	}
	return total / float64(len(values)-1)
}

// marginal은 한 단계가 바로 앞 단계에 더한 한계 기여다. 「검색 품질 평가」가 정답·필수
// 근거 증가량과 문자 수·지연 증가량을 나란히 남기고 하나의 합성 점수로 줄이지 말라고
// 정했다. 회수 수와 비용은 대응 회차의 차이를 먼저 질의별로 평균한다.
type marginal struct {
	Stage          string  `json:"stage"`
	ComparedTo     string  `json:"compared_to"`
	UseCase        string  `json:"use_case"`
	Queries        int     `json:"queries"`
	Recovered      float64 `json:"newly_recovered"`
	Lost           float64 `json:"lost"`
	AddedChars     float64 `json:"added_chars_per_query"`
	AddedLatencyMS float64 `json:"added_latency_ms_per_query"`
	LatencyQueries int     `json:"latency_queries"`
}

// marginals는 단계 목록을 받은 순서대로 바로 앞 단계와 견준 한계 기여를 낸다.
func marginals(stages []string, runs map[string][]runMetrics) []marginal {
	result := make([]marginal, 0)
	for index := 1; index < len(stages); index++ {
		previous, current := runs[stages[index-1]], runs[stages[index]]
		if len(previous) == 0 || len(current) == 0 {
			continue
		}
		before, after := queryCosts(previous), queryCosts(current)
		for _, useCase := range useCases {
			entry := marginal{Stage: stages[index], ComparedTo: stages[index-1], UseCase: useCase}
			chars, latencies := []float64{}, []float64{}
			recoveryMeasured := true
			for _, id := range slices.Sorted(maps.Keys(after[useCase])) {
				cost := after[useCase][id]
				base, present := before[useCase][id]
				if !present {
					continue
				}
				entry.Queries++
				chars = append(chars, cost.chars-base.chars)
				if validMetric(cost.latency) && validMetric(base.latency) {
					latencies = append(latencies, cost.latency-base.latency)
				}
				gained, lost := recoveredDifference(previous, current, id)
				if gained < 0 {
					recoveryMeasured = false
				} else {
					entry.Recovered += gained
					entry.Lost += lost
				}
			}
			if entry.Queries == 0 {
				continue
			}
			entry.AddedChars = mean(chars)
			entry.AddedLatencyMS = mean(latencies)
			entry.LatencyQueries = len(latencies)
			if !recoveryMeasured {
				entry.Recovered, entry.Lost = undefinedMetric, undefinedMetric
			}
			result = append(result, entry)
		}
	}
	return result
}

type queryCost struct {
	chars   float64
	latency float64
}

// queryCosts는 사용 사례와 질의별로 반복 회차의 문자 수와 지연을 평균낸다.
func queryCosts(runs []runMetrics) map[string]map[string]queryCost {
	type costs struct{ chars, latencies []float64 }
	totals := map[string]map[string]costs{}
	for _, run := range runs {
		for _, sample := range run.QuerySamples {
			if totals[sample.UseCase] == nil {
				totals[sample.UseCase] = map[string]costs{}
			}
			cost := totals[sample.UseCase][sample.ID]
			cost.chars = append(cost.chars, float64(sample.BudgetUsed))
			if validMetric(sample.LatencyMS) {
				cost.latencies = append(cost.latencies, sample.LatencyMS)
			}
			totals[sample.UseCase][sample.ID] = cost
		}
	}
	result := map[string]map[string]queryCost{}
	for useCase, queries := range totals {
		result[useCase] = map[string]queryCost{}
		for id, costs := range queries {
			latency := undefinedMetric
			if len(costs.latencies) == len(runs) {
				latency = mean(costs.latencies)
			}
			result[useCase][id] = queryCost{chars: mean(costs.chars), latency: latency}
		}
	}
	return result
}

func recoveredDifference(before, after []runMetrics, id string) (float64, float64) {
	if len(before) != len(after) || len(before) == 0 {
		return undefinedMetric, undefinedMetric
	}
	previous := map[int]runMetrics{}
	for _, run := range before {
		previous[run.Repeat] = run
	}
	gained, lost := []float64{}, []float64{}
	for _, run := range after {
		base, ok := previous[run.Repeat]
		left, leftOK := base.recovered[id]
		right, rightOK := run.recovered[id]
		if !ok || !leftOK || !rightOK {
			return undefinedMetric, undefinedMetric
		}
		add, remove := 0.0, 0.0
		for key := range right {
			if _, ok := left[key]; !ok {
				add++
			}
		}
		for key := range left {
			if _, ok := right[key]; !ok {
				remove++
			}
		}
		gained, lost = append(gained, add), append(lost, remove)
	}
	return mean(gained), mean(lost)
}

func statisticalStatus(n int) string {
	if n == 0 {
		return "unmeasured"
	}
	if n == 1 {
		return "insufficient_units"
	}
	return "measured"
}
