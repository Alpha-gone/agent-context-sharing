package main

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

func validMetric(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func unitOf(sample queryMetrics) string {
	if sample.IndependentUnit == "" {
		return "query:" + sample.ID
	}
	return "unit:" + sample.IndependentUnit
}

// validateMeasuredRuns는10회 대응 집계 전에 중복·누락·단위 변경을 거부한다.
func validateMeasuredRuns(stages []string, runs map[string][]runMetrics) error {
	type identity struct{ useCase, unit, datasetType, evidenceStatus string }
	var reference map[string]identity
	seenStages := map[string]bool{}
	if len(stages) == 0 {
		return fmt.Errorf("집계할 단계가 없다")
	}
	for _, stage := range stages {
		if seenStages[stage] || len(runs[stage]) != initialEvaluationRepeats {
			return fmt.Errorf("집계 단계 또는 10회 반복이 올바르지 않다")
		}
		seenStages[stage] = true
		repeats := map[int]bool{}
		for _, run := range runs[stage] {
			if run.Stage != stage || run.Repeat < 1 || run.Repeat > initialEvaluationRepeats || repeats[run.Repeat] || len(run.QuerySamples) == 0 {
				return fmt.Errorf("집계 회차·질의 표본이 누락되었거나 중복됐다")
			}
			repeats[run.Repeat] = true
			identities := map[string]identity{}
			for _, q := range run.QuerySamples {
				_, duplicate := identities[q.ID]
				if q.ID == "" || duplicate || !slices.Contains(useCases, q.UseCase) || (q.IndependentUnit != "" && !manifestCode.MatchString(q.IndependentUnit)) || q.BudgetUsed < 0 {
					return fmt.Errorf("집계 질의·독립 단위가 올바르지 않다")
				}
				for _, metric := range metricNames {
					v := metric.sample(q)
					if v != undefinedMetric && !validMetric(v) {
						return fmt.Errorf("집계 지표가 유한 측정값 또는 -1이 아니다")
					}
				}
				identities[q.ID] = identity{q.UseCase, unitOf(q), q.DatasetType, q.EvidenceStatus}
			}
			if reference == nil {
				reference = identities
			} else if !maps.Equal(reference, identities) {
				return fmt.Errorf("조건·회차 사이 질의 또는 독립 단위 매핑이 다르다")
			}
		}
	}
	return nil
}

func queryUnits(runs []runMetrics) map[string]string {
	units := map[string]string{}
	for _, run := range runs {
		for _, q := range run.QuerySamples {
			units[q.ID] = unitOf(q)
		}
	}
	return units
}

// independentSamples는 질의 반복 평균을 대화·과업별로 한 번 더 평균한다.
func independentSamples(queries map[string]map[string]map[string]float64, units map[string]string) map[string]map[string]map[string]float64 {
	result := map[string]map[string]map[string]float64{}
	for useCase, values := range queries {
		groups := map[string]map[string][]float64{}
		for _, id := range slices.Sorted(maps.Keys(values)) {
			unit := units[id]
			if groups[unit] == nil {
				groups[unit] = map[string][]float64{}
			}
			for name, value := range values[id] {
				groups[unit][name] = append(groups[unit][name], value)
			}
		}
		result[useCase] = map[string]map[string]float64{}
		for unit, metrics := range groups {
			result[useCase][unit] = map[string]float64{}
			for name, values := range metrics {
				result[useCase][unit][name] = mean(values)
			}
		}
	}
	return result
}

// pairedIndependentSamples는 같은 질의·지표의 유효 쌍만 단위별 평균에 넣는다.
// 양쪽에서 서로 다른 문항을 평균한 뒤 대응시키지 않는다.
func pairedIndependentSamples(before, after map[string]map[string]map[string]float64, units map[string]string) (map[string]map[string]map[string]float64, map[string]map[string]map[string]float64) {
	left, right := map[string]map[string]map[string]float64{}, map[string]map[string]map[string]float64{}
	for useCase, queries := range after {
		left[useCase], right[useCase] = map[string]map[string]float64{}, map[string]map[string]float64{}
		for id, values := range queries {
			left[useCase][id], right[useCase][id] = map[string]float64{}, map[string]float64{}
			for name, value := range values {
				if base, ok := before[useCase][id][name]; ok {
					left[useCase][id][name], right[useCase][id][name] = base, value
				}
			}
		}
	}
	return independentSamples(left, units), independentSamples(right, units)
}

// orderedRuns는 원본을 바꾸지 않고 누적 순서를 회차·질의 ID로 고정한다.
func orderedRuns(runs []runMetrics) []runMetrics {
	result := slices.Clone(runs)
	slices.SortFunc(result, func(a, b runMetrics) int { return cmp.Compare(a.Repeat, b.Repeat) })
	for i := range result {
		result[i].QuerySamples = slices.SortedFunc(slices.Values(result[i].QuerySamples), func(a, b queryMetrics) int { return strings.Compare(a.ID, b.ID) })
	}
	return result
}
