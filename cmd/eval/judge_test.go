package main

import (
	"math"
	"testing"
)

// stageRuns는 회차별 재현율만 다르게 둔 측정값을 만든다. 판정은 지표 값만 보므로
// 나머지 필드는 판정에 영향을 주지 않는다.
func stageRuns(stage string, recalls ...float64) []runMetrics {
	runs := make([]runMetrics, 0, len(recalls))
	for index, recall := range recalls {
		runs = append(runs, runMetrics{
			Stage:  stage,
			Repeat: index + 1,
			UseCases: map[string]useCaseMetrics{
				useCaseFact: {Queries: 10, Recall: recall, ReciprocalRank: recall, BudgetPerHit: 100},
			},
		})
	}
	return runs
}

func TestJudgeMarksSignificantImprovement(t *testing.T) {
	runs := map[string][]runMetrics{
		"baseline":   stageRuns("baseline", 0.40, 0.41, 0.39),
		"references": stageRuns("references", 0.70, 0.71, 0.69),
	}
	judgements := judge([]string{"baseline", "references"}, runs)
	if len(judgements) != 2 {
		t.Fatalf("단계 수가 다르다: %d", len(judgements))
	}
	if judgements[0].ComparedTo != "" || judgements[0].Comparisons != nil {
		t.Fatal("첫 단계는 견줄 대상이 없어야 한다")
	}
	recall := findComparison(t, judgements[1], "recall")
	if !recall.Improved || !recall.Significant {
		t.Fatalf("유의한 개선으로 판정해야 한다: %+v", recall)
	}
	if math.Abs(recall.Difference-0.30) > 0.001 {
		t.Fatalf("개선폭이 다르다: %v", recall.Difference)
	}
}

func TestJudgeMarksNoiseAsNotSignificant(t *testing.T) {
	runs := map[string][]runMetrics{
		"baseline":  stageRuns("baseline", 0.50, 0.40, 0.60),
		"relations": stageRuns("relations", 0.52, 0.38, 0.62),
	}
	judgements := judge([]string{"baseline", "relations"}, runs)
	recall := findComparison(t, judgements[1], "recall")
	if recall.Significant {
		t.Fatalf("흩어짐이 큰 차이를 유의하다고 판정했다: %+v", recall)
	}
}

// 예산 효율은 문자 수이므로 값이 줄어야 개선이다.
func TestJudgeTreatsBudgetLowerAsBetter(t *testing.T) {
	baseline := stageRuns("baseline", 0.5, 0.5, 0.5)
	improved := stageRuns("references", 0.5, 0.5, 0.5)
	for index := range improved {
		metrics := improved[index].UseCases[useCaseFact]
		metrics.BudgetPerHit = 50
		improved[index].UseCases[useCaseFact] = metrics
	}
	judgements := judge([]string{"baseline", "references"}, map[string][]runMetrics{
		"baseline": baseline, "references": improved,
	})
	budget := findComparison(t, judgements[1], "budget_per_hit")
	if !budget.Improved || budget.Difference >= 0 {
		t.Fatalf("문자 수가 줄면 개선이어야 한다: %+v", budget)
	}
}

// 정답을 못 찾은 회차의 예산 효율은 정의되지 않으므로 표본에서 빠져야 한다.
func TestJudgeDropsUndefinedBudget(t *testing.T) {
	runs := stageRuns("baseline", 0.5, 0.5, 0.5)
	metrics := runs[0].UseCases[useCaseFact]
	metrics.BudgetPerHit = -1
	runs[0].UseCases[useCaseFact] = metrics
	judgements := judge([]string{"baseline"}, map[string][]runMetrics{"baseline": runs})
	budget := judgements[0].Estimates[useCaseFact]["budget_per_hit"]
	if budget.Repeats != 2 {
		t.Fatalf("정의되지 않은 회차가 표본에 남았다: %+v", budget)
	}
}

func findComparison(t *testing.T, judgement stageJudgement, metric string) comparison {
	t.Helper()
	for _, value := range judgement.Comparisons[useCaseFact] {
		if value.Metric == metric {
			return value
		}
	}
	t.Fatalf("지표 %q의 비교가 없다", metric)
	return comparison{}
}

// 비교 단계로 꺼둔 채널은 실패로 세지 않는다. 기준선은 그래프 채널을 끈 구성이므로
// 세면 매 질의 실패한 것으로 집계되어 실제 조회 실패와 구분할 수 없다.
func TestCountsAsFailure(t *testing.T) {
	cases := map[string]bool{
		"":                      false,
		channelDisabled:         false,
		"embedding_unavailable": true,
		"no_entry_point":        true,
	}
	for failure, want := range cases {
		if got := countsAsFailure(failure); got != want {
			t.Fatalf("countsAsFailure(%q) = %v; %v여야 한다", failure, got, want)
		}
	}
}
