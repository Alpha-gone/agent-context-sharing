package main

import (
	"fmt"
	"math"
	"slices"
)

type unitCohort struct {
	Cohort      string                         `json:"cohort"`
	Samples     int                            `json:"independent_units"`
	Annotated   bool                           `json:"complete_retrieval_annotations"`
	Totals      map[string]unitReviewMetrics   `json:"review_totals"`
	Estimates   map[string]map[string]estimate `json:"estimates"`
	Comparisons []comparison                   `json:"comparisons"`
	Retrieval   []stageJudgement               `json:"retrieval_judgements"`
	Status      string                         `json:"status"`
	Reason      string                         `json:"reason"`
	Activated   bool                           `json:"default_activation"`
}

var unitMetricNames = []string{"unit_completeness", "derived_count", "embedding_http_attempts", "vector_column_bytes", "task_success", "total_cost"}

func unitValues(run unitRun) map[string]float64 {
	return map[string]float64{"unit_completeness": run.Review.Completeness, "derived_count": float64(run.Review.Derived),
		"embedding_http_attempts": float64(run.Metrics.Preparation.EmbeddingCalls), "vector_column_bytes": float64(run.Metrics.Preparation.VectorBytes),
		"task_success": run.Task.Success, "total_cost": run.Task.TotalCost}
}

func aggregateUnits(input unitInput, all []unitRun) ([]unitCohort, error) {
	if len(all) != len(input.Samples)*2*initialEvaluationRepeats {
		return nil, fmt.Errorf("검색 단위 집계 회차 누락")
	}
	var result []unitCohort
	for _, cohort := range []string{"conversation", "code_document"} {
		groups := map[string][]unitRun{}
		for _, sample := range input.Samples {
			if sample.Cohort == cohort {
				groups[sample.Code] = nil
			}
		}
		if len(groups) == 0 {
			continue
		}
		for _, run := range all {
			if _, ok := groups[run.Sample]; ok && run.Cohort == cohort {
				groups[run.Sample] = append(groups[run.Sample], run)
			}
		}
		entry := unitCohort{Cohort: cohort, Samples: len(groups), Annotated: true, Totals: map[string]unitReviewMetrics{}, Estimates: map[string]map[string]estimate{}}
		totals := map[string]map[string][]float64{"current": {}, "semantic": {}}
		retrieval := map[string][]runMetrics{"current": {}, "semantic": {}}
		for _, sample := range input.Samples {
			if sample.Cohort != cohort {
				continue
			}
			runs := map[string][]runMetrics{"current": {}, "semantic": {}}
			metrics := map[string]map[string][]float64{"current": {}, "semantic": {}}
			for _, run := range groups[sample.Code] {
				policy := run.Metrics.Stage
				if _, ok := runs[policy]; !ok || run.Metrics.Preparation == nil {
					return nil, fmt.Errorf("검색 단위 집계 조건 오류")
				}
				runs[policy] = append(runs[policy], run.Metrics)
				for _, query := range run.Metrics.QuerySamples {
					entry.Annotated = entry.Annotated && validMetric(query.EvidenceCompleteness) && validMetric(query.PathContinuity)
				}
				for name, value := range unitValues(run) {
					metrics[policy][name] = append(metrics[policy][name], value)
				}
				if run.Metrics.Repeat == 1 {
					total := entry.Totals[policy]
					total.Derived += run.Review.Derived
					total.Complete += run.Review.Complete
					total.HumanReviewed += run.Review.HumanReviewed
					total.Disagreements += run.Review.Disagreements
					total.MissingLinks += run.Review.MissingLinks
					entry.Totals[policy] = total
				}
			}
			if err := validateMeasuredRuns([]string{"current", "semantic"}, runs); err != nil {
				return nil, err
			}
			for _, policy := range []string{"current", "semantic"} {
				for _, name := range unitMetricNames {
					values, complete := metrics[policy][name], true
					for _, value := range values {
						complete = complete && validMetric(value)
					}
					v := undefinedMetric
					if complete && len(values) == initialEvaluationRepeats {
						v = mean(values)
					}
					totals[policy][name] = append(totals[policy][name], v)
				}
			}
		}
		for _, policy := range []string{"current", "semantic"} {
			total := entry.Totals[policy]
			total.Completeness = float64(total.Complete) / float64(total.Derived)
			entry.Totals[policy] = total
			entry.Estimates[policy] = map[string]estimate{}
			for _, name := range unitMetricNames {
				entry.Estimates[policy][name] = estimateOf(totals[policy][name])
			}
			for repeat := 1; repeat <= initialEvaluationRepeats; repeat++ {
				run := runMetrics{Stage: policy, Repeat: repeat}
				for _, sample := range input.Samples {
					if sample.Cohort != cohort {
						continue
					}
					for _, part := range groups[sample.Code] {
						if part.Metrics.Stage == policy && part.Metrics.Repeat == repeat {
							run.QuerySamples = append(run.QuerySamples, part.Metrics.QuerySamples...)
						}
					}
				}
				retrieval[policy] = append(retrieval[policy], run)
			}
		}
		if err := validateMeasuredRuns([]string{"current", "semantic"}, retrieval); err != nil {
			return nil, err
		}
		entry.Retrieval = judge([]string{"current", "semantic"}, retrieval)
		for _, name := range unitMetricNames {
			var left, right []float64
			for i, before := range totals["current"][name] {
				after := totals["semantic"][name][i]
				if validMetric(before) && validMetric(after) {
					left, right = append(left, before), append(right, after)
				}
			}
			entry.Comparisons = append(entry.Comparisons, unitComparison(name, left, right))
		}
		entry.Status, entry.Reason = assessUnits(input.Provenance, entry)
		result = append(result, entry)
	}
	return result, nil
}

func unitComparison(name string, left, right []float64) comparison {
	difference, interval := pairedInterval(left, right)
	improved := difference < 0
	if name == "unit_completeness" || name == "task_success" {
		improved = difference > 0
	}
	return comparison{Metric: name, Samples: len(left), DegreesFreedom: max(0, len(left)-1), Status: statisticalStatus(len(left)),
		Baseline: mean(left), Current: mean(right), Difference: difference, Interval: interval,
		Improved: len(left) > 0 && improved, Significant: interval >= 0 && math.Abs(difference) > interval}
}

func assessUnits(provenance string, entry unitCohort) (string, string) {
	ready, improved, regressed, successLost := entry.Samples >= 2 && entry.Annotated, false, false, false
	costMeasured := false
	for _, metric := range entry.Comparisons {
		if slices.Contains([]string{"unit_completeness", "task_success"}, metric.Metric) {
			ready = ready && metric.Samples == entry.Samples && metric.Status == "measured"
		}
		if metric.Metric == "unit_completeness" {
			regressed = regressed || metric.Significant && !metric.Improved
		}
		if metric.Metric == "task_success" {
			successLost = metric.Difference < 0 && metric.Samples > 0
		}
		if metric.Metric == "total_cost" {
			costMeasured = metric.Samples == entry.Samples && metric.Status == "measured"
			improved = improved || metric.Samples == entry.Samples && metric.Significant && metric.Improved
		}
	}
	for _, comparisons := range entry.Retrieval[1].Comparisons {
		for _, metric := range comparisons {
			if slices.Contains([]string{"recall", "reciprocal_rank", "evidence_completeness", "path_continuity"}, metric.Metric) {
				ready = ready && metric.Status == "measured"
			}
			if !slices.Contains([]string{"budget_used", "budget_per_hit", "misroute_rate"}, metric.Metric) {
				regressed = regressed || metric.Significant && !metric.Improved
			}
			if metric.Metric == "budget_used" {
				improved = improved || metric.Significant && metric.Improved
			}
		}
	}
	if regressed || successLost {
		return "rejected", "quality_or_task_regression"
	}
	if !ready {
		return "deferred", "unmeasured_or_insufficient_independent_units"
	}
	for _, policy := range []string{"current", "semantic"} {
		total := entry.Totals[policy]
		if total.HumanReviewed == 0 || total.Disagreements > 0 {
			return "deferred", "human_review_pending_or_disagrees"
		}
	}
	if provenance == "synthetic" {
		return "deferred", "synthetic_execution_only"
	}
	if !improved {
		if !costMeasured {
			return "deferred", "total_cost_unmeasured"
		}
		return "rejected", "no_significant_efficiency_improvement"
	}
	return "candidate", "controlled_candidate_final_acceptance_pending"
}
