package main

import "testing"

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
