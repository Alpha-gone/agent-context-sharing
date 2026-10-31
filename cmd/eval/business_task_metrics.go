package main

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"time"
)

var businessTaskMetrics = []string{"task_success", "elapsed_ms", "preparation_ms", "reindex_ms", "model_turns", "tool_calls", "user_interventions", "evidence_refetches", "evidence_available", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "preparation_tokens", "reindex_tokens", "execution_tokens", "total_tokens", "total_cost"}

type businessTasksReport struct {
	StartedAt         time.Time                 `json:"started_at"`
	Version           string                    `json:"dataset_version"`
	Source            string                    `json:"source"`
	Provenance        string                    `json:"provenance"`
	Experiment        string                    `json:"experiment"`
	Controls          businessTaskControls      `json:"controls"`
	Pricing           businessPricing           `json:"pricing"`
	Conditions        []businessTaskCondition   `json:"conditions"`
	Summaries         []businessTaskSummary     `json:"summaries"`
	Comparisons       []businessTaskComparison  `json:"comparisons"`
	Costs             []businessCostObservation `json:"costs"`
	DefaultActivation bool                      `json:"default_activation"`
}

type businessTaskSummary struct {
	Type             string                       `json:"type"`
	Condition        string                       `json:"condition"`
	Tasks            int                          `json:"tasks"`
	IndependentUnits int                          `json:"independent_units"`
	Attempts         int                          `json:"attempts"`
	StatusCounts     map[string]int               `json:"status_counts"`
	IndexStates      map[string]int               `json:"index_states"`
	SuccessRate      float64                      `json:"success_rate"`
	All              map[string]estimate          `json:"all_runs"`
	SuccessOnly      map[string]estimate          `json:"successful_runs_only"`
	Costs            map[string]businessCostTotal `json:"costs_by_phase"`
}

type businessTaskComparison struct {
	Type             string       `json:"type"`
	Baseline         string       `json:"baseline"`
	Condition        string       `json:"condition"`
	Tasks            int          `json:"tasks"`
	IndependentUnits int          `json:"independent_units"`
	Metrics          []comparison `json:"metrics"`
	Decision         string       `json:"decision"`
	Reason           string       `json:"reason"`
}

func businessRunMetrics(run businessTaskRun, cost businessCostObservation) map[string]float64 {
	values := map[string]float64{"task_success": 0, "elapsed_ms": float64(*run.ElapsedMS), "model_turns": float64(*run.Turns), "tool_calls": float64(*run.Tools), "user_interventions": float64(*run.Interventions), "evidence_refetches": float64(*run.EvidenceRefetches), "evidence_available": 0, "total_cost": cost.Total.Total}
	values["preparation_ms"], values["reindex_ms"] = cost.TimesMS["preparation"], cost.TimesMS["reindex"]
	if run.Status == "success" {
		values["task_success"] = 1
	}
	if *run.EvidenceAvailable {
		values["evidence_available"] = 1
	}
	total, complete := 0.0, true
	for category, value := range businessRunTokens(run, "") {
		values[category+"_tokens"] = value
		if value < 0 {
			complete = false
		} else {
			total += value
		}
	}
	if !complete {
		total = undefinedMetric
	}
	values["total_tokens"] = total
	for _, phase := range businessPhases {
		tokens, measured := 0.0, true
		for _, value := range businessRunTokens(run, phase) {
			if value < 0 {
				measured = false
			} else {
				tokens += value
			}
		}
		if !measured {
			tokens = undefinedMetric
		}
		values[phase+"_tokens"] = tokens
	}
	return values
}

// 반복→과업→독립 단위 순으로 평균한다. 한 반복의 미측정을 다른 반복으로 숨기지 않는다.
func businessIndependentValues(input businessTaskInput, cohort, condition string, successful bool) map[string]map[string]float64 {
	units := map[string]map[string][]float64{}
	tasks := slices.Clone(input.Tasks)
	slices.SortFunc(tasks, func(a, b businessTask) int { return cmp.Compare(a.Code, b.Code) })
	for _, task := range tasks {
		if cohort != "all" && task.Type != cohort {
			continue
		}
		samples := map[string][]float64{}
		for _, run := range task.Runs {
			if run.Condition != condition || (successful && run.Status != "success") {
				continue
			}
			values := businessRunMetrics(run, businessRunCost(task.Code, run, input.Pricing))
			for _, metric := range businessTaskMetrics {
				samples[metric] = append(samples[metric], values[metric])
			}
		}
		if len(samples) == 0 {
			continue
		}
		if units[task.Independent] == nil {
			units[task.Independent] = map[string][]float64{}
		}
		for _, metric := range businessTaskMetrics {
			units[task.Independent][metric] = append(units[task.Independent][metric], businessCompleteMean(samples[metric]))
		}
	}
	result := map[string]map[string]float64{}
	for unit, metrics := range units {
		result[unit] = map[string]float64{}
		for _, metric := range businessTaskMetrics {
			result[unit][metric] = businessCompleteMean(metrics[metric])
		}
	}
	return result
}

func businessCompleteMean(values []float64) float64 {
	for _, value := range values {
		if !validMetric(value) {
			return undefinedMetric
		}
	}
	return mean(values)
}

func summarizeBusinessTasks(input businessTaskInput) businessTasksReport {
	input.Tasks = slices.Clone(input.Tasks)
	slices.SortFunc(input.Tasks, func(a, b businessTask) int { return cmp.Compare(a.Code, b.Code) })
	for i := range input.Tasks {
		input.Tasks[i].Runs = slices.Clone(input.Tasks[i].Runs)
		slices.SortFunc(input.Tasks[i].Runs, func(a, b businessTaskRun) int {
			return cmp.Or(cmp.Compare(a.Repeat, b.Repeat), cmp.Compare(a.Order, b.Order))
		})
	}
	report := businessTasksReport{Version: input.Version, Source: "imported_task_trace", Provenance: input.Provenance, Experiment: input.Experiment, Controls: input.Controls, Pricing: input.Pricing, Conditions: input.Conditions}
	for _, task := range input.Tasks {
		for _, run := range task.Runs {
			report.Costs = append(report.Costs, businessRunCost(task.Code, run, input.Pricing))
		}
	}
	slices.SortFunc(report.Costs, func(a, b businessCostObservation) int {
		return cmp.Or(cmp.Compare(a.Task, b.Task), cmp.Compare(a.Repeat, b.Repeat), cmp.Compare(a.Condition, b.Condition))
	})
	for _, cohort := range []string{"all", "resume", "handoff", "fact", "multi_evidence"} {
		count := 0
		for _, task := range input.Tasks {
			if cohort == "all" || task.Type == cohort {
				count++
			}
		}
		if count == 0 {
			continue
		}
		values := map[string]map[string]map[string]float64{}
		for _, condition := range input.Conditions {
			units := businessIndependentValues(input, cohort, condition.Code, false)
			values[condition.Code] = units
			summary := businessTaskSummary{Type: cohort, Condition: condition.Code, Tasks: count, IndependentUnits: len(units), StatusCounts: map[string]int{"success": 0, "failure": 0, "timeout": 0, "cancelled": 0}, All: businessEstimates(units), SuccessOnly: businessEstimates(businessIndependentValues(input, cohort, condition.Code, true)), Costs: map[string]businessCostTotal{}}
			summary.IndexStates = map[string]int{"initial": 0, "prepared": 0}
			for _, task := range input.Tasks {
				if cohort != "all" && task.Type != cohort {
					continue
				}
				for _, run := range task.Runs {
					if run.Condition != condition.Code {
						continue
					}
					summary.Attempts++
					summary.StatusCounts[run.Status]++
					summary.IndexStates[run.IndexState]++
					cost := businessRunCost(task.Code, run, input.Pricing)
					for _, phase := range businessPhases {
						value := summary.Costs[phase]
						if value.Status == "" {
							value.Status = "measured"
						}
						if cost.Phases[phase].Status == "unmeasured" {
							value.Status = "unmeasured"
						}
						value.KnownSubtotal += cost.Phases[phase].KnownSubtotal
						value.Total = value.KnownSubtotal
						if value.Status == "unmeasured" {
							value.Total = undefinedMetric
						}
						summary.Costs[phase] = value
					}
				}
			}
			summary.SuccessRate = float64(summary.StatusCounts["success"]) / float64(summary.Attempts)
			report.Summaries = append(report.Summaries, summary)
		}
		baseline := input.Conditions[0].Code
		for _, condition := range input.Conditions[1:] {
			entry := businessTaskComparison{Type: cohort, Baseline: baseline, Condition: condition.Code, Tasks: count, IndependentUnits: len(values[baseline])}
			for _, metric := range businessTaskMetrics {
				left, right := []float64{}, []float64{}
				for _, unit := range slices.Sorted(maps.Keys(values[baseline])) {
					a, b := values[baseline][unit][metric], values[condition.Code][unit][metric]
					if validMetric(a) && validMetric(b) {
						left, right = append(left, a), append(right, b)
					}
				}
				difference, interval := pairedInterval(left, right)
				improved := difference < 0
				if metric == "task_success" || metric == "evidence_available" {
					improved = difference > 0
				}
				entry.Metrics = append(entry.Metrics, comparison{Metric: metric, Samples: len(left), DegreesFreedom: max(0, len(left)-1), Status: statisticalStatus(len(left)), Baseline: mean(left), Current: mean(right), Difference: difference, Interval: interval, Improved: len(left) > 0 && improved, Significant: interval >= 0 && math.Abs(difference) > interval})
			}
			entry.Decision, entry.Reason = judgeBusinessTasks(input, entry)
			report.Comparisons = append(report.Comparisons, entry)
		}
	}
	return report
}

func businessEstimates(units map[string]map[string]float64) map[string]estimate {
	result := map[string]estimate{}
	for _, metric := range businessTaskMetrics {
		values := []float64{}
		for _, unit := range slices.Sorted(maps.Keys(units)) {
			values = append(values, units[unit][metric])
		}
		result[metric] = estimateOf(values)
		if result[metric].Samples > 0 && result[metric].Samples < len(units) {
			value := result[metric]
			value.Status = "partially_measured"
			result[metric] = value
		}
	}
	return result
}

func judgeBusinessTasks(input businessTaskInput, entry businessTaskComparison) (string, string) {
	improved := false
	for _, metric := range entry.Metrics {
		if metric.Metric == "task_success" && metric.Significant && !metric.Improved {
			return "rejected", "task_success_regression"
		}
		if metric.Significant && metric.Improved && slices.Contains([]string{"elapsed_ms", "model_turns", "total_tokens", "total_cost"}, metric.Metric) {
			improved = true
		}
	}
	if entry.Tasks < minimumRepeats || entry.IndependentUnits < 2 {
		return "deferred", "insufficient_independent_tasks"
	}
	if input.Provenance != "agent_trace" {
		return "deferred", "synthetic_trace"
	}
	for _, metric := range entry.Metrics {
		// 재색인 시간 생략(-1)은 허용 입력이며 보류 사유가 아니다. 표본 수는 결과에 그대로 남는다.
		if metric.Samples != entry.IndependentUnits && metric.Metric != "reindex_ms" {
			return "deferred", "usage_or_cost_unmeasured"
		}
		if input.Experiment == "reduction" && metric.Metric == "evidence_available" && (metric.Baseline < 1 || metric.Current < 1) {
			return "deferred", "required_evidence_unavailable"
		}
	}
	if improved {
		return "candidate", "significant_task_efficiency_improvement"
	}
	return "deferred", "no_significant_task_efficiency_improvement"
}
