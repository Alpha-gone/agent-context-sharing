package main

import (
	"reflect"
	"testing"

	"agent_context_sharing/internal/search"
)

func outcome(useCase, route string, chars, latency float64, quality ...float64) queryOutcome {
	return queryOutcome{useCase: useCase, route: route, quality: quality, chars: chars, latency: latency}
}

// TestOracleRoutesPrefersQualityThenCost는 품질, 문자 수, 지연 순으로 최적 경로를 고르고
// 요약이 없어 직접 경로로 되돌아간 강제 전역 실행을 후보에서 빼는지 확인한다.
func TestOracleRoutesPrefersQualityThenCost(t *testing.T) {
	forced := map[string]map[string]queryOutcome{
		"direct": {
			"quality": outcome(useCaseFact, "direct", 100, 5, 0.5, 0.5),
			"chars":   outcome(useCaseFact, "direct", 120, 5, 1, 1),
			"latency": outcome(useCaseFact, "direct", 100, 9, 1, 1),
		},
		"local": {
			"quality": outcome(useCaseFact, "local", 300, 50, 1, 0.5),
			"chars":   outcome(useCaseFact, "local", 100, 50, 1, 1),
			"latency": outcome(useCaseFact, "local", 100, 8, 1, 1),
		},
		"global": {
			// 요약 부재로 직접 경로가 실행됐으므로 비용이 가장 작아도 후보가 아니다.
			"quality": outcome(useCaseFact, "direct", 1, 1, 1, 1),
			"chars":   outcome(useCaseFact, "global", 110, 5, 1, 1),
			"latency": outcome(useCaseFact, "global", 100, 10, 1, 1),
		},
	}
	got := oracleRoutes(forced)
	want := map[string]string{"quality": "local", "chars": "local", "latency": "local"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("오프라인 최적 경로 = %v, want %v", got, want)
	}
}

func TestQualityOfUsesEvidenceForAssociativeWhenDefined(t *testing.T) {
	sample := queryMetrics{UseCase: useCaseAssociative, Recall: 0.1, ReciprocalRank: 0.2, EvidenceCompleteness: 0.5, PathContinuity: 1}
	if got := qualityOf(sample); !reflect.DeepEqual(got, []float64{0.5, 1}) {
		t.Fatalf("연상 품질 = %v", got)
	}
	sample.EvidenceCompleteness = undefinedMetric
	if got := qualityOf(sample); !reflect.DeepEqual(got, []float64{0.1, 0.2}) {
		t.Fatalf("기대 근거 없는 연상 품질 = %v", got)
	}
}

// TestThresholdGridAndChoice는 격자 후보가 신호로 경로를 다시 고르고, 품질이 기존 auto보다
// 떨어지는 후보를 빼며, 남은 후보 중 오선택률이 가장 낮은 것을 고르는지 확인한다.
func TestThresholdGridAndChoice(t *testing.T) {
	// q1은 의미 1위가 0.8, 2위가 0.75라 분리 폭 0.1에서는 국소 확장, 0.02에서는 직접이다.
	signals := map[string]search.RouteSignals{
		"q1": {RelevantLocal: true, SemanticCount: 2, SemanticTop: 0.8, SemanticSecond: 0.75},
		"q2": {RelevantLocal: true, SemanticCount: 2, SemanticTop: 0.9, SemanticSecond: 0.1},
	}
	forced := map[string]map[string]queryOutcome{
		"direct": {"q1": outcome(useCaseFact, "direct", 100, 5, 1, 1), "q2": outcome(useCaseFact, "direct", 100, 5, 1, 1)},
		"local":  {"q1": outcome(useCaseFact, "local", 150, 9, 1, 1), "q2": outcome(useCaseFact, "local", 100, 9, 0, 0)},
		"global": {"q1": outcome(useCaseFact, "direct", 100, 5, 1, 1), "q2": outcome(useCaseFact, "direct", 100, 5, 1, 1)},
	}
	oracle := oracleRoutes(forced)
	legacy := map[string]queryOutcome{"q1": outcome(useCaseFact, "local", 150, 9, 1, 1), "q2": outcome(useCaseFact, "local", 100, 9, 1, 1)}
	grid := thresholdGrid([]float64{0.95, 0.7}, []float64{0.1, 0.02}, signals, forced, oracle, legacy)
	if len(grid) != 4 {
		t.Fatalf("격자 크기 = %d", len(grid))
	}
	// 하한 0.95는 q2를 국소 확장으로 보내 재현율 0이 되므로 품질 비악화를 어긴다.
	for _, entry := range grid {
		if entry.Direct == 0.95 && entry.Feasible {
			t.Fatalf("품질이 떨어진 후보를 허용했다: %+v", entry)
		}
	}
	chosen, err := chooseThresholds(grid)
	if err != nil || chosen != (thresholdPair{Direct: 0.7, Margin: 0.02}) {
		t.Fatalf("고른 임계값 = %+v, %v", chosen, err)
	}
	for _, entry := range grid {
		if entry.thresholdPair == chosen && entry.UseCases[useCaseFact].MisrouteRate != 0 {
			t.Fatalf("고른 후보의 오선택률 = %v", entry.UseCases[useCaseFact].MisrouteRate)
		}
	}
	if _, err := chooseThresholds([]thresholdResult{{Feasible: false}}); err == nil {
		t.Fatal("품질을 유지하는 후보가 없을 때 오류를 내지 않았다")
	}
}

func TestMarkMisroutes(t *testing.T) {
	runs := []runMetrics{{UseCases: map[string]useCaseMetrics{useCaseFact: {}}, QuerySamples: []queryMetrics{
		{ID: "q1", UseCase: useCaseFact, Route: "direct"},
		{ID: "q2", UseCase: useCaseFact, Route: "local"},
	}}}
	markMisroutes(runs, map[string]string{"q1": "direct", "q2": "global"})
	if runs[0].QuerySamples[0].Misrouted != 0 || runs[0].QuerySamples[1].Misrouted != 1 || runs[0].UseCases[useCaseFact].MisrouteRate != 0.5 {
		t.Fatalf("오선택 표시 = %+v", runs[0])
	}
}

func TestParseFloats(t *testing.T) {
	values, err := parseFloats("0.5, 0.7,", "하한")
	if err != nil || !reflect.DeepEqual(values, []float64{0.5, 0.7}) {
		t.Fatalf("후보 해석 = %v, %v", values, err)
	}
	for _, raw := range []string{"", "1.5", "abc"} {
		if _, err := parseFloats(raw, "하한"); err == nil {
			t.Errorf("%q를 받아들였다", raw)
		}
	}
}

// TestUseCaseQualitySkipsUndefinedValues는 정의되지 않은 경로 연속성이 평균을 끌어내리지
// 않고, 기준에서 정의된 지표가 후보에서 떨어지면 악화로 보는지 확인한다.
func TestUseCaseQualitySkipsUndefinedValues(t *testing.T) {
	got := useCaseQuality(map[string]queryOutcome{
		"q1": outcome(useCaseAssociative, "local", 0, 0, 0.5, 1),
		"q2": outcome(useCaseAssociative, "local", 0, 0, 0.3, undefinedMetric),
		"q3": outcome(useCaseAssociative, "local", 0, 0, 0.4, undefinedMetric),
	})
	if want := []float64{0.4, 1}; !qualityNotWorse(got[useCaseAssociative], want) || !qualityNotWorse(want, got[useCaseAssociative]) {
		t.Fatalf("품질 평균 = %v, want %v", got[useCaseAssociative], want)
	}
	if qualityNotWorse([]float64{0.4, 0}, []float64{0.4, 1}) {
		t.Fatal("경로 연속성 하락을 악화로 보지 않았다")
	}
	if !qualityNotWorse([]float64{0.4, undefinedMetric}, []float64{0.4, undefinedMetric}) {
		t.Fatal("양쪽 모두 정의되지 않은 지표를 악화로 봤다")
	}
}
