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

// 「검증」이 각 조건을 최소 세 번 반복하라고 정했다. 신뢰구간의 폭이 판정에 필요한
// 수준보다 넓으면 반복을 늘린다.
const minimumRepeats = 3

// metricNames는 판정에 쓰는 지표와 그 방향이다. 예산 효율은 문자 수, 단절 근거·허브
// 편중·중복 컨텍스트는 비율, 지연은 시간이므로 값이 작을수록 좋고 나머지는 클수록
// 좋다. 모든 지표가 음수가 될 수 없으므로 음수는 정의되지 않은 값으로 보고 뺀다.
var metricNames = []struct {
	name          string
	lowerIsBetter bool
	value         func(useCaseMetrics) float64
	sample        func(queryMetrics) float64
}{
	{"recall", false, func(m useCaseMetrics) float64 { return m.Recall }, func(q queryMetrics) float64 { return q.Recall }},
	{"reciprocal_rank", false, func(m useCaseMetrics) float64 { return m.ReciprocalRank }, func(q queryMetrics) float64 { return q.ReciprocalRank }},
	{"budget_per_hit", true, func(m useCaseMetrics) float64 { return m.BudgetPerHit }, func(q queryMetrics) float64 { return q.BudgetPerHit }},
	{"evidence_completeness", false, func(m useCaseMetrics) float64 { return m.EvidenceCompleteness }, func(q queryMetrics) float64 { return q.EvidenceCompleteness }},
	{"path_continuity", false, func(m useCaseMetrics) float64 { return m.PathContinuity }, func(q queryMetrics) float64 { return q.PathContinuity }},
	{"disconnected_ratio", true, func(m useCaseMetrics) float64 { return m.DisconnectedRatio }, func(q queryMetrics) float64 { return q.DisconnectedRatio }},
	{"hub_concentration", true, func(m useCaseMetrics) float64 { return m.HubConcentration }, func(q queryMetrics) float64 { return q.HubConcentration }},
	{"duplicate_ratio", true, func(m useCaseMetrics) float64 { return m.DuplicateRatio }, func(q queryMetrics) float64 { return q.DuplicateRatio }},
	{"latency_ms", true, func(m useCaseMetrics) float64 { return m.LatencyMS }, func(q queryMetrics) float64 { return q.LatencyMS }},
	{"misroute_rate", true, func(m useCaseMetrics) float64 { return m.MisrouteRate }, func(q queryMetrics) float64 { return q.Misrouted }},
}

// estimate는 한 지표의 반복 집계다.
type estimate struct {
	Repeats  int     `json:"repeats"`
	Mean     float64 `json:"mean"`
	StdDev   float64 `json:"std_dev"`
	Interval float64 `json:"interval_half_width"`
}

// comparison은 한 단계를 이전 단계와 견준 결과다.
type comparison struct {
	Metric      string  `json:"metric"`
	Samples     int     `json:"samples"`
	Baseline    float64 `json:"baseline_mean"`
	Current     float64 `json:"current_mean"`
	Difference  float64 `json:"difference"`
	Interval    float64 `json:"interval_half_width"`
	Improved    bool    `json:"improved"`
	Significant bool    `json:"significant"`
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
			judgement.Comparisons = compareStages(querySamples[previous], querySamples[stage])
		}
		judgements = append(judgements, judgement)
	}
	return judgements
}

// collectQuerySamples는 같은 질의의 반복값을 먼저 평균내 하나의 독립 표본으로 만든다.
// 반복값을 그대로 쌓으면 결정적인 검색에서 같은 값을 여러 번 복제해 표본 수를 부풀린다.
func collectQuerySamples(runs []runMetrics) map[string]map[string]map[string]float64 {
	type values struct {
		total float64
		count int
	}
	totals := map[string]map[string]map[string]values{}
	for _, run := range runs {
		for _, sample := range run.QuerySamples {
			if _, present := totals[sample.UseCase]; !present {
				totals[sample.UseCase] = map[string]map[string]values{}
			}
			if _, present := totals[sample.UseCase][sample.ID]; !present {
				totals[sample.UseCase][sample.ID] = map[string]values{}
			}
			for _, metric := range metricNames {
				value := metric.sample(sample)
				if value < 0 {
					continue
				}
				current := totals[sample.UseCase][sample.ID][metric.name]
				current.total += value
				current.count++
				totals[sample.UseCase][sample.ID][metric.name] = current
			}
		}
	}
	result := map[string]map[string]map[string]float64{}
	for useCase, queries := range totals {
		result[useCase] = map[string]map[string]float64{}
		for id, metrics := range queries {
			result[useCase][id] = map[string]float64{}
			for name, value := range metrics {
				result[useCase][id][name] = value.total / float64(value.count)
			}
		}
	}
	return result
}

// collectSamples는 회차별 측정값을 사용 사례와 지표별 표본으로 옮긴다.
func collectSamples(runs []runMetrics) map[string]map[string][]float64 {
	samples := map[string]map[string][]float64{}
	for _, run := range runs {
		for useCase, metrics := range run.UseCases {
			if _, present := samples[useCase]; !present {
				samples[useCase] = map[string][]float64{}
			}
			for _, metric := range metricNames {
				value := metric.value(metrics)
				// 정답이나 기대 근거가 없어 정의되지 않은 값은 표본에서 뺀다.
				if value < 0 {
					continue
				}
				samples[useCase][metric.name] = append(samples[useCase][metric.name], value)
			}
		}
	}
	return samples
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
			if len(left) < 2 {
				continue
			}
			difference, interval := pairedInterval(left, right)
			improved := difference > 0
			if metric.lowerIsBetter {
				improved = difference < 0
			}
			comparisons[useCase] = append(comparisons[useCase], comparison{
				Metric:     metric.name,
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
	result := estimate{Repeats: len(values), Mean: mean(values), StdDev: undefinedMetric, Interval: undefinedMetric}
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
	differences := make([]float64, len(baseline))
	for index := range baseline {
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
		return 0
	}
	total := 0.0
	for _, value := range values {
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
	for _, value := range values {
		total += (value - average) * (value - average)
	}
	return total / float64(len(values)-1)
}

// marginal은 한 단계가 바로 앞 단계에 더한 한계 기여다. 「검색 품질 평가」가 정답·필수
// 근거 증가량과 문자 수·지연 증가량을 나란히 남기고 하나의 합성 점수로 줄이지 말라고
// 정했다. 회수 수는 검색이 결정적이므로 첫 회차로 세고, 문자 수와 지연은 반복 평균의
// 질의당 차이다.
type marginal struct {
	Stage          string  `json:"stage"`
	ComparedTo     string  `json:"compared_to"`
	UseCase        string  `json:"use_case"`
	Queries        int     `json:"queries"`
	Recovered      int     `json:"newly_recovered"`
	Lost           int     `json:"lost"`
	AddedChars     float64 `json:"added_chars_per_query"`
	AddedLatencyMS float64 `json:"added_latency_ms_per_query"`
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
			chars, latency := 0.0, 0.0
			for _, id := range slices.Sorted(maps.Keys(after[useCase])) {
				cost := after[useCase][id]
				base, present := before[useCase][id]
				if !present {
					continue
				}
				entry.Queries++
				chars += cost.chars - base.chars
				latency += cost.latency - base.latency
				for key := range current[0].recovered[id] {
					if _, found := previous[0].recovered[id][key]; !found {
						entry.Recovered++
					}
				}
				for key := range previous[0].recovered[id] {
					if _, found := current[0].recovered[id][key]; !found {
						entry.Lost++
					}
				}
			}
			if entry.Queries == 0 {
				continue
			}
			entry.AddedChars = chars / float64(entry.Queries)
			entry.AddedLatencyMS = latency / float64(entry.Queries)
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
	totals := map[string]map[string]queryCost{}
	for _, run := range runs {
		for _, sample := range run.QuerySamples {
			if totals[sample.UseCase] == nil {
				totals[sample.UseCase] = map[string]queryCost{}
			}
			cost := totals[sample.UseCase][sample.ID]
			cost.chars += float64(sample.BudgetUsed) / float64(len(runs))
			cost.latency += sample.LatencyMS / float64(len(runs))
			totals[sample.UseCase][sample.ID] = cost
		}
	}
	return totals
}
