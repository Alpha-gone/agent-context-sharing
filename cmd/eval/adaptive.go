// 「질의 적응형 검색 비교」를 실행한다. 같은 질의에 직접·국소 확장·전역 진입을 모두 실행해
// 오프라인 최적 경로를 정하고, 기존 auto 규칙과 적응형 라우터의 경로 오선택률과 품질·예산·
// 지연을 같은 입력에서 견준다.
package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

// 비교 구성 이름이다. 강제 경로 셋은 오프라인 최적 경로를 정하는 데만 쓴다.
const (
	variantAuto     = "auto"
	variantAdaptive = "adaptive"
	variantPrefix   = "route-"
)

var forcedRoutes = []search.Route{search.RouteDirect, search.RouteLocal, search.RouteGlobal}

// adaptiveReport는 적응형 비교의 결과 파일이다. 질의문·본문·경로 원문은 싣지 않는다.
type adaptiveReport struct {
	StartedAt      time.Time                 `json:"started_at"`
	ContextVersion string                    `json:"context_dataset_version"`
	QueryVersion   string                    `json:"query_dataset_version"`
	Conditions     adaptiveConditions        `json:"conditions"`
	Oracle         map[string]map[string]int `json:"offline_optimal_routes"`
	LegacyQuality  map[string][]float64      `json:"auto_quality"`
	Grid           []thresholdResult         `json:"threshold_grid"`
	// Chosen은 품질을 유지하는 임계값 후보가 없으면 비어 있고 적응형 구성을 실행하지 않는다.
	Chosen     *thresholdPair   `json:"chosen_thresholds"`
	Runs       []runMetrics     `json:"runs"`
	Judgements []stageJudgement `json:"judgements"`
}

// adaptiveConditions는 두 구성이 공유하는 통제 조건이다.
type adaptiveConditions struct {
	Repeats               int     `json:"repeats"`
	Budget                int     `json:"budget"`
	MaxHops               int     `json:"max_hops"`
	MaxHopNodes           int     `json:"max_hop_nodes"`
	GraphStage            string  `json:"graph_stage"`
	EvidencePathSelection bool    `json:"evidence_path_selection"`
	Execution             string  `json:"channel_execution"`
	CandidateLimit        int     `json:"channel_candidate_limit"`
	SemanticThreshold     float64 `json:"semantic_similarity_threshold"`
	FoldThreshold         float64 `json:"fold_threshold"`
	EmbeddingModel        string  `json:"embedding_model"`
	IndexTargets          string  `json:"index_targets"`
}

type thresholdPair struct {
	Direct float64 `json:"direct_similarity"`
	Margin float64 `json:"direct_margin"`
}

// thresholdResult는 임계값 후보 하나를 강제 경로 결과로 재구성한 사용 사례별 집계다.
// Feasible은 모든 사용 사례의 품질 평균이 기존 auto보다 낮지 않다는 뜻이다.
type thresholdResult struct {
	thresholdPair
	Feasible bool                        `json:"quality_not_worse"`
	UseCases map[string]thresholdUseCase `json:"use_cases"`
}

type thresholdUseCase struct {
	Quality      []float64          `json:"quality"`
	Chars        float64            `json:"chars_per_query"`
	LatencyMS    float64            `json:"latency_ms"`
	MisrouteRate float64            `json:"misroute_rate"`
	Routes       map[string]float64 `json:"route_ratio"`
	Reasons      map[string]int     `json:"route_reasons"`
}

// queryOutcome은 한 구성에서 질의 하나의 반복 평균이다.
type queryOutcome struct {
	useCase string
	route   string
	quality []float64
	chars   float64
	latency float64
}

// parseFloats는 쉼표로 나눈 임계값 후보를 읽는다.
func parseFloats(raw, name string) ([]float64, error) {
	values := []float64{}
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		value, err := strconv.ParseFloat(part, 64)
		if err != nil || value < 0 || value > 1 {
			return nil, fmt.Errorf("%s 후보 %q가 0 이상 1 이하의 수가 아니다", name, part)
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s 후보가 없다", name)
	}
	return values, nil
}

// runAdaptive는 기존 auto, 강제 경로 셋, 적응형 구성을 같은 그래프에서 반복 측정한다.
// 적응형 구성의 임계값은 강제 경로 결과로 격자를 먼저 견준 뒤 고른 값이다.
func runAdaptive(ctx context.Context, database *store.Store, worker search.Embedder, graph loadedGraph, contexts contextSet, queries querySet, loaded settings, conditions adaptiveConditions, directs, margins []float64) (adaptiveReport, error) {
	base := search.Config{
		Execution:             loaded.execution,
		CandidateLimit:        loaded.candidateLimit,
		SemanticThreshold:     loaded.semanticThreshold,
		FoldThreshold:         loaded.foldThreshold,
		GraphStage:            search.GraphStage(conditions.GraphStage),
		GlobalFallback:        true,
		EvidencePathSelection: conditions.EvidencePathSelection,
	}
	result := adaptiveReport{StartedAt: time.Now().UTC(), ContextVersion: contexts.Version, QueryVersion: queries.Version, Conditions: conditions}
	runs := map[string][]runMetrics{}
	signals := map[string]search.RouteSignals{}
	measureVariant := func(name string, config search.Config) error {
		service, err := search.New(database, worker, config, slog.Default())
		if err != nil {
			return fmt.Errorf("구성 %q 검색 실행기 준비: %w", name, err)
		}
		for repeat := 1; repeat <= conditions.Repeats; repeat++ {
			started := time.Now()
			measured, err := measure(ctx, service, graph, queries, conditions.Budget, conditions.MaxHops, conditions.MaxHopNodes)
			if err != nil {
				return fmt.Errorf("구성 %q 회차 %d: %w", name, repeat, err)
			}
			if strings.HasPrefix(name, variantPrefix) {
				for id, value := range measured.Signals {
					signals[id] = value
				}
			}
			runs[name] = append(runs[name], runMetrics{Stage: name, Repeat: repeat, DurationMS: time.Since(started).Milliseconds(), UseCases: measured.UseCases, QuerySamples: measured.QuerySamples, recovered: measured.Recovered})
			slog.Info("회차 완료", "variant", name, "repeat", repeat, "duration", time.Since(started).String())
		}
		return nil
	}
	if err := measureVariant(variantAuto, base); err != nil {
		return adaptiveReport{}, err
	}
	for _, route := range forcedRoutes {
		config := base
		config.AdaptiveRouting, config.Route = true, route
		config.AdaptiveDirectThreshold, config.AdaptiveMarginThreshold = directs[0], margins[0]
		if err := measureVariant(variantPrefix+string(route), config); err != nil {
			return adaptiveReport{}, err
		}
	}

	forced := map[string]map[string]queryOutcome{}
	for _, route := range forcedRoutes {
		forced[string(route)] = outcomes(runs[variantPrefix+string(route)], queries)
	}
	oracle := oracleRoutes(forced)
	result.Oracle = oracleDistribution(oracle, forced)
	legacy := outcomes(runs[variantAuto], queries)
	result.LegacyQuality = useCaseQuality(legacy)
	result.Grid = thresholdGrid(directs, margins, signals, forced, oracle, legacy)
	chosen, err := chooseThresholds(result.Grid)
	if err != nil {
		// 품질 비악화를 만족하는 후보가 없으면 승격 조건을 넘을 수 없다. 판단 근거가 되는
		// 격자와 강제 경로 결과를 남기고 기존 auto만 오선택률을 채워 끝낸다.
		slog.Warn("적응형 구성을 실행하지 않는다", "reason", err.Error())
		markMisroutes(runs[variantAuto], oracle)
		for _, name := range []string{variantAuto, variantPrefix + "direct", variantPrefix + "local", variantPrefix + "global"} {
			result.Runs = append(result.Runs, runs[name]...)
		}
		return result, nil
	}
	result.Chosen = &chosen

	config := base
	config.AdaptiveRouting = true
	config.AdaptiveDirectThreshold, config.AdaptiveMarginThreshold = chosen.Direct, chosen.Margin
	if err := measureVariant(variantAdaptive, config); err != nil {
		return adaptiveReport{}, err
	}
	for _, name := range []string{variantAuto, variantAdaptive} {
		markMisroutes(runs[name], oracle)
	}
	for _, name := range append([]string{variantAuto, variantAdaptive}, variantPrefix+"direct", variantPrefix+"local", variantPrefix+"global") {
		result.Runs = append(result.Runs, runs[name]...)
	}
	result.Judgements = judge([]string{variantAuto, variantAdaptive}, runs)
	return result, nil
}

// qualityOf는 오프라인 최적 경로를 정하는 품질 지표를 우선순위대로 낸다. 사실 검색은
// 재현율과 순위 품질을, 연상 검색과 전역 요약은 근거 완전성과 경로 연속성을 쓴다. 기대
// 근거가 없어 근거 완전성이 정의되지 않은 질의는 사실 검색과 같은 지표를 쓴다.
func qualityOf(sample queryMetrics) []float64 {
	if sample.UseCase == useCaseFact || sample.EvidenceCompleteness < 0 {
		return []float64{sample.Recall, sample.ReciprocalRank}
	}
	return []float64{sample.EvidenceCompleteness, sample.PathContinuity}
}

// outcomes는 반복 회차를 질의별로 평균낸다. 경로는 검색이 결정적이므로 첫 회차 값을 쓴다.
func outcomes(runs []runMetrics, queries querySet) map[string]queryOutcome {
	result := make(map[string]queryOutcome, len(queries.Queries))
	for _, run := range runs {
		for _, sample := range run.QuerySamples {
			current, present := result[sample.ID]
			if !present {
				current = queryOutcome{useCase: sample.UseCase, route: sample.Route, quality: make([]float64, len(qualityOf(sample)))}
			}
			for index, value := range qualityOf(sample) {
				current.quality[index] += value / float64(len(runs))
			}
			current.chars += float64(sample.BudgetUsed) / float64(len(runs))
			current.latency += sample.LatencyMS / float64(len(runs))
			result[sample.ID] = current
		}
	}
	return result
}

// compareOutcome은 품질이 높고 문자 수와 지연이 작은 쪽을 앞에 둔다.
func compareOutcome(left, right queryOutcome) int {
	if order := slices.Compare(right.quality, left.quality); order != 0 {
		return order
	}
	if order := cmp.Compare(left.chars, right.chars); order != 0 {
		return order
	}
	return cmp.Compare(left.latency, right.latency)
}

// oracleRoutes는 질의별 오프라인 최적 경로를 정한다. 전역 요약이 없어 직접 경로로 되돌아간
// 강제 전역 실행은 직접 경로와 같은 실행이므로 후보에서 뺀다.
func oracleRoutes(forced map[string]map[string]queryOutcome) map[string]string {
	result := map[string]string{}
	for id := range forced[string(search.RouteDirect)] {
		best, bestRoute := queryOutcome{}, ""
		for _, route := range forcedRoutes {
			outcome, present := forced[string(route)][id]
			if !present || outcome.route != string(route) {
				continue
			}
			if bestRoute == "" || compareOutcome(outcome, best) < 0 {
				best, bestRoute = outcome, string(route)
			}
		}
		result[id] = bestRoute
	}
	return result
}

func oracleDistribution(oracle map[string]string, forced map[string]map[string]queryOutcome) map[string]map[string]int {
	result := map[string]map[string]int{}
	for id, route := range oracle {
		useCase := forced[string(search.RouteDirect)][id].useCase
		if result[useCase] == nil {
			result[useCase] = map[string]int{}
		}
		result[useCase][route]++
	}
	return result
}

// thresholdGrid는 임계값 후보마다 라우터가 고를 경로를 신호로 다시 계산하고 그 경로의 강제
// 실행 결과로 집계한다. 라우터 입력이 진입 채널 결과뿐이라 같은 신호의 경로 실행은 강제
// 실행과 같으므로 후보마다 검색을 다시 돌리지 않는다.
func thresholdGrid(directs, margins []float64, signals map[string]search.RouteSignals, forced map[string]map[string]queryOutcome, oracle map[string]string, legacy map[string]queryOutcome) []thresholdResult {
	legacyQuality := useCaseQuality(legacy)
	grid := make([]thresholdResult, 0, len(directs)*len(margins))
	for _, direct := range directs {
		for _, margin := range margins {
			chosen := map[string]queryOutcome{}
			misrouted := map[string]float64{}
			reasons := map[string]string{}
			for id, value := range signals {
				route, reason := value.Decide(direct, margin)
				reasons[id] = reason
				outcome := forced[string(route)][id]
				chosen[id] = outcome
				if outcome.route != oracle[id] {
					misrouted[id] = 1
				}
			}
			entry := thresholdResult{thresholdPair: thresholdPair{Direct: direct, Margin: margin}, Feasible: true, UseCases: map[string]thresholdUseCase{}}
			quality := useCaseQuality(chosen)
			for useCase, values := range quality {
				summary := thresholdUseCase{Quality: values, Routes: map[string]float64{}, Reasons: map[string]int{}}
				count := 0.0
				for id, outcome := range chosen {
					if outcome.useCase != useCase {
						continue
					}
					count++
					summary.Chars += outcome.chars
					summary.LatencyMS += outcome.latency
					summary.MisrouteRate += misrouted[id]
					summary.Routes[outcome.route]++
					summary.Reasons[reasons[id]]++
				}
				summary.Chars /= count
				summary.LatencyMS /= count
				summary.MisrouteRate /= count
				for route := range summary.Routes {
					summary.Routes[route] /= count
				}
				entry.UseCases[useCase] = summary
				if !qualityNotWorse(values, legacyQuality[useCase]) {
					entry.Feasible = false
				}
			}
			grid = append(grid, entry)
		}
	}
	return grid
}

// qualityNotWorse는 모든 품질 지표가 기준 이상인지 본다. 반복 평균의 부동소수 오차는
// 같은 값으로 본다. 기준에서 정의된 지표가 후보에서 정의되지 않으면 악화로 본다.
func qualityNotWorse(values, baseline []float64) bool {
	for index, value := range values {
		if baseline[index] < 0 {
			continue
		}
		if value < baseline[index]-1e-9 {
			return false
		}
	}
	return true
}

// useCaseQuality는 사용 사례별 품질 지표 평균이다. 품질이 비악화인지 견주는 기준이다.
// 대표 후보가 반환되지 않아 정의되지 않은 경로 연속성처럼 음수 표식인 값은 평균에서 빼고,
// 한 질의에서도 정의되지 않으면 그 지표는 정의되지 않은 값이다.
func useCaseQuality(values map[string]queryOutcome) map[string][]float64 {
	totals := map[string][]float64{}
	counts := map[string][]float64{}
	for _, outcome := range values {
		if totals[outcome.useCase] == nil {
			totals[outcome.useCase] = make([]float64, len(outcome.quality))
			counts[outcome.useCase] = make([]float64, len(outcome.quality))
		}
		for index, value := range outcome.quality {
			if value < 0 {
				continue
			}
			totals[outcome.useCase][index] += value
			counts[outcome.useCase][index]++
		}
	}
	for useCase, values := range totals {
		for index := range values {
			if counts[useCase][index] == 0 {
				values[index] = undefinedMetric
				continue
			}
			values[index] /= counts[useCase][index]
		}
	}
	return totals
}

// chooseThresholds는 「검색 품질 평가」가 정한 대로 사용 사례별 품질을 유지하는 후보 중
// 경로 오선택률이 가장 낮은 것을 고르고, 같으면 문자 수, 지연, 더 작은 하한과 분리 폭 순으로
// 정한다. 사용 사례별 값은 질의 수로 가중하지 않고 평균한다.
func chooseThresholds(grid []thresholdResult) (thresholdPair, error) {
	type score struct {
		pair                     thresholdPair
		misroute, chars, latency float64
	}
	scores := []score{}
	for _, entry := range grid {
		if !entry.Feasible {
			continue
		}
		current := score{pair: entry.thresholdPair}
		for _, summary := range entry.UseCases {
			current.misroute += summary.MisrouteRate / float64(len(entry.UseCases))
			current.chars += summary.Chars / float64(len(entry.UseCases))
			current.latency += summary.LatencyMS / float64(len(entry.UseCases))
		}
		scores = append(scores, current)
	}
	if len(scores) == 0 {
		return thresholdPair{}, fmt.Errorf("품질을 유지하는 임계값 후보가 없다")
	}
	best := slices.MinFunc(scores, func(left, right score) int {
		return cmp.Or(
			cmp.Compare(left.misroute, right.misroute),
			cmp.Compare(left.chars, right.chars),
			cmp.Compare(left.latency, right.latency),
			cmp.Compare(left.pair.Direct, right.pair.Direct),
			cmp.Compare(left.pair.Margin, right.pair.Margin),
		)
	})
	return best.pair, nil
}

// markMisroutes는 구성이 실제로 실행한 경로를 오프라인 최적 경로와 대조해 질의 표본과
// 사용 사례 집계에 오선택 여부를 채운다.
func markMisroutes(runs []runMetrics, oracle map[string]string) {
	for runIndex := range runs {
		samplesByUseCase := map[string][]queryMetrics{}
		for sampleIndex := range runs[runIndex].QuerySamples {
			sample := &runs[runIndex].QuerySamples[sampleIndex]
			sample.Misrouted = 0
			if sample.Route != oracle[sample.ID] {
				sample.Misrouted = 1
			}
			samplesByUseCase[sample.UseCase] = append(samplesByUseCase[sample.UseCase], *sample)
		}
		for useCase, samples := range samplesByUseCase {
			metrics := runs[runIndex].UseCases[useCase]
			metrics.MisrouteRate = definedMean(samples, func(sample queryMetrics) float64 { return sample.Misrouted })
			runs[runIndex].UseCases[useCase] = metrics
		}
	}
}
