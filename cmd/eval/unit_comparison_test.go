package main

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func unitFixture(t *testing.T, perCohort int) unitInput {
	t.Helper()
	input := unitInput{Version: "unit-v1", Provenance: "synthetic", Agent: unitAgent{"agent-v1", "agent-model", "prompt-family-v1"}, Judge: unitJudge{"judge-v1", "judge-model", "judge-prompt-v1"}, Pricing: "pricing-v1", Currency: "USD",
		Policies: []unitPolicy{{"current", "current-v1"}, {"semantic", "semantic-v1"}},
		Search:   unitSearch{Stage: "references", Budget: 20000, MaxHops: 2, MaxHopNodes: 100, IndexTargets: "all_layers", EmbeddingModel: "gemini:gemini-embedding-2:retrieval-v1:vector:768", Execution: "parallel", CandidateLimit: 50, SemanticThreshold: 0.5, FoldThreshold: 0.95}}
	for _, cohort := range []string{"conversation", "code_document"} {
		for i := range perCohort {
			sample := unitSample{Code: fmt.Sprintf("%s-%d", cohort, i), Cohort: cohort,
				Sources: []contextSpec{
					{Key: "source1", Layer: "source", Body: "private-source: 담당자는 배포가 승인되면 실행하되 주말에는 실행하지 않는다.", Source: &sourceSpec{Channel: "conversation", Locator: "urn:private:source1", OccurredAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), OriginKind: "user_utterance"}},
					{Key: "source2", Layer: "source", Body: "private-source: 앞서 정한 담당자가 그 예외도 적용하기로 결정했다.", Source: &sourceSpec{Channel: "conversation", Locator: "urn:private:source2", OccurredAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), OriginKind: "user_utterance"}},
				}, Queries: []querySpec{{ID: "query1", UseCase: useCaseFact, WorkContext: "private-question: 담당자의 실행 조건과 예외는?"}, {ID: "query2", UseCase: useCaseFact, WorkContext: "private-question: 이후 결정된 담당자의 역할은?"}}}
			for j := range sample.Queries {
				sample.Queries[j].AsOf = new(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
			}
			if cohort == "code_document" {
				sample.Sources[0].Body = "private-source: func deploy(approved, weekend bool) bool { return approved && !weekend }"
				sample.Sources[1].Body = "private-source: 담당자는 앞 함수의 승인 조건과 주말 예외를 배포 결정에 적용한다."
				for j := range sample.Sources {
					sample.Sources[j].Source.Channel = "file"
					sample.Sources[j].Source.OriginKind = "external_content"
				}
			}
			kinds := []string{"subject", "condition", "exception", "negation", "reference", "multi_turn_decision"}
			var all []string
			for _, kind := range kinds {
				sample.Elements = append(sample.Elements, unitElement{Code: kind, Kind: kind, Meaning: "private-element: " + kind, Evidence: []string{"source1", "source2"}})
				all = append(all, kind)
			}
			for p, policy := range input.Policies {
				variant := unitVariant{Policy: policy.Name}
				count := 2 - p
				for j := range count {
					spec := contextSpec{Key: fmt.Sprintf("derived%d", j), Layer: "derived", Body: "private-derived: 담당자는 승인과 예외를 적용한다.", Derived: &derivedSpec{Kind: "proposition", DerivedFrom: []string{"source1", "source2"}, EvidenceState: "observation"}}
					preserved := slices.Clone(all)
					if p == 0 {
						spec.Body = fmt.Sprintf("private-derived: 분리한 일부 조건 %d", j)
						preserved = preserved[:2]
					}
					variant.Derived = append(variant.Derived, spec)
					variant.Reviews = append(variant.Reviews, unitReview{Key: spec.Key, Required: slices.Clone(all), Preserved: slices.Clone(preserved), Human: &humanReview{"reviewer-test", "human-v1", slices.Clone(preserved)}})
				}
				for _, query := range sample.Queries {
					variant.Answers = append(variant.Answers, unitAnswer{Query: query.ID, Answers: []string{"derived0"}, Required: []string{"source1", "derived0"}, Paths: [][]string{{"derived0", "source1"}}})
				}
				for repeat := 1; repeat <= initialEvaluationRepeats; repeat++ {
					variant.Tasks = append(variant.Tasks, unitTask{Repeat: repeat, Trace: "synthetic-trace-v1", Success: new(true), TotalCost: new(float64(10 - p*5))})
				}
				sample.Variants = append(sample.Variants, variant)
			}
			input.Samples = append(input.Samples, sample)
		}
	}
	refreshUnitHashes(&input)
	return input
}

func refreshUnitHashes(input *unitInput) {
	for si := range input.Samples {
		sample := &input.Samples[si]
		for vi := range sample.Variants {
			variant := &sample.Variants[vi]
			for j := range variant.Reviews {
				variant.Reviews[j].SHA256 = unitReviewHash(input.Judge, *sample, variant.Derived[j], variant.Reviews[j].Required)
			}
			for j := range variant.Tasks {
				variant.Tasks[j].InputHash = unitTaskHash(*input, *sample, *variant)
			}
		}
	}
}

func unitFixtureRuns(input unitInput) []unitRun {
	var result []unitRun
	for _, sample := range input.Samples {
		for _, variant := range sample.Variants {
			for repeat := 1; repeat <= initialEvaluationRepeats; repeat++ {
				run := unitRun{Sample: sample.Code, Cohort: sample.Cohort, Review: reviewUnit(sample, variant), Task: unitTaskMetrics{Success: -1, TotalCost: -1}, Metrics: runMetrics{Stage: variant.Policy, Repeat: repeat, Preparation: &preparationMetrics{EmbeddingCalls: int64(len(variant.Derived) + 2), VectorBytes: int64(len(variant.Derived)+2) * 3000}}}
				task := variant.Tasks[repeat-1]
				if task.Success != nil {
					run.Task.Success = 0
					if *task.Success {
						run.Task.Success = 1
					}
				}
				if task.TotalCost != nil {
					run.Task.TotalCost = *task.TotalCost
				}
				for _, q := range unitQueries(input, sample, variant).Queries {
					run.Metrics.QuerySamples = append(run.Metrics.QuerySamples, queryMetrics{ID: q.ID, IndependentUnit: sample.Code, DatasetType: sample.Cohort, UseCase: q.UseCase,
						Recall: 1, ReciprocalRank: 1, BudgetUsed: 100, BudgetPerHit: 100, EvidenceCompleteness: 1, PathContinuity: 1, LatencyMS: 1, Misrouted: -1})
				}
				result = append(result, run)
			}
		}
	}
	return result
}

func TestUnitComparisonInputContracts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*unitInput)
		rehash bool
	}{
		{"valid", func(*unitInput) {}, false},
		{"source_changed", func(i *unitInput) { i.Samples[0].Sources[0].Body += "changed" }, false},
		{"boundary_changed", func(i *unitInput) { i.Policies[0].Boundary += "changed" }, false},
		{"derived_changed", func(i *unitInput) { i.Samples[0].Variants[0].Derived[0].Body += "changed" }, false},
		{"required_changed", func(i *unitInput) { i.Samples[0].Elements[0].Meaning += "changed" }, false},
		{"judge_changed", func(i *unitInput) { i.Judge.Prompt += "changed" }, false},
		{"agent_changed", func(i *unitInput) { i.Agent.Model += "changed" }, false},
		{"task_search_changed", func(i *unitInput) { i.Search.Budget++ }, false},
		{"task_answers_changed", func(i *unitInput) { i.Samples[0].Variants[0].Answers[0].Answers = []string{"source1"} }, false},
		{"policy_axis", func(i *unitInput) { i.Policies[1].Name = "other" }, false},
		{"duplicate_sample", func(i *unitInput) { i.Samples[1].Code = i.Samples[0].Code }, true},
		{"source_in_derived", func(i *unitInput) { i.Samples[0].Variants[0].Derived[0] = i.Samples[0].Sources[0] }, false},
		{"unknown_element", func(i *unitInput) { i.Samples[0].Variants[0].Reviews[0].Preserved = []string{"missing"} }, false},
		{"no_review", func(i *unitInput) { i.Samples[0].Variants[0].Reviews = nil }, false},
		{"missing_coverage", func(i *unitInput) {
			for j := range i.Samples[0].Variants[0].Reviews {
				r := &i.Samples[0].Variants[0].Reviews[j]
				r.Required = r.Required[:2]
			}
		}, true},
		{"missing_origin", func(i *unitInput) { i.Samples[0].Sources[0].Source = nil }, false},
		{"event_added", func(i *unitInput) { i.Samples[0].Variants[0].Derived[0].Event = &eventSpec{} }, false},
		{"changed_kind", func(i *unitInput) { i.Samples[0].Variants[1].Derived[0].Derived.Kind = "summary" }, true},
		{"nonauto_query", func(i *unitInput) { i.Samples[0].Queries[0].Scope = "local" }, true},
		{"query_answer_leak", func(i *unitInput) { i.Samples[0].Queries[0].Answers = []string{"source1"} }, true},
		{"repeated_task", func(i *unitInput) { i.Samples[0].Variants[0].Tasks[1].Repeat = 1 }, false},
		{"missing_task", func(i *unitInput) { i.Samples[0].Variants[0].Tasks = i.Samples[0].Variants[0].Tasks[:9] }, false},
		{"bad_cost", func(i *unitInput) { i.Samples[0].Variants[0].Tasks[0].TotalCost = new(math.Inf(1)) }, false},
		{"no_human", func(i *unitInput) { i.Samples[0].Variants[0].Reviews[0].Human = nil }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := unitFixture(t, 1)
			test.mutate(&input)
			if test.rehash {
				refreshUnitHashes(&input)
			}
			err := validateUnitInput(input)
			valid := test.name == "valid" || test.name == "no_human"
			if (err == nil) != valid {
				t.Fatalf("valid=%v: %v", valid, err)
			}
		})
	}
}

func TestUnitCandidateBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, status string
		change       func(*unitInput, []unitRun)
	}{
		{"human_missing", "deferred", func(_ *unitInput, runs []unitRun) {
			for j := range runs {
				runs[j].Review.HumanReviewed = 0
			}
		}},
		{"human_disagrees", "deferred", func(_ *unitInput, runs []unitRun) {
			for j := range runs {
				runs[j].Review.Disagreements = 1
			}
		}},
		{"partial_annotation", "deferred", func(_ *unitInput, runs []unitRun) {
			for j := range runs {
				runs[j].Metrics.QuerySamples[0].PathContinuity = -1
			}
		}},
		{"unit_regression", "rejected", func(_ *unitInput, runs []unitRun) {
			for j := range runs {
				if runs[j].Metrics.Stage == "current" {
					runs[j].Review.Completeness = 1
				} else {
					runs[j].Review.Completeness = 0
				}
			}
		}},
		{"search_regression", "rejected", func(_ *unitInput, runs []unitRun) {
			for j := range runs {
				if runs[j].Metrics.Stage == "semantic" {
					for k := range runs[j].Metrics.QuerySamples {
						runs[j].Metrics.QuerySamples[k].Recall = 0
					}
				}
			}
		}},
		{"cost_unmeasured", "deferred", func(_ *unitInput, runs []unitRun) {
			for j := range runs {
				runs[j].Task.TotalCost = -1
			}
		}},
		{"budget_improves", "candidate", func(_ *unitInput, runs []unitRun) {
			for j := range runs {
				runs[j].Task.TotalCost = -1
				if runs[j].Metrics.Stage == "semantic" {
					for k := range runs[j].Metrics.QuerySamples {
						runs[j].Metrics.QuerySamples[k].BudgetUsed = 50
					}
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := unitFixture(t, 2)
			input.Provenance = "agent_trace"
			runs := unitFixtureRuns(input)
			test.change(&input, runs)
			cohorts, err := aggregateUnits(input, runs)
			if err != nil {
				t.Fatal(err)
			}
			for _, cohort := range cohorts {
				if cohort.Status != test.status || cohort.Activated {
					t.Fatalf("want=%s: %+v", test.status, cohort)
				}
			}
		})
	}
}

func TestUnitCompletenessSeparatesMeaningAndLinks(t *testing.T) {
	input := unitFixture(t, 1)
	sample := input.Samples[0]
	variant := sample.Variants[1]
	if got := reviewUnit(sample, variant); got.Completeness != 1 || got.HumanReviewed != 1 {
		t.Fatal(got)
	}
	variant.Derived[0].Derived.DerivedFrom = []string{"source1"}
	if got := reviewUnit(sample, variant); got.Completeness != 0 || got.MissingLinks != 6 {
		t.Fatal(got)
	}
	variant.Derived[0].Derived.DerivedFrom = []string{"source1", "source2"}
	variant.Reviews[0].Human.Preserved = []string{}
	if got := reviewUnit(sample, variant); got.Completeness != 0 || got.Disagreements != 1 {
		t.Fatal(got)
	}
}

func TestUnitCohortsAndIndependentAggregation(t *testing.T) {
	input := unitFixture(t, 2)
	runs := unitFixtureRuns(input)
	cohorts, err := aggregateUnits(input, runs)
	if err != nil || len(cohorts) != 2 {
		t.Fatalf("%+v %v", cohorts, err)
	}
	for _, cohort := range cohorts {
		if cohort.Samples != 2 || cohort.Totals["current"].Derived != 4 || cohort.Totals["semantic"].Derived != 2 || cohort.Totals["current"].Completeness != 0 || cohort.Totals["semantic"].Completeness != 1 {
			t.Fatal(cohort)
		}
		if cohort.Estimates["semantic"]["unit_completeness"].Samples != 2 || cohort.Retrieval[1].Estimates[useCaseFact]["recall"].Samples != 2 || cohort.Reason != "synthetic_execution_only" {
			t.Fatal(cohort)
		}
	}
	input.Provenance = "agent_trace"
	accepted, err := aggregateUnits(input, runs)
	if err != nil || accepted[0].Status != "candidate" || accepted[0].Activated {
		t.Fatalf("%+v %v", accepted, err)
	}
	// 같은 표본 안의 한 회차 미측정을 평균으로 숨기지 않는다.
	runs[0].Task.TotalCost, runs[0].Task.Success = -1, -1
	deferred, err := aggregateUnits(input, runs)
	if err != nil || deferred[0].Status != "deferred" || deferred[0].Estimates["current"]["task_success"].Samples != 1 {
		t.Fatalf("%+v %v", deferred, err)
	}
	if _, err := aggregateUnits(input, runs[:len(runs)-1]); err == nil {
		t.Fatal("누락 회차 허용")
	}
	duplicate := slices.Clone(runs)
	duplicate[1] = duplicate[0]
	if _, err := aggregateUnits(input, duplicate); err == nil {
		t.Fatal("중복 회차 허용")
	}
	for i := range runs {
		if runs[i].Metrics.Stage == "semantic" {
			runs[i].Task.Success = 0
		}
	}
	regressed, err := aggregateUnits(input, runs)
	if err != nil || regressed[1].Status != "rejected" {
		t.Fatalf("%+v %v", regressed, err)
	}
}

func TestUnitComparisonStrictJSONAndPrivacy(t *testing.T) {
	input := unitFixture(t, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "input.json")
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUnitInput(path); err != nil {
		t.Fatal(err)
	}
	cohorts, err := aggregateUnits(input, unitFixtureRuns(input))
	if err != nil {
		t.Fatal(err)
	}
	for _, cohort := range cohorts {
		if cohort.Comparisons[0].Interval != -1 || cohort.Samples != 1 {
			t.Fatal(cohort)
		}
	}
	report, err := json.Marshal(unitReport{Version: input.Version, Runs: unitFixtureRuns(input), Cohorts: cohorts})
	if err != nil || bytes.Contains(report, []byte("private-")) || bytes.Contains(report, []byte("urn:")) {
		t.Fatalf("보고서 유출: %v", err)
	}
	raw = append([]byte(`{"private-unknown":true,`), raw[1:]...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUnitInput(path); err == nil || bytes.Contains([]byte(err.Error()), []byte("private-unknown")) {
		t.Fatalf("엄격한 JSON: %v", err)
	}
}

// TestUnitTaskSuccessRejectsOnlySignificantLoss는 과업 성공률 하락을 확장 업무 판정과 같이
// 유의한 악화일 때만 거부하고, 독립 단위 부족은 보류로 남기는지 확인한다.
func TestUnitTaskSuccessRejectsOnlySignificantLoss(t *testing.T) {
	input := unitFixture(t, 2)
	input.Provenance = "agent_trace"
	cohorts, err := aggregateUnits(input, unitFixtureRuns(input))
	if err != nil || cohorts[0].Status != "candidate" {
		t.Fatalf("기준 후보 = %+v, %v", cohorts, err)
	}
	for _, test := range []struct {
		name, status, reason string
		change               func(*unitCohort, *comparison)
	}{
		{"not_significant", "candidate", cohorts[0].Reason, func(_ *unitCohort, c *comparison) {
			c.Difference, c.Interval, c.Significant, c.Improved = -0.01, 0.05, false, false
		}},
		{"one_unit", "deferred", "unmeasured_or_insufficient_independent_units", func(entry *unitCohort, c *comparison) {
			entry.Samples = 1
			c.Samples, c.Difference, c.Interval, c.Significant, c.Improved = 1, -0.1, -1, false, false
		}},
		{"significant_loss", "rejected", "quality_or_task_regression", func(_ *unitCohort, c *comparison) {
			c.Difference, c.Interval, c.Significant, c.Improved = -0.5, 0.1, true, false
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := cohorts[0]
			entry.Comparisons = slices.Clone(entry.Comparisons)
			i := slices.IndexFunc(entry.Comparisons, func(c comparison) bool { return c.Metric == "task_success" })
			if i < 0 {
				t.Fatal("과업 성공률 비교 없음")
			}
			test.change(&entry, &entry.Comparisons[i])
			status, reason := assessUnits(input.Provenance, entry)
			if status != test.status || reason != test.reason {
				t.Fatalf("판정 = %s/%s, 기대 %s/%s", status, reason, test.status, test.reason)
			}
		})
	}
}
