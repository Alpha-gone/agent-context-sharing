package main

import (
	"bytes"
	json "encoding/json/v2"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

func groupedStageRuns() map[string][]runMetrics {
	runs := map[string][]runMetrics{"baseline": stageRuns("baseline", 0, 0, 0), "references": stageRuns("references", .2, .4, .4)}
	// 재현율은 음수가 될 수 없으므로 차이 -0.1은 .5→.4로 만든다.
	for stage, values := range runs {
		for r := range values {
			for q := range values[r].QuerySamples {
				values[r].QuerySamples[q].IndependentUnit = "conversation-a"
				if q == 2 {
					values[r].QuerySamples[q].IndependentUnit = "conversation-b"
					values[r].QuerySamples[q].Recall = .5
					if stage == "references" {
						values[r].QuerySamples[q].Recall = .4
					}
				}
			}
		}
	}
	return runs
}

func TestIndependentUnitStatistics(t *testing.T) {
	runs := groupedStageRuns()
	stages := []string{"baseline", "references"}
	if err := validateMeasuredRuns(stages, runs); err != nil {
		t.Fatal(err)
	}
	got := judge(stages, runs)
	comparison := findComparison(t, got[1], "recall")
	if comparison.Samples != 2 || comparison.DegreesFreedom != 1 || math.Abs(comparison.Difference-.1) > 1e-12 || math.Abs(comparison.Interval-2.54124094723494) > 1e-10 || comparison.Significant {
		t.Fatalf("독립 대화의 대응 통계: %+v", comparison)
	}
	estimate := got[1].Estimates[useCaseFact]["recall"]
	if estimate.Samples != 2 || math.Abs(estimate.Mean-.35) > 1e-12 {
		t.Fatalf("대화별 동일 가중 집계: %+v", estimate)
	}
	for stage := range runs {
		for r := range runs[stage] {
			for q := range runs[stage][r].QuerySamples {
				runs[stage][r].QuerySamples[q].IndependentUnit = "one-conversation"
			}
		}
	}
	one := findComparison(t, judge(stages, runs)[1], "recall")
	if one.Samples != 1 || one.Interval != -1 || one.Significant || one.Status != "insufficient_units" {
		t.Fatalf("한 대화의 여러 문항과 반복을 부풀렸다: %+v", one)
	}
}

func TestIndependentMetricPairsUseSameQuestions(t *testing.T) {
	runs := groupedStageRuns()
	runs["baseline"][0].QuerySamples[0].Recall = -1
	got := findComparison(t, judge([]string{"baseline", "references"}, runs)[1], "recall")
	if got.Samples != 2 || math.Abs(got.Difference-.15) > 1e-12 {
		t.Fatalf("양쪽의 다른 문항으로 대응 평균을 만들었다: %+v", got)
	}
	for stage := range runs {
		for r := range runs[stage] {
			for q := range runs[stage][r].QuerySamples {
				runs[stage][r].QuerySamples[q].Recall = -1
			}
		}
	}
	missing := findComparison(t, judge([]string{"baseline", "references"}, runs)[1], "recall")
	if missing.Samples != 0 || missing.Baseline != -1 || missing.Current != -1 || missing.Difference != -1 || missing.Interval != -1 || missing.Significant || missing.Status != "unmeasured" {
		t.Fatalf("미측정 비교: %+v", missing)
	}
}

func TestMeasuredRunsRejectBrokenPairs(t *testing.T) {
	cases := map[string]func(map[string][]runMetrics){
		"missing_round":   func(r map[string][]runMetrics) { r["references"] = r["references"][:9] },
		"duplicate_round": func(r map[string][]runMetrics) { r["references"][1].Repeat = 1 },
		"round_range":     func(r map[string][]runMetrics) { r["references"][1].Repeat = 11 },
		"wrong_stage":     func(r map[string][]runMetrics) { r["references"][1].Stage = "baseline" },
		"missing_query":   func(r map[string][]runMetrics) { r["references"][1].QuerySamples = r["references"][1].QuerySamples[:2] },
		"duplicate_query": func(r map[string][]runMetrics) { r["references"][1].QuerySamples[1].ID = "q0" },
		"changed_unit":    func(r map[string][]runMetrics) { r["references"][1].QuerySamples[1].IndependentUnit = "another" },
		"changed_case":    func(r map[string][]runMetrics) { r["references"][1].QuerySamples[1].UseCase = useCaseGlobal },
		"nan":             func(r map[string][]runMetrics) { r["references"][1].QuerySamples[1].Recall = math.NaN() },
		"infinity":        func(r map[string][]runMetrics) { r["references"][1].QuerySamples[1].Recall = math.Inf(1) },
		"negative":        func(r map[string][]runMetrics) { r["references"][1].QuerySamples[1].Recall = -2 },
		"private_unit": func(r map[string][]runMetrics) {
			r["references"][1].QuerySamples[1].IndependentUnit = "https://private"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := groupedStageRuns()
			change(r)
			if err := validateMeasuredRuns([]string{"baseline", "references"}, r); err == nil {
				t.Fatal("불완전한 대응 집계를 허용했다")
			}
		})
	}
	if validateMeasuredRuns(nil, nil) == nil {
		t.Fatal("빈 단계를 허용했다")
	}
}

func TestAggregationIgnoresInputOrder(t *testing.T) {
	runs := groupedStageRuns()
	stages := []string{"baseline", "references"}
	for stage := range runs {
		for r := range runs[stage] {
			for q := range runs[stage][r].QuerySamples {
				runs[stage][r].QuerySamples[q].LatencyMS = float64((r+1)*(q+1)) / 67
			}
		}
	}
	serialize := func() []byte {
		t.Helper()
		raw, err := json.Marshal(struct {
			Judgements []stageJudgement
			Marginals  []marginal
		}{judge(stages, runs), marginals(stages, runs)}, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	first := serialize()
	firstOutcomes := outcomes(runs["references"], querySet{})
	random := rand.New(rand.NewPCG(42, 17))
	for range 64 {
		for stage := range runs {
			random.Shuffle(len(runs[stage]), func(i, j int) { runs[stage][i], runs[stage][j] = runs[stage][j], runs[stage][i] })
			for r := range runs[stage] {
				random.Shuffle(len(runs[stage][r].QuerySamples), func(i, j int) {
					runs[stage][r].QuerySamples[i], runs[stage][r].QuerySamples[j] = runs[stage][r].QuerySamples[j], runs[stage][r].QuerySamples[i]
				})
			}
		}
		if !reflect.DeepEqual(firstOutcomes, outcomes(runs["references"], querySet{})) {
			t.Fatal("적응형 질의 평균이 순서에 따라 달라졌다")
		}
		if !bytes.Equal(first, serialize()) {
			t.Fatal("회차·질의 순서만 바꿔 집계가 달라졌다")
		}
	}
}

func TestStatisticsKnownPairsAndMissing(t *testing.T) {
	difference, interval := pairedInterval([]float64{1, 2, 3}, []float64{2, 4, 6})
	if difference != 2 || math.Abs(interval-2.484137711719546) > 1e-10 {
		t.Fatalf("알려진 대응 구간: %v %v", difference, interval)
	}
	for _, pair := range [][2][]float64{{nil, nil}, {{1}, {1, 2}}, {{1}, {math.NaN()}}, {{-1}, {1}}} {
		if a, b := pairedInterval(pair[0], pair[1]); a != -1 || b != -1 {
			t.Fatalf("무효 대응: %v %v", a, b)
		}
	}
	if got := estimateOf(nil); got.Mean != -1 || got.Interval != -1 || got.Status != "unmeasured" {
		t.Fatalf("빈 표본: %+v", got)
	}
	if got := estimateOf([]float64{0}); got.Mean != 0 || got.StdDev != -1 || got.Status != "insufficient_units" {
		t.Fatalf("측정 0: %+v", got)
	}
	if got := latencyP95([]queryMetrics{{LatencyMS: -1}, {LatencyMS: math.NaN()}}); got != -1 {
		t.Fatal(got)
	}
}

func TestMarginalsAverageCorrespondingRecoveryRounds(t *testing.T) {
	set := func(keys ...string) map[string]struct{} {
		result := map[string]struct{}{}
		for _, k := range keys {
			result[k] = struct{}{}
		}
		return result
	}
	makeRun := func(stage string, r int, keys ...string) runMetrics {
		return runMetrics{Stage: stage, Repeat: r, QuerySamples: []queryMetrics{{ID: "q", UseCase: useCaseFact, LatencyMS: float64(r), BudgetUsed: 10}}, recovered: map[string]map[string]struct{}{"q": set(keys...)}}
	}
	runs := map[string][]runMetrics{"baseline": {makeRun("baseline", 1, "a"), makeRun("baseline", 2, "b")}, "references": {makeRun("references", 2, "b"), makeRun("references", 1, "a", "c")}}
	got := marginals([]string{"baseline", "references"}, runs)[0]
	if got.Recovered != .5 || got.Lost != 0 {
		t.Fatalf("첫 회차만의 회수 수를 보고했다: %+v", got)
	}
	slices.Reverse(runs["baseline"])
	if other := marginals([]string{"baseline", "references"}, runs)[0]; other != got {
		t.Fatalf("회차 순서에 의존한다: %+v %+v", got, other)
	}
}

func TestEvidenceDenominatorsAndReturnedSubgraph(t *testing.T) {
	a, b, c, x := model.ID{6: 0x70, 8: 0x80, 15: 1}, model.ID{6: 0x70, 8: 0x80, 15: 2}, model.ID{6: 0x70, 8: 0x80, 15: 3}, model.ID{6: 0x70, 8: 0x80, 15: 4}
	keys := map[string]model.ID{"a": a, "b": b, "c": c}
	returned := map[model.ID]struct{}{a: {}, b: {}, c: {}}
	complete, continuous := evidenceScore(querySpec{Required: []string{"a", "a", "c"}, Paths: [][]string{{"a", "b", "c"}, {"a", "b"}}}, keys, returned, map[[2]model.ID]struct{}{pairOf(a, b): {}})
	if complete != .75 || continuous != .5 {
		t.Fatalf("겹친 필수 근거 분모: %v %v", complete, continuous)
	}
	flow := search.Flow{Contexts: []search.Context{structureContext(a, model.LayerEvent, 0, "keyword"), structureContext(b, model.LayerEvent, 0, "graph")}, Edges: []store.HopEdge{{FromID: a, ToID: x}, {FromID: x, ToID: b}}}
	if disconnectedRatio(flow) != 1 {
		t.Fatal("반환되지 않은 노드로 단절을 연결했다")
	}
}

func TestContinualRepeatsAreNotIndependentTasks(t *testing.T) {
	var runs []continualRun
	for r := range initialEvaluationRepeats {
		runs = append(runs, continualRun{Scenario: "online_update", Repeat: r + 1, StateVerified: true, JudgmentVerified: true, Baseline: queryMetrics{Recall: 1, ReciprocalRank: 1, BudgetPerHit: 100}, Current: queryMetrics{Recall: 1, ReciprocalRank: 1, BudgetPerHit: 100}})
	}
	got := judgeContinual(runs)[0]
	if got.Passed || got.Comparisons[0].Samples != 1 || got.Comparisons[0].Interval != -1 {
		t.Fatalf("지속 반복을 독립 과업으로 부풀렸다: %+v", got)
	}
}

func TestQueryIndependentUnitInput(t *testing.T) {
	contexts, err := loadContextSet(writeFile(t, "contexts.json", validContexts))
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{"conversation-001", "https://private"} {
		body := `{"version":"q1","queries":[{"id":"q","independent_unit":"` + unit + `","use_case":"fact","work_context":"합성 질의","answers":["s1"]}]}`
		queries, err := loadQuerySet(writeFile(t, "queries.json", body), contexts)
		if unit == "conversation-001" {
			if err != nil || queries.Queries[0].IndependentUnit != unit {
				t.Fatalf("독립 단위 입력 유실: %+v %v", queries, err)
			}
		} else if err == nil {
			t.Fatal("원문 주소를 독립 단위로 허용했다")
		}
	}
}

func TestHubConcentrationAvoidsIntegerOverflow(t *testing.T) {
	flow := search.Flow{Contexts: []search.Context{{CandidateDegree: 1 << 60}, {CandidateDegree: 0}}}
	if got := hubConcentration(flow); got != .5 {
		t.Fatalf("큰 차수의 지니 계수: %v", got)
	}
}

func TestMarginalLatencyDistinguishesMissingFromNegativeDifference(t *testing.T) {
	before := stageRuns("baseline", 1)
	after := stageRuns("references", 1)
	for i := range before {
		before[i].QuerySamples[0].LatencyMS = 2
		after[i].QuerySamples[0].LatencyMS = 1
	}
	runs := map[string][]runMetrics{"baseline": before, "references": after}
	measured := marginals([]string{"baseline", "references"}, runs)[0]
	if measured.AddedLatencyMS != -1 || measured.LatencyQueries != 1 {
		t.Fatalf("실제 -1ms 차이: %+v", measured)
	}
	after[0].QuerySamples[0].LatencyMS = -1
	missing := marginals([]string{"baseline", "references"}, runs)[0]
	if missing.AddedLatencyMS != -1 || missing.LatencyQueries != 0 {
		t.Fatalf("미측정 차이: %+v", missing)
	}
}
