package main

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"agent_context_sharing/internal/search"
)

func TestEvaluationAggregationIsDeterministic(t *testing.T) {
	values := map[string]queryOutcome{}
	signals := map[string]search.RouteSignals{}
	oracle := map[string]string{}
	for i := range 64 {
		id := fmt.Sprintf("q%02d", i)
		value := float64(i+1) / 67
		values[id] = queryOutcome{useCase: useCases[i%len(useCases)], route: string(search.RouteDirect),
			quality: []float64{value, value / 3}, chars: value * 128, latency: value / 7}
		signals[id] = search.RouteSignals{}
		oracle[id] = string(search.RouteDirect)
	}
	forced := map[string]map[string]queryOutcome{}
	for _, route := range forcedRoutes {
		forced[string(route)] = values
	}
	var first []byte
	for repeat := range 256 {
		grid := thresholdGrid([]float64{0.8, 0.7}, []float64{0.2, 0.1}, signals, forced, oracle, values)
		chosen, err := chooseThresholds(grid)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(struct {
			Grid   []thresholdResult
			Chosen thresholdPair
		}{grid, chosen}, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		if repeat == 0 {
			first = raw
			continue
		}
		if !bytes.Equal(first, raw) {
			t.Fatalf("같은 측정값의 집계가 %d회에 달라졌다", repeat)
		}
	}
}

func TestChooseThresholdsDeterministicTieBreak(t *testing.T) {
	grid := []thresholdResult{}
	for _, direct := range []float64{0.8, 0.7} {
		entry := thresholdResult{thresholdPair: thresholdPair{Direct: direct, Margin: 0.1}, Feasible: true, UseCases: map[string]thresholdUseCase{}}
		for i := range 64 {
			entry.UseCases[fmt.Sprintf("case%02d", i)] = thresholdUseCase{MisrouteRate: float64(i) / 67, Chars: 100, LatencyMS: 10}
		}
		grid = append(grid, entry)
	}
	for range 256 {
		got, err := chooseThresholds(grid)
		if err != nil || got.Direct != 0.7 {
			t.Fatalf("동점 후보 선택 = %+v, %v", got, err)
		}
	}
}

func TestEvaluationUncertaintyIsUndefinedWithOneSample(t *testing.T) {
	for _, values := range [][]float64{nil, {0.5}} {
		got := estimateOf(values)
		if got.Interval != undefinedMetric || got.StdDev != undefinedMetric {
			t.Fatalf("표본 부족의 불확실성 = %+v", got)
		}
		_, half := pairedInterval(values, values)
		if half != undefinedMetric {
			t.Fatalf("표본 부족의 대응 구간 = %v", half)
		}
		if got := summarizeMetric(values, values); got.HalfWidth != undefinedMetric {
			t.Fatalf("적대적 평가 구간 = %+v", got)
		}
	}
	if got := estimateOf([]float64{0.5, 0.5}); got.Interval != 0 || got.StdDev != 0 {
		t.Fatalf("실제 무분산 표본 = %+v", got)
	}
}

func TestCriticalValueUsesStudentTAfterThirty(t *testing.T) {
	for _, test := range []struct {
		degrees int
		want    float64
	}{
		{1, 12.7062047361747}, {2, 4.30265272974946}, {30, 2.04227245630124},
		{31, 2.03951344639641}, {60, 2.00029782201426}, {120, 1.97993040505277}, {121, 1.97976376247693},
	} {
		if got := criticalValue(test.degrees); math.Abs(got-test.want) > 1e-8 {
			t.Errorf("자유도 %d = %.12f, want %.12f", test.degrees, got, test.want)
		}
	}
}

func TestContinualJudgementOnlyComparesMeasuredMetrics(t *testing.T) {
	run := continualRun{Scenario: "online_update", StateVerified: true, JudgmentVerified: true,
		Baseline: queryMetrics{Recall: 1, ReciprocalRank: 1, BudgetPerHit: 100}, Current: queryMetrics{Recall: 1, ReciprocalRank: 1, BudgetPerHit: 100}}
	got := judgeContinual([]continualRun{run, run, run})[0]
	if len(got.Comparisons) != 3 {
		t.Fatalf("측정하지 않은 지표를 비교했다: %+v", got.Comparisons)
	}
	if judgeContinual([]continualRun{run})[0].Passed {
		t.Fatal("표본 하나로 지속 평가를 합격 처리했다")
	}
}

func TestEvaluationReportHasControlConditions(t *testing.T) {
	for _, conditions := range []any{consistencyConditions{}, adversarialConditions{}} {
		raw, err := json.Marshal(conditions)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["index_targets"]; !ok {
			t.Fatalf("색인 대상 기록 누락: %s", raw)
		}
		if _, ok := fields["fold_threshold"]; !ok {
			t.Fatalf("중복 접기 임계값 기록 누락: %s", raw)
		}
	}
}

func TestWriteReportSortsMapKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	value := map[string]int{}
	for i := range 32 {
		value[fmt.Sprintf("key%02d", i)] = i
	}
	var first []byte
	for repeat := range 32 {
		if err := writeReport(path, value); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if repeat == 0 {
			first = raw
			continue
		}
		if !bytes.Equal(first, raw) {
			t.Fatal("같은 결과의 JSON 맵 키 순서가 달라졌다")
		}
	}
}

func TestStageJudgementAndMarginalsAreDeterministic(t *testing.T) {
	values := make([]float64, 64)
	for i := range values {
		values[i] = float64(i+1) / 67
	}
	baseline := stageRuns("baseline", values...)
	current := stageRuns("references", values...)
	for i := range current {
		for j := range current[i].QuerySamples {
			current[i].QuerySamples[j].Recall /= 3
			current[i].QuerySamples[j].LatencyMS = float64(j+1) / 67
			current[i].QuerySamples[j].BudgetUsed = j
		}
	}
	stages := []string{"baseline", "references"}
	runs := map[string][]runMetrics{"baseline": baseline, "references": current}
	var first []byte
	for repeat := range 256 {
		raw, err := json.Marshal(struct {
			Judgements []stageJudgement
			Marginals  []marginal
		}{judge(stages, runs), marginals(stages, runs)}, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		if repeat == 0 {
			first = raw
		} else if !bytes.Equal(first, raw) {
			t.Fatal("단계 판정이나 한계 기여의 누적 순서가 달라졌다")
		}
	}
}

// 연분수 구현과 독립적인 t 확률밀도의 Simpson 적분으로 분위수를 대조한다.
func TestCriticalValueMatchesStudentTDensityIntegral(t *testing.T) {
	for degrees := 1; degrees <= 120; degrees++ {
		nu := float64(degrees)
		critical := criticalValue(degrees)
		lg1, _ := math.Lgamma((nu + 1) / 2)
		lg2, _ := math.Lgamma(nu / 2)
		normalization := math.Exp(lg1-lg2) / math.Sqrt(nu*math.Pi)
		pdf := func(x float64) float64 { return normalization * math.Pow(1+x*x/nu, -(nu+1)/2) }
		const intervals = 4096
		step := critical / intervals
		total := pdf(0) + pdf(critical)
		for i := 1; i < intervals; i++ {
			weight := 2.0
			if i%2 != 0 {
				weight = 4
			}
			total += weight * pdf(float64(i)*step)
		}
		if cdf := 0.5 + total*step/3; math.Abs(cdf-0.975) > 1e-9 {
			t.Errorf("자유도 %d의 밀도 적분 = %.12f", degrees, cdf)
		}
	}
	if criticalValue(0) != undefinedMetric || criticalValue(-1) != undefinedMetric {
		t.Fatal("표본이 없는 자유도를 유효한 임계값으로 처리했다")
	}
}

func TestStudentTCriticalValueDoesNotOverstateSignificance(t *testing.T) {
	before := map[string]map[string]float64{}
	after := map[string]map[string]float64{}
	for i := range 32 {
		id := fmt.Sprintf("q%02d", i)
		before[id] = map[string]float64{"recall": 0.5}
		after[id] = map[string]float64{"recall": 0.5355 + float64(2*(i%2)-1)*0.1}
	}
	result := compareStages(map[string]map[string]map[string]float64{useCaseFact: before}, map[string]map[string]map[string]float64{useCaseFact: after})[useCaseFact][0]
	if result.Significant || result.Interval <= result.Difference {
		t.Fatalf("정규 근사의 거짓 유의성이 남았다: %+v", result)
	}
}

func TestBusinessDoesNotPassUndefinedInterval(t *testing.T) {
	comparisons, passed := judgeBusiness([]businessSample{{WithoutContext: businessMetrics{CompletionTimeMS: 100}, WithContext: businessMetrics{CompletionTimeMS: 50}}})
	if passed {
		t.Fatal("표본 하나로 업무 효과 평가를 합격 처리했다")
	}
	for _, value := range comparisons {
		if value.Significant || value.Interval != undefinedMetric {
			t.Fatalf("미정의 신뢰구간을 유의하게 처리했다: %+v", value)
		}
	}
}

func TestAttackReferencesRejectDuplicateNormalizedIdentities(t *testing.T) {
	layers := map[string]string{"m1": "event", "m2": "event"}
	for _, refs := range [][]attackRef{
		{{Key: "m1"}, {Key: "m1"}},
		{{Relation: new(relationSpec{Type: "relates_to", From: "m1", To: "m2"})}, {Relation: new(relationSpec{Type: "relates_to", From: "m2", To: "m1"})}},
	} {
		if err := validateAttackRefs(refs, layers); err == nil {
			t.Fatal("정규화한 같은 식별자의 중복을 허용했다")
		}
	}
	if err := validateAttackRefs([]attackRef{{Key: "m1"}, {Key: "m2"}}, layers); err != nil {
		t.Fatalf("서로 다른 식별자를 거부했다: %v", err)
	}
}

func TestMutualAttackRejectsDuplicateContaminationRequirement(t *testing.T) {
	contexts, queries, attacks := adversarialFixture(t)
	for i := range attacks.Samples {
		if attacks.Samples[i].AttackType != attackMutual {
			continue
		}
		ref := attacks.Samples[i].SuccessCriteria[0].MustReturn[0]
		attacks.Samples[i].SuccessCriteria[0].MustReturn = []attackRef{ref, ref}
	}
	if err := validateAttackSet(attacks, contexts, queries); err == nil {
		t.Fatal("같은 오염 식별자 두 개를 서로 다른 식별자로 인정했다")
	}
}
