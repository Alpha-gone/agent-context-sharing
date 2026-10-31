package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestComparisonSingleAxis(t *testing.T) {
	stages := []string{"baseline", "references", "relations", "global", "evidence"}
	variants, err := comparisonVariants(stages, "all_layers", false)
	if err != nil || len(variants) != 5 {
		t.Fatalf("누적 비교: %v, %v", variants, err)
	}
	for i, variant := range variants {
		if variant.Name != stages[i] || variant.IndexTargets != "all_layers" || variant.EvidenceSelection != (i == 4) || variant.GlobalFallback != (i >= 3) {
			t.Fatalf("변경 축: %+v", variant)
		}
	}
	if variants[0].Stage != "baseline" || variants[4].Stage != variants[3].Stage {
		t.Fatal("비그래프 또는 근거 선택의 범위가 다르다")
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			variants, err := comparisonVariants([]string{stage}, "all_layers", true)
			if err != nil || len(variants) != 2 {
				t.Fatalf("색인 비교: %v", err)
			}
			left, right := variants[0], variants[1]
			if left.IndexTargets != "all_layers" || right.IndexTargets != "without_source" {
				t.Fatalf("대상: %v", variants)
			}
			left.Name, left.IndexTargets = right.Name, right.IndexTargets
			if left != right {
				t.Fatalf("색인 외 조건이 달라졌다: %v", variants)
			}
		})
	}
}

func TestComparisonRejectsMixedAndSkippedAxes(t *testing.T) {
	for _, test := range []struct {
		stages []string
		index  bool
	}{
		{nil, false}, {[]string{"baseline", "relations"}, false}, {[]string{"global", "relations"}, false},
		{[]string{"baseline", "baseline"}, false}, {[]string{"baseline", "references"}, true}, {[]string{"unknown"}, false},
	} {
		t.Run(fmt.Sprint(test.stages, test.index), func(t *testing.T) {
			if _, err := comparisonVariants(test.stages, "all_layers", test.index); err == nil {
				t.Fatal("잘못된 축을 수용했다")
			}
		})
	}
}

func TestControlledQueriesFreezeClockAndScope(t *testing.T) {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	input := querySet{Queries: []querySpec{{ID: "a"}, {ID: "b", Scope: "auto", AsOf: &past}}}
	output, err := controlledQueries(input, now)
	if err != nil || output.Queries[0].Scope != "auto" || !output.Queries[0].AsOf.Equal(now) || !output.Queries[1].AsOf.Equal(past) {
		t.Fatalf("조회 시각·범위: %v, %v", output, err)
	}
	if input.Queries[0].AsOf != nil || input.Queries[0].Scope != "" {
		t.Fatal("입력을 변경했다")
	}
	input.Queries[0].Scope = "global"
	if _, err := controlledQueries(input, now); err == nil {
		t.Fatal("강제 전역 입력을 수용했다")
	}
}

func TestComparisonRoundOrderAndFailure(t *testing.T) {
	variants, _ := comparisonVariants([]string{"baseline"}, "all_layers", true)
	var order []string
	runs, err := executeComparisons(t.Context(), variants, 10, func(_ context.Context, variant comparisonVariant, repeat int) (runMetrics, error) {
		order = append(order, variant.Name)
		return runMetrics{Stage: variant.Name, Repeat: repeat}, nil
	})
	if err != nil || len(runs) != 20 || !slices.Equal(order[:4], []string{"all_layers", "without_source", "without_source", "all_layers"}) {
		t.Fatalf("교차 순서: %v, %v", order, err)
	}
	marker := errors.New("measurement failure")
	for _, cancel := range []bool{false, true} {
		ctx, stop := context.WithCancel(t.Context())
		calls := 0
		failed, err := executeComparisons(ctx, variants, 10, func(context.Context, comparisonVariant, int) (runMetrics, error) {
			calls++
			if cancel {
				stop()
				return runMetrics{}, ctx.Err()
			}
			return runMetrics{}, marker
		})
		stop()
		if err == nil || failed != nil || calls != 1 || (!cancel && !errors.Is(err, marker)) {
			t.Fatalf("실패 뒤 실행: calls=%d err=%v", calls, err)
		}
	}
}

func TestComparisonAssessmentDoesNotEnableDefaults(t *testing.T) {
	for _, test := range []struct {
		name, axis, reason string
		improve, regress   bool
		count              int
		slow, failure      bool
	}{
		{name: "unchanged", axis: "graph_stage", count: 3, reason: "no_significant_improvement"},
		{name: "improved", axis: "graph_stage", count: 3, improve: true, reason: "improvement_observed_cost_and_public_acceptance_pending"},
		{name: "regressed", axis: "graph_stage", count: 3, regress: true, reason: "significant_regression"},
		{name: "one_unit", axis: "graph_stage", count: 1, improve: true, reason: "insufficient_independent_units"},
		{name: "slo", axis: "graph_stage", count: 3, improve: true, slow: true, reason: "latency_slo_exceeded"},
		{name: "failure", axis: "graph_stage", count: 3, improve: true, failure: true, reason: "channel_failures"},
		// 채널 실패·지연 초과는 독립 단위 부족보다 먼저 비채택한다.
		{name: "one_unit_slo", axis: "graph_stage", count: 1, improve: true, slow: true, reason: "latency_slo_exceeded"},
		{name: "one_unit_failure", axis: "graph_stage", count: 1, improve: true, failure: true, reason: "channel_failures"},
		{name: "one_unit_index_failure", axis: "index_targets", count: 1, failure: true, reason: "channel_failures"},
		{name: "index_equal", axis: "index_targets", count: 3, reason: "no_significant_recall_loss_final_acceptance_pending"},
		{name: "index_loss", axis: "index_targets", count: 3, regress: true, reason: "significant_recall_loss"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, after := make([]float64, test.count), make([]float64, test.count)
			for i := range before {
				before[i], after[i] = 0.5, 0.5
				if test.improve {
					after[i] = 0.8
				}
				if test.regress {
					after[i] = 0.2
				}
			}
			runs := map[string][]runMetrics{"a": stageRuns("a", before...), "b": stageRuns("b", after...)}
			for stage, rounds := range runs {
				for i := range rounds {
					for j := range rounds[i].QuerySamples {
						rounds[i].QuerySamples[j].LatencyMS = 10
					}
					metrics := rounds[i].UseCases[useCaseFact]
					metrics.LatencyP95MS = 10
					if stage == "b" && test.slow {
						metrics.LatencyP95MS = 501
					}
					if stage == "a" && test.failure {
						metrics.Failures = map[string]int{"semantic": 1}
					}
					rounds[i].UseCases[useCaseFact] = metrics
				}
			}
			assessments := assessComparisons(test.axis, judge([]string{"a", "b"}, runs), runs)
			if len(assessments) != 1 || assessments[0].Reason != test.reason || assessments[0].DefaultActivation {
				t.Fatalf("판정: %+v", assessments)
			}
		})
	}
}

func TestEmbeddingCounterCountsFailedAttempts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	counter := &embeddingCounter{next: server.Client().Transport}
	client := &http.Client{Transport: counter}
	for range 3 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if counter.calls.Load() != 3 {
		t.Fatal("HTTP 실패 시도를 누락했다")
	}
}

func TestComparisonOrderDoesNotMutateVariants(t *testing.T) {
	variants, _ := comparisonVariants([]string{"baseline", "references", "relations"}, "without_source", false)
	copy := slices.Clone(variants)
	_, _ = executeComparisons(t.Context(), variants, 10, func(_ context.Context, variant comparisonVariant, repeat int) (runMetrics, error) {
		return runMetrics{Stage: variant.Name, Repeat: repeat}, nil
	})
	if !reflect.DeepEqual(variants, copy) {
		t.Fatal("조건 순서를 변경했다")
	}
}

func TestControlledCLIRejectsInvalidAxesBeforeIO(t *testing.T) {
	for _, args := range [][]string{
		{"-compare-index-targets"},
		{"-prepared-input=missing-prepared"},
		{"-prepared-input=missing-prepared", "-adaptive"},
		{"-compare-index-targets", "-stages=baseline", "-index-targets=all_layers"},
		{"-stages=baseline,relations"},
		{"-stages=global,relations"},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			originalFlags, originalArgs := flag.CommandLine, os.Args
			flag.CommandLine = flag.NewFlagSet("eval", flag.ContinueOnError)
			os.Args = append([]string{"eval", "-contexts=missing-contexts.json", "-queries=missing-queries.json"}, args...)
			t.Cleanup(func() { flag.CommandLine, os.Args = originalFlags, originalArgs })
			if err := run(); err == nil || strings.Contains(err.Error(), "missing-") {
				t.Fatalf("입력 읽기 전 통제 검증: %v", err)
			}
		})
	}
}
