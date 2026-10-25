package main

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

var embeddingMetricDirections = map[string]bool{
	"relation_precision": false, "relation_recall": false, "relation_f1": false, "relation_candidates": true,
	"global_false_pass": true, "global_false_reject": true, "global_balanced_error": true,
	"preparation_ms": true, "document_embedding_http_attempts": true, "measurement_embedding_http_attempts": true,
	"vector_column_bytes": true, "vector_rows": true, "measurement_duration_ms": true, "embedding_cost": true,
}

func embeddingValues(run embeddingRun, t embeddingThresholds, limit int) map[string]float64 {
	selected, hits, positives := 0, 0, 0
	for _, pair := range run.Relations {
		if pair.Relevant {
			positives++
		}
		if pair.Score >= t.Relation && selected < limit {
			selected++
			if pair.Relevant {
				hits++
			}
		}
	}
	precision, recall, f1 := -1.0, -1.0, -1.0
	if selected > 0 {
		precision = float64(hits) / float64(selected)
	}
	if positives > 0 {
		recall = float64(hits) / float64(positives)
		f1 = 2 * float64(hits) / float64(selected+positives)
	}
	fp, fn, negative, positive := 0, 0, 0, 0
	for _, q := range run.Signals {
		predicted := !q.Keyword && (!q.Semantic || q.Top < t.Global)
		if q.Expected {
			positive++
			if !predicted {
				fn++
			}
		} else {
			negative++
			if predicted {
				fp++
			}
		}
	}
	fpr, fnr, balanced := -1.0, -1.0, -1.0
	if negative > 0 {
		fpr = float64(fp) / float64(negative)
	}
	if positive > 0 {
		fnr = float64(fn) / float64(positive)
	}
	if fpr >= 0 && fnr >= 0 {
		balanced = (fpr + fnr) / 2
	}
	return map[string]float64{"relation_precision": precision, "relation_recall": recall, "relation_f1": f1, "relation_candidates": float64(selected), "global_false_pass": fpr, "global_false_reject": fnr, "global_balanced_error": balanced,
		"preparation_ms": run.Preparation.DurationMS, "document_embedding_http_attempts": float64(run.Preparation.EmbeddingCalls), "measurement_embedding_http_attempts": float64(run.Calls), "vector_column_bytes": float64(run.Preparation.VectorBytes), "vector_rows": float64(run.Preparation.VectorRows), "measurement_duration_ms": run.DurationMS, "embedding_cost": run.Cost}
}

// 반복을 먼저 평균하고 같은 독립 단위의 표본을 묶는다. 한 회차라도 미측정이면
// 그 표본 지표를 유효한 0으로 바꾸지 않는다.
func embeddingUnitValues(runs []embeddingRun, variant, split string, t embeddingThresholds, limit int) map[string]map[string]float64 {
	samples, units := map[string]map[string][]float64{}, map[string]string{}
	for _, r := range runs {
		if r.Variant != variant || r.Split != split {
			continue
		}
		if samples[r.Sample] == nil {
			samples[r.Sample] = map[string][]float64{}
		}
		units[r.Sample] = r.Unit
		for name, value := range embeddingValues(r, t, limit) {
			samples[r.Sample][name] = append(samples[r.Sample][name], value)
		}
	}
	grouped := map[string]map[string][]float64{}
	for _, code := range slices.Sorted(maps.Keys(samples)) {
		unit := units[code]
		if grouped[unit] == nil {
			grouped[unit] = map[string][]float64{}
		}
		for name, values := range samples[code] {
			value := -1.0
			if len(values) == initialEvaluationRepeats && !slices.ContainsFunc(values, func(v float64) bool { return !validMetric(v) }) {
				value = mean(values)
			}
			grouped[unit][name] = append(grouped[unit][name], value)
		}
	}
	result := map[string]map[string]float64{}
	for unit, metrics := range grouped {
		result[unit] = map[string]float64{}
		for name, values := range metrics {
			value := -1.0
			if !slices.ContainsFunc(values, func(v float64) bool { return !validMetric(v) }) {
				value = mean(values)
			}
			result[unit][name] = value
		}
	}
	return result
}

func tuneEmbeddingThresholds(input embeddingInput, v embeddingVariant, runs []embeddingRun) (embeddingThresholds, error) {
	result := embeddingThresholds{}
	for _, relation := range []bool{true, false} {
		grid, metric, best := v.GlobalThresholds, "global_balanced_error", math.Inf(1)
		if relation {
			grid, metric, best = v.RelationThresholds, "relation_f1", math.Inf(-1)
		}
		chosen := -2.0
		for _, threshold := range slices.Sorted(slices.Values(grid)) {
			candidate := result
			if relation {
				candidate.Relation = threshold
			} else {
				candidate.Global = threshold
			}
			units := embeddingUnitValues(runs, v.Code, "tuning", candidate, input.Search.RelationLimit)
			var values []float64
			for _, unit := range slices.Sorted(maps.Keys(units)) {
				values = append(values, units[unit][metric])
			}
			if len(values) == 0 || slices.ContainsFunc(values, func(v float64) bool { return !validMetric(v) }) {
				continue
			}
			value := mean(values)
			if (relation && value >= best) || (!relation && value <= best) {
				best, chosen = value, threshold
			}
		}
		if chosen == -2 {
			return result, fmt.Errorf("임베딩 비교 조정 표본의 관련성/전역 양성·음성 분모가 부족하다")
		}
		if relation {
			result.Relation = chosen
		} else {
			result.Global = chosen
		}
	}
	return result, nil
}

func validateEmbeddingRuns(input embeddingInput, runs []embeddingRun) error {
	if len(runs) != 2*initialEvaluationRepeats*len(input.Samples) {
		return fmt.Errorf("임베딩 비교 회차 누락")
	}
	samples := map[string]embeddingSample{}
	for _, s := range input.Samples {
		samples[s.Code] = s
	}
	variants := []string{input.Variants[0].Code, input.Variants[1].Code}
	seen := map[string]bool{}
	for _, r := range runs {
		s, ok := samples[r.Sample]
		key := fmt.Sprintf("%s/%s/%d", r.Variant, r.Sample, r.Repeat)
		if !ok || !slices.Contains(variants, r.Variant) || r.Unit != s.Unit || r.Split != s.Split || r.Repeat < 1 || r.Repeat > initialEvaluationRepeats || seen[key] || len(r.Relations) != len(s.Related) || len(r.Signals) != len(s.Queries) || (r.Split == "evaluation") != (r.Metrics != nil) {
			return fmt.Errorf("임베딩 비교 대응 표본 오류")
		}
		seen[key] = true
		pairs := map[string]bool{}
		previous := math.Inf(1)
		for _, p := range r.Relations {
			label, ok := s.Related[p.Target]
			if !ok || pairs[p.Target] || label != p.Relevant || !validMetric(p.Score+1) || p.Score > 1 || p.Score > previous {
				return fmt.Errorf("임베딩 관계 점수 표본 오류")
			}
			pairs[p.Target] = true
			previous = p.Score
		}
		queries := map[string]bool{}
		for _, q := range r.Signals {
			id := q.Query
			expected := false
			found := false
			for _, original := range s.Queries {
				if id == s.Code+"."+original.ID {
					expected, found = s.GlobalExpected[original.ID], true
					break
				}
			}
			if !found || queries[id] || q.Expected != expected || (!q.Semantic && q.Top != -1) || (q.Semantic && (!validMetric(q.Top+1) || q.Top > 1)) {
				return fmt.Errorf("임베딩 전역 점수 표본 오류")
			}
			queries[id] = true
		}
		if !validMetric(r.Preparation.DurationMS) || r.Preparation.EmbeddingCalls < 0 || r.Preparation.VectorRows < 1 || r.Preparation.VectorBytes < 1 || r.Calls < 0 || !validMetric(r.DurationMS) || r.Cost != -1 {
			return fmt.Errorf("임베딩 자원 측정 오류")
		}
	}
	return nil
}

func aggregateEmbedding(input embeddingInput, result *embeddingReport) error {
	if err := validateEmbeddingRuns(input, result.Runs); err != nil {
		return err
	}
	if len(result.Conditions) != 2 {
		return fmt.Errorf("임베딩 집계 구성 누락")
	}
	names := []string{input.Variants[0].Code, input.Variants[1].Code}
	retrieval := map[string][]runMetrics{}
	totals := map[string]map[string]map[string]float64{}
	result.Estimates = map[string]map[string]estimate{}
	result.Comparisons = nil
	for i, name := range names {
		c := result.Conditions[i]
		selected, err := tuneEmbeddingThresholds(input, input.Variants[i], result.Runs)
		if err != nil {
			return err
		}
		if c.Code != name || c.Selected != selected {
			return fmt.Errorf("임베딩 조정 선택값 변경")
		}
		totals[name] = embeddingUnitValues(result.Runs, name, "evaluation", selected, input.Search.RelationLimit)
		result.Estimates[name] = map[string]estimate{}
		for metric := range embeddingMetricDirections {
			var values []float64
			for _, unit := range slices.Sorted(maps.Keys(totals[name])) {
				values = append(values, totals[name][unit][metric])
			}
			result.Estimates[name][metric] = estimateOf(values)
		}
		for repeat := 1; repeat <= initialEvaluationRepeats; repeat++ {
			var parts []measurement
			for _, r := range result.Runs {
				if r.Variant == name && r.Split == "evaluation" && r.Repeat == repeat {
					parts = append(parts, measurement{UseCases: r.Metrics.UseCases, QuerySamples: r.Metrics.QuerySamples})
				}
			}
			m := mergeMeasurements(parts)
			retrieval[name] = append(retrieval[name], runMetrics{Stage: name, Repeat: repeat, UseCases: m.UseCases, QuerySamples: m.QuerySamples})
		}
	}
	if err := validateMeasuredRuns(names, retrieval); err != nil {
		return err
	}
	result.Retrieval = judge(names, retrieval)
	for _, metric := range slices.Sorted(maps.Keys(embeddingMetricDirections)) {
		var left, right []float64
		for _, unit := range slices.Sorted(maps.Keys(totals[names[0]])) {
			a, b := totals[names[0]][unit][metric], totals[names[1]][unit][metric]
			if validMetric(a) && validMetric(b) {
				left = append(left, a)
				right = append(right, b)
			}
		}
		diff, interval := pairedInterval(left, right)
		improved := diff > 0
		if embeddingMetricDirections[metric] {
			improved = diff < 0
		}
		result.Comparisons = append(result.Comparisons, comparison{Metric: metric, Samples: len(left), DegreesFreedom: max(0, len(left)-1), Status: statisticalStatus(len(left)), Baseline: mean(left), Current: mean(right), Difference: diff, Interval: interval, Improved: improved, Significant: interval >= 0 && math.Abs(diff) > interval})
	}
	result.Status, result.Reason = assessEmbedding(input, *result)
	return nil
}

func assessEmbedding(input embeddingInput, r embeddingReport) (string, string) {
	ready, improved, regressed := true, false, false
	required := []string{"relation_precision", "relation_recall", "global_false_pass", "global_false_reject"}
	for _, name := range required {
		i := slices.IndexFunc(r.Comparisons, func(c comparison) bool { return c.Metric == name })
		if i < 0 || r.Comparisons[i].Samples < 2 {
			ready = false
			continue
		}
		c := r.Comparisons[i]
		regressed = regressed || (c.Significant && !c.Improved)
	}
	if len(r.Retrieval) != 2 || len(r.Retrieval[1].Comparisons) == 0 {
		return "deferred", "missing_retrieval"
	}
	for _, metrics := range r.Retrieval[1].Comparisons {
		for _, name := range []string{"recall", "reciprocal_rank", "evidence_completeness", "budget_per_hit", "latency_ms"} {
			i := slices.IndexFunc(metrics, func(c comparison) bool { return c.Metric == name })
			if i < 0 || metrics[i].Samples < 2 {
				ready = false
				continue
			}
			c := metrics[i]
			regressed = regressed || (c.Significant && !c.Improved)
			improved = improved || (name == "latency_ms" && c.Significant && c.Improved)
		}
	}
	for _, run := range r.Runs {
		if run.Metrics != nil {
			for _, q := range run.Metrics.QuerySamples {
				if q.LatencyMS > 500 {
					ready = false
				}
			}
		}
	}
	if regressed {
		return "rejected", "significant_quality_or_latency_regression"
	}
	if !ready {
		return "deferred", "insufficient_independent_units_annotations_or_latency_slo"
	}
	if input.Provenance == "synthetic" {
		return "deferred", "synthetic_provider"
	}
	if !improved {
		return "deferred", "no_significant_measured_latency_or_cost_improvement"
	}
	return "candidate", "quality_preserved_latency_improved_final_acceptance_pending"
}
