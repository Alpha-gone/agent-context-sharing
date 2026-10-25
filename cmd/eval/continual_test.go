package main

import (
	"math"
	"reflect"
	"slices"
	"testing"
)

func TestContinualScenariosAreCompleteAndValid(t *testing.T) {
	scenarios := continualScenarios()
	if len(scenarios) != 6 {
		t.Fatalf("지속 평가 시나리오 = %d개, want 6개", len(scenarios))
	}
	seen := map[string]bool{}
	for _, scenario := range scenarios {
		if seen[scenario.name] {
			t.Fatalf("지속 평가 시나리오 %q가 중복됐다", scenario.name)
		}
		seen[scenario.name] = true
		keys := map[string]string{}
		for _, value := range scenario.contexts.Contexts {
			keys[value.Key] = value.Layer
		}
		for _, value := range scenario.contexts.Contexts {
			if err := validateContextSpec(value, keys); err != nil {
				t.Fatalf("시나리오 %q 컨텍스트 %q: %v", scenario.name, value.Key, err)
			}
		}
		for _, answer := range scenario.query.Answers {
			if _, present := keys[answer]; !present {
				t.Fatalf("시나리오 %q의 정답 %q가 없다", scenario.name, answer)
			}
		}
	}
}

func TestJudgeContinualRejectsSignificantRegression(t *testing.T) {
	runs := make([]continualRun, 0, minimumRepeats)
	for repeat := 1; repeat <= minimumRepeats; repeat++ {
		runs = append(runs, continualRun{
			Scenario: "online_update", Repeat: repeat, StateVerified: true, JudgmentVerified: true,
			Baseline: queryMetrics{Recall: 1, ReciprocalRank: 1, BudgetPerHit: 100},
			Current:  queryMetrics{Recall: 0, ReciprocalRank: 0, BudgetPerHit: 200},
		})
	}
	judgements := judgeContinual(runs)
	if len(judgements) == 0 || judgements[0].Passed {
		t.Fatalf("품질 회귀가 합격 처리됐다: %#v", judgements)
	}
}

func continualPairedRuns() []continualRun {
	var runs []continualRun
	for _, unit := range []string{"task-a", "task-b"} {
		for repeat := 1; repeat <= initialEvaluationRepeats; repeat++ {
			runs = append(runs, continualRun{Scenario: "online_update", IndependentUnit: unit, Repeat: repeat, StateVerified: true, JudgmentVerified: true,
				Baseline: queryMetrics{Recall: 1, ReciprocalRank: 1, BudgetPerHit: 100}, Current: queryMetrics{Recall: 1, ReciprocalRank: 1, BudgetPerHit: 100}})
		}
	}
	return runs
}

func TestContinualJudgementStatesAndCoverage(t *testing.T) {
	for _, test := range []struct {
		name, status, reason string
		change               func([]continualRun) []continualRun
	}{
		{"maintained", "passed", "", func(r []continualRun) []continualRun { return r }},
		{"empty", "deferred", "missing_runs", func([]continualRun) []continualRun { return nil }},
		{"one_unit", "deferred", "insufficient_independent_units", func(r []continualRun) []continualRun { return r[:10] }},
		{"missing_repeat", "deferred", "incomplete_or_duplicate_repeats", func(r []continualRun) []continualRun { return r[1:] }},
		{"duplicate_repeat", "deferred", "incomplete_or_duplicate_repeats", func(r []continualRun) []continualRun { r[0].Repeat = 2; return r }},
		{"invalid_repeat", "deferred", "incomplete_or_duplicate_repeats", func(r []continualRun) []continualRun { r[0].Repeat = 0; return r }},
		{"missing_metric", "deferred", "unmeasured_metrics", func(r []continualRun) []continualRun { r[0].Current.BudgetPerHit = -1; return r }},
		{"nan", "deferred", "unmeasured_metrics", func(r []continualRun) []continualRun { r[0].Current.Recall = math.NaN(); return r }},
		{"infinite", "deferred", "unmeasured_metrics", func(r []continualRun) []continualRun { r[0].Baseline.ReciprocalRank = math.Inf(1); return r }},
		{"recall_regression", "failed", "quality_regression", func(r []continualRun) []continualRun {
			for i := range r {
				r[i].Current.Recall = 0
			}
			return r
		}},
		{"rank_regression", "failed", "quality_regression", func(r []continualRun) []continualRun {
			for i := range r {
				r[i].Current.ReciprocalRank = 0.5
			}
			return r
		}},
		{"budget_regression", "failed", "quality_regression", func(r []continualRun) []continualRun {
			for i := range r {
				r[i].Current.BudgetPerHit = 200
			}
			return r
		}},
		{"state_violation", "failed", "state_or_record_violation", func(r []continualRun) []continualRun { r[0].StateVerified = false; return r }},
		{"record_violation", "failed", "state_or_record_violation", func(r []continualRun) []continualRun { r[0].JudgmentVerified = false; return r }},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := judgeContinual(test.change(continualPairedRuns()))
			if len(got) != 6 || got[0].Status != test.status || got[0].Reason != test.reason || got[0].Passed != (test.status == "passed") || len(got[0].Comparisons) != 3 {
				t.Fatalf("지속 판정: %+v", got)
			}
			for _, missing := range got[1:] {
				if missing.Status != "deferred" || missing.Reason != "missing_runs" || missing.Passed || len(missing.Comparisons) != 3 || missing.Comparisons[0].Status != "unmeasured" {
					t.Fatalf("누락 시나리오를 정상 처리했다: %+v", missing)
				}
			}
		})
	}
}

func TestContinualJudgementOrderIsDeterministic(t *testing.T) {
	runs := continualPairedRuns()
	for i := range runs {
		runs[i].Current.BudgetPerHit = 1 / float64(i+1)
	}
	before := judgeContinual(runs)
	slices.Reverse(runs)
	if after := judgeContinual(runs); !reflect.DeepEqual(before, after) {
		t.Fatal("입력 순서가 지속 집계를 바꿨다")
	}
}
