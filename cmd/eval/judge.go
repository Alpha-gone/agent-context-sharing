// 반복 회차를 모아 기준선 대비 개선폭과 유의성으로 판정한다.
//
// 절대 점수로 판정하지 않는 이유는 「검증 원칙」이 그렇게 확정했기 때문이다. 지금
// 목표 점수를 정할 측정 근거가 없으므로, 단계마다 이전 단계와의 차이와 그 차이의
// 신뢰구간만 본다.
package main

import "math"

// 「검증」이 각 조건을 최소 세 번 반복하라고 정했다. 신뢰구간의 폭이 판정에 필요한
// 수준보다 넓으면 반복을 늘린다.
const minimumRepeats = 3

// metricNames는 판정에 쓰는 지표와 그 방향이다. 예산 효율은 문자 수이므로 값이
// 작을수록 좋고 나머지는 클수록 좋다.
var metricNames = []struct {
	name          string
	lowerIsBetter bool
	value         func(useCaseMetrics) float64
}{
	{"recall", false, func(m useCaseMetrics) float64 { return m.Recall }},
	{"reciprocal_rank", false, func(m useCaseMetrics) float64 { return m.ReciprocalRank }},
	{"budget_per_hit", true, func(m useCaseMetrics) float64 { return m.BudgetPerHit }},
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
	for _, stage := range stages {
		samples[stage] = collectSamples(runs[stage])
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
			judgement.Comparisons = compareStages(samples[previous], samples[stage])
		}
		judgements = append(judgements, judgement)
	}
	return judgements
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
				// 정답을 못 찾아 정의되지 않은 예산 효율은 표본에서 뺀다.
				if metric.lowerIsBetter && value < 0 {
					continue
				}
				samples[useCase][metric.name] = append(samples[useCase][metric.name], value)
			}
		}
	}
	return samples
}

func compareStages(baseline, current map[string]map[string][]float64) map[string][]comparison {
	comparisons := map[string][]comparison{}
	for useCase, metrics := range current {
		previous, present := baseline[useCase]
		if !present {
			continue
		}
		for _, metric := range metricNames {
			left, right := previous[metric.name], metrics[metric.name]
			if len(left) < 2 || len(right) < 2 {
				continue
			}
			difference, interval := welch(left, right)
			improved := difference > 0
			if metric.lowerIsBetter {
				improved = difference < 0
			}
			comparisons[useCase] = append(comparisons[useCase], comparison{
				Metric:     metric.name,
				Baseline:   mean(left),
				Current:    mean(right),
				Difference: difference,
				Interval:   interval,
				Improved:   improved,
				// 차이의 신뢰구간이 0을 품지 않을 때만 유의한 것으로 본다.
				Significant: math.Abs(difference) > interval,
			})
		}
	}
	return comparisons
}

func estimateOf(values []float64) estimate {
	result := estimate{Repeats: len(values), Mean: mean(values)}
	if len(values) < 2 {
		return result
	}
	result.StdDev = math.Sqrt(variance(values))
	result.Interval = criticalValue(len(values)-1) * result.StdDev / math.Sqrt(float64(len(values)))
	return result
}

// welch는 분산이 다를 수 있는 두 표본의 평균 차이와 95% 신뢰구간 반폭을 낸다.
// 단계마다 질의 수가 같아도 회차 수와 흩어짐이 다를 수 있어 등분산을 가정하지 않는다.
func welch(baseline, current []float64) (float64, float64) {
	leftN, rightN := float64(len(baseline)), float64(len(current))
	leftVar, rightVar := variance(baseline), variance(current)
	standardError := math.Sqrt(leftVar/leftN + rightVar/rightN)
	difference := mean(current) - mean(baseline)
	if standardError == 0 {
		return difference, 0
	}
	numerator := math.Pow(leftVar/leftN+rightVar/rightN, 2)
	denominator := math.Pow(leftVar/leftN, 2)/(leftN-1) + math.Pow(rightVar/rightN, 2)/(rightN-1)
	degrees := int(numerator / denominator)
	return difference, criticalValue(degrees) * standardError
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

// tTable은 양측 95% 신뢰구간의 임계값이다. 자유도가 표를 넘으면 정규 근사를 쓴다.
var tTable = []float64{
	12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
	2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
	2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042,
}

func criticalValue(degrees int) float64 {
	if degrees < 1 {
		return tTable[0]
	}
	if degrees > len(tTable) {
		return 1.96
	}
	return tTable[degrees-1]
}
