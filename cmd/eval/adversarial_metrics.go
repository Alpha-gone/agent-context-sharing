package main

import (
	"maps"
	"slices"
)

type controlRun struct{ before, after float64 }
type attackMetricPairs map[string][]controlRun

func (pairs attackMetricPairs) add(unit string, before, after float64) {
	pairs[unit] = append(pairs[unit], controlRun{before: before, after: after})
}

func (pairs attackMetricPairs) summary() metricSummary {
	var before, after []float64
	for _, unit := range slices.Sorted(maps.Keys(pairs)) {
		var left, right float64
		for _, pair := range pairs[unit] {
			left += pair.before
			right += pair.after
		}
		count := float64(len(pairs[unit]))
		before = append(before, left/count)
		after = append(after, right/count)
	}
	return summarizeMetric(before, after)
}

// 반복은 질의·표본 안에서, 같은 과업의 질의는 독립 단위 안에서 먼저 평균한다.
// 영향 질의 수는 같은 질의의 같은 회차가 여러 표본에 나타나도 한 번만 센다.
func summarizeMetrics(runs []sampleRuns) adversarialMetrics {
	success, affected, contamination := attackMetricPairs{}, attackMetricPairs{}, attackMetricPairs{}
	trace, recovery, controls := attackMetricPairs{}, attackMetricPairs{}, attackMetricPairs{}
	affectedQueries := map[string][]float64{}
	units := map[string]string{}
	pairs := 0
	for _, sample := range runs {
		unitFor := func(id string) string {
			if unit := sample.units[id]; unit != "" {
				return unit
			}
			return id
		}
		for _, id := range slices.Sorted(maps.Keys(sample.targets)) {
			series := sample.targets[id]
			if len(series) == 0 {
				continue
			}
			pairs++
			unit := unitFor(id)
			units[id] = unit
			average := func(value func(targetRun) float64) float64 {
				total := 0.0
				for _, run := range series {
					total += value(run)
				}
				return total / float64(len(series))
			}
			success.add(unit, average(func(r targetRun) float64 { return r.PreSuccess }), average(func(r targetRun) float64 { return r.Success }))
			contamination.add(unit, average(func(r targetRun) float64 { return r.PreContamination }), average(func(r targetRun) float64 { return r.Contamination }))
			recovery.add(unit, average(func(r targetRun) float64 { return r.AttackRecovered }), average(func(r targetRun) float64 { return r.Recovered }))
			total, count := 0.0, 0
			for _, run := range series {
				if run.Traced >= 0 {
					total += run.Traced
					count++
				}
			}
			if count > 0 {
				trace.add(unit, 0, total/float64(count))
			}
			union := affectedQueries[id]
			if len(union) < len(series) {
				union = append(union, make([]float64, len(series)-len(union))...)
			}
			for repeat, run := range series {
				union[repeat] = max(union[repeat], run.Affected)
			}
			affectedQueries[id] = union
		}
		for _, id := range slices.Sorted(maps.Keys(sample.controlRuns)) {
			series := sample.controlRuns[id]
			if len(series) == 0 {
				continue
			}
			var before, after float64
			for _, run := range series {
				before += run.before
				after += run.after
			}
			controls.add(unitFor(id), before/float64(len(series)), after/float64(len(series)))
		}
	}
	affectedCount := 0.0
	for _, id := range slices.Sorted(maps.Keys(affectedQueries)) {
		value := mean(affectedQueries[id])
		affected.add(units[id], 0, value)
		affectedCount += value
	}
	result := adversarialMetrics{Samples: len(runs), TargetQueries: len(affectedQueries), TargetPairs: pairs,
		AttackSuccess: success.summary(), Affected: affected.summary(), Contamination: contamination.summary(),
		Trace: trace.summary(), FalsePositive: controls.summary(), Recovery: recovery.summary()}
	result.AffectedQueries = affectedCount
	return result
}
