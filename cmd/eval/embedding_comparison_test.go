package main

import (
	json "encoding/json/v2"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func embeddingFixture(t *testing.T) embeddingInput {
	t.Helper()
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	in := embeddingInput{Version: "eval11-v1", Provenance: "synthetic", Axis: "dimension", GraphName: "agent_context", AsOf: at,
		Search: embeddingSearch{Budget: 4000, MaxHops: 1, MaxHopNodes: 24, Execution: "sequential", CandidateLimit: 16, RelationLimit: 2, FoldThreshold: 1, IndexTargets: "all_layers"},
		Variants: []embeddingVariant{
			{Code: "base", Provider: "gemini", Model: "gemini-embedding-2", Dimension: 768, VectorType: "vector", DatabaseEnv: "EVAL11_BASE_DB", EndpointEnv: "EVAL11_ENDPOINT", KeyEnv: "EVAL11_KEY", RelationThresholds: []float64{0.3, 0.9}, GlobalThresholds: []float64{0.2, 0.8}},
			{Code: "candidate", Provider: "gemini", Model: "gemini-embedding-2", Dimension: 128, VectorType: "vector", DatabaseEnv: "EVAL11_OTHER_DB", EndpointEnv: "EVAL11_ENDPOINT", KeyEnv: "EVAL11_KEY", RelationThresholds: []float64{0.2, 0.7}, GlobalThresholds: []float64{0.1, 0.9}}}}
	for _, split := range []string{"tuning", "evaluation"} {
		set := contextSet{Version: "corpus-v1", Contexts: []contextSpec{
			{Key: "s", Layer: "source", Body: "private-source", Source: &sourceSpec{Channel: "conversation", Locator: "urn:private:" + split, OccurredAt: at.Add(-time.Hour), OriginKind: "user_utterance"}},
			{Key: "d", Layer: "derived", Body: "private-derived", Derived: &derivedSpec{Kind: "proposition", DerivedFrom: []string{"s"}, EvidenceState: "observation"}},
			{Key: "anchor", Layer: "event", Body: "private-anchor", Event: &eventSpec{Members: []string{"d"}, Start: at.Add(-time.Minute)}},
			{Key: "yes", Layer: "event", Body: "private-positive", Event: &eventSpec{Members: []string{"d"}, Start: at.Add(-time.Minute)}},
			{Key: "no", Layer: "event", Body: "private-negative", Event: &eventSpec{Members: []string{"d"}, Start: at.Add(-time.Minute)}},
		}}
		in.Samples = append(in.Samples, embeddingSample{Code: split, Unit: split + "-unit", Split: split, Contexts: set, Anchor: "anchor", Related: map[string]bool{"yes": true, "no": false},
			Queries: []querySpec{{ID: "hit", UseCase: useCaseFact, WorkContext: "needlehit", Answers: []string{"anchor"}, Required: []string{"s", "d"}, Paths: [][]string{{"d", "s"}}}, {ID: "miss", UseCase: useCaseFact, WorkContext: "needlemiss", Answers: []string{"yes"}, Required: []string{"s", "d"}, Paths: [][]string{{"d", "s"}}}}, GlobalExpected: map[string]bool{"hit": false, "miss": true}})
	}
	return in
}

func TestEmbeddingInputPreflight(t *testing.T) {
	changes := map[string]func(*embeddingInput){
		"two_axes": func(in *embeddingInput) { in.Variants[1].VectorType = "halfvec" },
		"no_axis":  func(in *embeddingInput) { in.Variants[1].Dimension = 768 },
		"unsupported_model": func(in *embeddingInput) {
			in.Axis = "model"
			in.Variants[1].Dimension = 768
			in.Variants[1].Model = "unadopted"
		},
		"binary":                 func(in *embeddingInput) { in.Variants[1].VectorType = "bit" },
		"duplicate_variant":      func(in *embeddingInput) { in.Variants[1].Code = "base" },
		"same_unit_across_split": func(in *embeddingInput) { in.Samples[1].Unit = in.Samples[0].Unit },
		"same_source_across_split": func(in *embeddingInput) {
			in.Samples[1].Contexts.Contexts[0].Source.Locator = in.Samples[0].Contexts.Contexts[0].Source.Locator
		},
		"no_final": func(in *embeddingInput) { in.Samples[1].Split = "tuning" },
		"unknown_target": func(in *embeddingInput) {
			delete(in.Samples[0].Related, "no")
			in.Samples[0].Related["unknown"] = false
		},
		"incomplete_pair_labels": func(in *embeddingInput) { delete(in.Samples[0].Related, "no") },
		"unknown_anchor":         func(in *embeddingInput) { in.Samples[0].Anchor = "d" },
		"missing_query_label":    func(in *embeddingInput) { delete(in.Samples[0].GlobalExpected, "miss") },
		"query_scope":            func(in *embeddingInput) { in.Samples[0].Queries[0].Scope = "global" },
		"query_time":             func(in *embeddingInput) { in.Samples[0].Queries[0].AsOf = new(in.AsOf.Add(time.Second)) },
		"query_unit":             func(in *embeddingInput) { in.Samples[0].Queries[0].IndependentUnit = "other" },
		"query_leak_code":        func(in *embeddingInput) { in.Samples[0].Queries[0].ID = "urn:private" },
		"environment_value":      func(in *embeddingInput) { in.Variants[0].KeyEnv = "private-secret" },
		"duplicate_grid":         func(in *embeddingInput) { in.Variants[0].GlobalThresholds = []float64{0.2, 0.2} },
		"grid_nan":               func(in *embeddingInput) { in.Variants[0].GlobalThresholds[0] = math.NaN() },
		"relation_range":         func(in *embeddingInput) { in.Variants[0].RelationThresholds[0] = -1.1 },
		"global_range":           func(in *embeddingInput) { in.Variants[0].GlobalThresholds[0] = -0.1 },
		"missing_asof":           func(in *embeddingInput) { in.AsOf = time.Time{} },
		"bad_budget":             func(in *embeddingInput) { in.Search.Budget = -1 },
		"empty_corpus":           func(in *embeddingInput) { in.Samples[0].Contexts.Contexts = nil },
	}
	if err := validateEmbeddingInput(embeddingFixture(t)); err != nil {
		t.Fatal(err)
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			in := embeddingFixture(t)
			change(&in)
			if err := validateEmbeddingInput(in); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("preflight: %v", err)
			}
		})
	}
	t.Run("unknown_json", func(t *testing.T) {
		path := writeManifestFixture(t, map[string]any{"private": "secret"})
		if _, _, err := loadEmbeddingInput(path); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	})
	t.Run("missing_credentials", func(t *testing.T) {
		in := embeddingFixture(t)
		t.Setenv("EVAL11_ENDPOINT", "https://embedding.invalid")
		t.Setenv("EVAL11_KEY", "")
		if _, _, err := loadEmbeddingInput(writeManifestFixture(t, in)); err == nil {
			t.Fatal("missing key accepted")
		}
	})
}

func embeddingSyntheticRuns(in embeddingInput) []embeddingRun {
	var runs []embeddingRun
	for _, s := range in.Samples {
		for i, v := range in.Variants {
			for repeat := 1; repeat <= 10; repeat++ {
				yes, no := 0.8, 0.2
				if i == 1 {
					yes, no = 0.6, 0.1
				}
				r := embeddingRun{Variant: v.Code, Split: s.Split, Sample: s.Code, Unit: s.Unit, Repeat: repeat, Cost: -1, Preparation: preparationMetrics{DurationMS: 1, EmbeddingCalls: 5, VectorRows: 5, VectorBytes: 500}, Calls: 2, DurationMS: 2,
					Relations: []embeddingScore{{"yes", yes, true}, {"no", no, false}}, Signals: []embeddingSignal{{s.Code + ".hit", false, true, 1, false}, {s.Code + ".miss", false, true, -0.2, true}}}
				if s.Split == "evaluation" {
					var qs []queryMetrics
					for _, q := range s.Queries {
						qs = append(qs, queryMetrics{ID: s.Code + "." + q.ID, IndependentUnit: s.Unit, UseCase: q.UseCase, Recall: 1, ReciprocalRank: 1, BudgetPerHit: 20, BudgetUsed: 20, EvidenceCompleteness: 1, LatencyMS: 2, Misrouted: -1})
					}
					r.Metrics = &runMetrics{Stage: v.Code, Repeat: repeat, UseCases: map[string]useCaseMetrics{useCaseFact: {Queries: 2}}, QuerySamples: qs}
				}
				runs = append(runs, r)
			}
		}
	}
	return runs
}

func TestEmbeddingCalibrationAndDenominators(t *testing.T) {
	in := embeddingFixture(t)
	runs := embeddingSyntheticRuns(in)
	a, err := tuneEmbeddingThresholds(in, in.Variants[0], runs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tuneEmbeddingThresholds(in, in.Variants[1], runs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Relation != 0.3 || b.Relation != 0.2 || a.Global != 0.8 || b.Global != 0.9 {
		t.Fatalf("per-model calibration: %+v %+v", a, b)
	}
	// 최종 점수/정답을 바꿔도 조정 결과는 바뀌지 않는다.
	for i := range runs {
		if runs[i].Split == "evaluation" {
			runs[i].Relations[0].Score = -0.5
			runs[i].Relations[0].Relevant = false
		}
	}
	again, err := tuneEmbeddingThresholds(in, in.Variants[0], runs)
	if err != nil || again != a {
		t.Fatalf("held out leaked: %+v %v", again, err)
	}
	run := embeddingSyntheticRuns(in)[0]
	run.Signals = []embeddingSignal{{Query: "a", Expected: false}, {Query: "b", Semantic: true, Top: 0.9, Expected: true}, {Query: "c", Keyword: true, Expected: false}}
	values := embeddingValues(run, embeddingThresholds{Relation: 0.3, Global: 0.5}, 1)
	if values["relation_precision"] != 1 || values["relation_recall"] != 1 || values["relation_candidates"] != 1 || values["global_false_pass"] != 0.5 || values["global_false_reject"] != 1 {
		t.Fatal(values)
	}
	run.Signals = nil
	run.Relations = nil
	values = embeddingValues(run, embeddingThresholds{}, 1)
	if values["relation_precision"] != -1 || values["relation_recall"] != -1 || values["global_false_pass"] != -1 || values["global_false_reject"] != -1 {
		t.Fatal("unmeasured denominator", values)
	}
}

func TestEmbeddingAggregationAndIntegrity(t *testing.T) {
	in := embeddingFixture(t)
	report := embeddingReport{Conditions: []embeddingCondition{{Code: "base"}, {Code: "candidate"}}, Runs: embeddingSyntheticRuns(in)}
	for i, v := range in.Variants {
		selected, err := tuneEmbeddingThresholds(in, v, report.Runs)
		if err != nil {
			t.Fatal(err)
		}
		report.Conditions[i].Selected = selected
	}
	if err := aggregateEmbedding(in, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "deferred" || report.Activated || report.Estimates["base"]["relation_precision"].Samples != 1 || report.Estimates["base"]["relation_precision"].Interval != -1 {
		t.Fatalf("repeats are not independent: %+v", report.Estimates)
	}
	raw, err := json.Marshal(report)
	if err != nil || strings.Contains(string(raw), "private-") {
		t.Fatalf("report privacy %v", err)
	}
	for name, change := range map[string]func([]embeddingRun) []embeddingRun{
		"missing":         func(r []embeddingRun) []embeddingRun { return r[1:] },
		"duplicate":       func(r []embeddingRun) []embeddingRun { r[0] = r[1]; return r },
		"unit_change":     func(r []embeddingRun) []embeddingRun { r[0].Unit = "different"; return r },
		"pair_change":     func(r []embeddingRun) []embeddingRun { r[0].Relations[0].Relevant = false; return r },
		"query_change":    func(r []embeddingRun) []embeddingRun { r[0].Signals[0].Query = "different"; return r },
		"nan":             func(r []embeddingRun) []embeddingRun { r[0].Relations[0].Score = math.NaN(); return r },
		"unmeasured_zero": func(r []embeddingRun) []embeddingRun { r[0].Cost = 0; return r },
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateEmbeddingRuns(in, change(embeddingSyntheticRuns(in))); err == nil {
				t.Fatal("invalid runs accepted")
			}
		})
	}
	t.Run("order_independent", func(t *testing.T) {
		slices.Reverse(report.Runs)
		if err := aggregateEmbedding(in, &report); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no_tuning_denominators", func(t *testing.T) {
		r := embeddingSyntheticRuns(in)
		for i := range r {
			for j := range r[i].Signals {
				r[i].Signals[j].Expected = false
			}
		}
		if _, err := tuneEmbeddingThresholds(in, in.Variants[0], r); err == nil {
			t.Fatal("missing positives accepted")
		}
	})
	t.Run("input_file", func(t *testing.T) {
		in := embeddingFixture(t)
		t.Setenv("EVAL11_ENDPOINT", "https://embedding.invalid")
		t.Setenv("EVAL11_KEY", "synthetic-secret")
		t.Setenv("EVAL11_BASE_DB", "postgres://unused")
		t.Setenv("EVAL11_OTHER_DB", "postgres://unused")
		path := writeManifestFixture(t, in)
		if _, _, err := loadEmbeddingInput(path); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(path)
	})
}

func TestEmbeddingAssessmentRequiresBothPurposes(t *testing.T) {
	fixture := func() embeddingReport {
		r := embeddingReport{}
		for _, name := range []string{"relation_precision", "relation_recall", "global_false_pass", "global_false_reject"} {
			r.Comparisons = append(r.Comparisons, comparison{Metric: name, Samples: 2})
		}
		var metrics []comparison
		for _, name := range []string{"recall", "reciprocal_rank", "evidence_completeness", "budget_per_hit", "latency_ms"} {
			metrics = append(metrics, comparison{Metric: name, Samples: 2, Significant: name == "latency_ms", Improved: true})
		}
		r.Retrieval = []stageJudgement{{}, {Comparisons: map[string][]comparison{useCaseFact: metrics}}}
		return r
	}
	for _, test := range []struct {
		name, provenance, want string
		change                 func(*embeddingReport)
	}{
		{"real_candidate", "observed", "candidate", func(*embeddingReport) {}},
		{"synthetic_deferred", "synthetic", "deferred", func(*embeddingReport) {}},
		{"relation_loss", "observed", "rejected", func(r *embeddingReport) { r.Comparisons[0].Significant = true }},
		{"global_loss", "observed", "rejected", func(r *embeddingReport) { r.Comparisons[3].Significant = true }},
		{"ranking_loss", "observed", "rejected", func(r *embeddingReport) {
			r.Retrieval[1].Comparisons[useCaseFact][0].Significant = true
			r.Retrieval[1].Comparisons[useCaseFact][0].Improved = false
		}},
		{"one_independent_unit", "observed", "deferred", func(r *embeddingReport) { r.Comparisons[0].Samples = 1 }},
		{"evidence_missing", "observed", "deferred", func(r *embeddingReport) { r.Retrieval[1].Comparisons[useCaseFact][2].Samples = 0 }},
		{"no_retrieval", "observed", "deferred", func(r *embeddingReport) { r.Retrieval[1].Comparisons = nil }},
		{"no_improvement", "observed", "deferred", func(r *embeddingReport) { r.Retrieval[1].Comparisons[useCaseFact][4].Significant = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := fixture()
			test.change(&r)
			status, _ := assessEmbedding(embeddingInput{Provenance: test.provenance}, r)
			if status != test.want || r.Activated {
				t.Fatalf("%s != %s", status, test.want)
			}
		})
	}
	// 비용 개선은 HTTP 시도 수·벡터 열 바이트·실측 금액의 유의한 감소이며 사유에서 지연과 구분한다.
	cost := func(metric string, samples int, significant, improved bool) func(*embeddingReport) {
		return func(r *embeddingReport) {
			r.Comparisons = append(r.Comparisons, comparison{Metric: metric, Samples: samples, Significant: significant, Improved: improved})
		}
	}
	noLatency := func(r *embeddingReport) { r.Retrieval[1].Comparisons[useCaseFact][4].Significant = false }
	for _, test := range []struct {
		name, provenance, status, reason string
		changes                          []func(*embeddingReport)
	}{
		{"latency_only", "observed", "candidate", "quality_preserved_latency_improved_final_acceptance_pending", nil},
		{"storage_only", "observed", "candidate", "quality_preserved_cost_improved_final_acceptance_pending", []func(*embeddingReport){noLatency, cost("vector_column_bytes", 5, true, true)}},
		{"document_calls_only", "observed", "candidate", "quality_preserved_cost_improved_final_acceptance_pending", []func(*embeddingReport){noLatency, cost("document_embedding_http_attempts", 5, true, true)}},
		{"measurement_calls_only", "observed", "candidate", "quality_preserved_cost_improved_final_acceptance_pending", []func(*embeddingReport){noLatency, cost("measurement_embedding_http_attempts", 5, true, true)}},
		{"measured_amount_only", "observed", "candidate", "quality_preserved_cost_improved_final_acceptance_pending", []func(*embeddingReport){noLatency, cost("embedding_cost", 5, true, true)}},
		{"latency_and_cost", "observed", "candidate", "quality_preserved_latency_and_cost_improved_final_acceptance_pending", []func(*embeddingReport){cost("vector_column_bytes", 5, true, true)}},
		{"unmeasured_amount", "observed", "deferred", "no_significant_measured_latency_or_cost_improvement", []func(*embeddingReport){noLatency, cost("embedding_cost", 0, false, false)}},
		{"storage_increase", "observed", "deferred", "no_significant_measured_latency_or_cost_improvement", []func(*embeddingReport){noLatency, cost("vector_column_bytes", 5, true, false)}},
		{"storage_not_significant", "observed", "deferred", "no_significant_measured_latency_or_cost_improvement", []func(*embeddingReport){noLatency, cost("vector_column_bytes", 5, false, true)}},
		{"storage_one_unit", "observed", "deferred", "no_significant_measured_latency_or_cost_improvement", []func(*embeddingReport){noLatency, cost("vector_column_bytes", 1, true, true)}},
		{"non_cost_metric", "observed", "deferred", "no_significant_measured_latency_or_cost_improvement", []func(*embeddingReport){noLatency, cost("preparation_ms", 5, true, true)}},
		{"storage_synthetic", "synthetic", "deferred", "synthetic_provider", []func(*embeddingReport){noLatency, cost("vector_column_bytes", 5, true, true)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := fixture()
			for _, change := range test.changes {
				change(&r)
			}
			status, reason := assessEmbedding(embeddingInput{Provenance: test.provenance}, r)
			if status != test.status || reason != test.reason {
				t.Fatalf("판정 = %s/%s, 기대 %s/%s", status, reason, test.status, test.reason)
			}
		})
	}
}

// TestEmbeddingStorageReductionCandidate는 지연 차이 없이 벡터 열 바이트만 유의하게 줄어든
// 압축 비교가 집계를 거쳐 비용 개선 후보가 되는지 확인한다. 합성 출처는 계속 보류한다.
func TestEmbeddingStorageReductionCandidate(t *testing.T) {
	for _, test := range []struct{ provenance, status, reason string }{
		{"observed", "candidate", "quality_preserved_cost_improved_final_acceptance_pending"},
		{"synthetic", "deferred", "synthetic_provider"},
	} {
		t.Run(test.provenance, func(t *testing.T) {
			in := embeddingFixture(t)
			in.Provenance, in.Axis = test.provenance, "compression"
			in.Variants[1].Dimension, in.Variants[1].VectorType = in.Variants[0].Dimension, "halfvec"
			evaluation := in.Samples[1]
			for i := 2; i <= 5; i++ {
				extra := evaluation
				extra.Code, extra.Unit = fmt.Sprintf("evaluation%d", i), fmt.Sprintf("evaluation-unit-%d", i)
				in.Samples = append(in.Samples, extra)
			}
			runs := embeddingSyntheticRuns(in)
			reductions := map[string]int64{"evaluation-unit": 2000, "evaluation-unit-2": 3000, "evaluation-unit-3": 2500, "evaluation-unit-4": 1800, "evaluation-unit-5": 2200}
			for i := range runs {
				if runs[i].Split != "evaluation" {
					continue
				}
				runs[i].Preparation.VectorBytes = 5000
				if runs[i].Variant == "candidate" {
					runs[i].Preparation.VectorBytes -= reductions[runs[i].Unit]
				}
			}
			report := embeddingReport{Conditions: []embeddingCondition{{Code: "base"}, {Code: "candidate"}}, Runs: runs}
			for i, v := range in.Variants {
				selected, err := tuneEmbeddingThresholds(in, v, runs)
				if err != nil {
					t.Fatal(err)
				}
				report.Conditions[i].Selected = selected
			}
			if err := aggregateEmbedding(in, &report); err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(report.Comparisons, func(c comparison) bool { return c.Metric == "vector_column_bytes" })
			if i < 0 || report.Comparisons[i].Samples != 5 || report.Comparisons[i].Difference != -2300 || !report.Comparisons[i].Significant || !report.Comparisons[i].Improved {
				t.Fatalf("벡터 열 바이트 비교 = %+v", report.Comparisons)
			}
			if report.Status != test.status || report.Reason != test.reason || report.Activated {
				t.Fatalf("판정 = %s/%s, 기대 %s/%s", report.Status, report.Reason, test.status, test.reason)
			}
		})
	}
}
