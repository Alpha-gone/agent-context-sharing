package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

type comparisonVariant struct {
	Name              string `json:"name"`
	Stage             string `json:"graph_stage"`
	IndexTargets      string `json:"index_targets"`
	GlobalFallback    bool   `json:"global_fallback"`
	EvidenceSelection bool   `json:"evidence_path_selection"`
}

type preparationMetrics struct {
	DurationMS     float64 `json:"duration_ms"`
	EmbeddingCalls int64   `json:"document_embedding_http_attempts"`
	VectorRows     int64   `json:"vector_rows"`
	VectorBytes    int64   `json:"vector_column_bytes"`
}

type controlledOptions struct {
	Stages                                []string
	IndexTargets                          string
	CompareIndex                          bool
	Repeats, Budget, MaxHops, MaxHopNodes int
}

// comparisonVariants는 실행마다 비교 축 하나만 바꾸며 중간 단계 생략을 거부한다.
func comparisonVariants(stages []string, targets string, compareIndex bool) ([]comparisonVariant, error) {
	known := []string{"baseline", "references", "relations", "global", stageEvidence}
	if len(stages) == 0 || !slices.Contains([]string{"all_layers", "without_source"}, targets) {
		return nil, fmt.Errorf("비교 단계 또는 색인 대상이 올바르지 않다")
	}
	for i, stage := range stages {
		position := slices.Index(known, stage)
		if position < 0 || (i > 0 && position != slices.Index(known, stages[i-1])+1) {
			return nil, fmt.Errorf("그래프 비교는 인접 누적 단계 순서로 지정해야 한다")
		}
	}
	if compareIndex && len(stages) != 1 {
		return nil, fmt.Errorf("색인 대상 비교에는 고정 그래프 단계 하나만 지정해야 한다")
	}
	var variants []comparisonVariant
	for _, stage := range stages {
		graphStage, evidence := stageConfig(stage)
		variants = append(variants, comparisonVariant{Name: stage, Stage: string(graphStage), IndexTargets: targets, GlobalFallback: graphStage == search.GraphStageGlobal, EvidenceSelection: evidence})
	}
	if compareIndex {
		variant := variants[0]
		variants = nil
		for _, target := range []string{"all_layers", "without_source"} {
			variant.Name, variant.IndexTargets = target, target
			variants = append(variants, variant)
		}
	}
	return variants, nil
}

func controlledQueries(queries querySet, asOf time.Time) (querySet, error) {
	queries.Queries = slices.Clone(queries.Queries)
	for i := range queries.Queries {
		query := &queries.Queries[i]
		if query.Scope != "" && query.Scope != "auto" {
			return querySet{}, fmt.Errorf("통제 비교의 모든 질의는 scope=auto여야 한다")
		}
		query.Scope = "auto"
		if query.AsOf == nil {
			query.AsOf = new(asOf.UTC())
		}
	}
	return queries, nil
}

// 준비 전후 카운터 차이는 실제 HTTP 시도만 세고 이후 질의 호출은 제외한다.
type embeddingCounter struct {
	next  http.RoundTripper
	calls atomic.Int64
}

func (counter *embeddingCounter) RoundTrip(request *http.Request) (*http.Response, error) {
	counter.calls.Add(1)
	return counter.next.RoundTrip(request)
}

type comparisonBackend struct {
	database *store.Store
	worker   *index.Worker
	counter  *embeddingCounter
}

func runControlledCommand(ctx context.Context, settings settings, contexts contextSet, queries querySet, options controlledOptions, output string) error {
	return runControlledInput(ctx, settings, comparisonInput{Datasets: []comparisonDataset{{Code: "direct", Contexts: contexts, Queries: queries}}, ContextVersion: contexts.Version, QueryVersion: queries.Version}, options, output)
}

func runControlledInput(ctx context.Context, settings settings, input comparisonInput, options controlledOptions, output string) error {
	variants, err := comparisonVariants(options.Stages, options.IndexTargets, options.CompareIndex)
	if err != nil {
		return err
	}
	asOf := time.Now().UTC()
	input.Datasets = slices.Clone(input.Datasets)
	if len(input.Datasets) == 0 {
		return fmt.Errorf("비교 데이터셋이 없다")
	}
	for i := range input.Datasets {
		input.Datasets[i].Queries, err = controlledQueries(input.Datasets[i].Queries, asOf)
		if err != nil {
			return err
		}
	}
	if options.Repeats != initialEvaluationRepeats {
		return fmt.Errorf("통제 비교는 조건별 10회여야 한다")
	}
	backends := map[string]comparisonBackend{}
	for _, variant := range variants {
		if _, exists := backends[variant.IndexTargets]; exists {
			continue
		}
		database, err := store.New(ctx, settings.databaseURL, settings.graphName, nil, nil, store.IndexTargets(variant.IndexTargets))
		if err != nil {
			return fmt.Errorf("비교 저장소 준비: %w", err)
		}
		defer database.Close()
		if err := database.CheckEmbeddingSchema(ctx, settings.index.VectorType, settings.index.Dimension); err != nil {
			return err
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		defer transport.CloseIdleConnections()
		counter := &embeddingCounter{next: transport}
		worker, err := index.New(database, settings.index, &http.Client{Transport: counter, Timeout: 30 * time.Second}, slog.Default())
		if err != nil {
			return err
		}
		backends[variant.IndexTargets] = comparisonBackend{database, worker, counter}
	}
	connection, err := pgx.Connect(ctx, settings.databaseURL)
	if err != nil {
		return fmt.Errorf("비교 저장량 연결: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = connection.Close(closeCtx)
	}()
	stages := make([]string, 0, len(variants))
	for _, variant := range variants {
		stages = append(stages, variant.Name)
	}
	axis, target := "graph_stage", options.IndexTargets
	if options.CompareIndex {
		axis, target = "index_targets", "compared"
	}
	result := report{StartedAt: asOf, ContextVersion: input.ContextVersion, QueryVersion: input.QueryVersion, InputPreparation: input.Preparation,
		Conditions: conditions{ComparisonAxis: axis, GraphGroups: len(input.Datasets), Variants: variants, PreparationPolicy: "fresh_graph_no_explicit_warmup_shared_cache", AsOf: asOf,
			Stages: stages, Repeats: options.Repeats, Budget: options.Budget, MaxHops: options.MaxHops, MaxHopNodes: options.MaxHopNodes,
			Execution: string(settings.execution), CandidateLimit: settings.candidateLimit, SemanticThreshold: settings.semanticThreshold,
			FoldThreshold: settings.foldThreshold, EmbeddingModel: settings.index.ModelID(), IndexTargets: target}}
	result.Runs, err = executeComparisons(ctx, variants, options.Repeats, func(ctx context.Context, variant comparisonVariant, repeat int) (runMetrics, error) {
		backend := backends[variant.IndexTargets]
		service, err := search.New(backend.database, backend.worker, search.Config{Execution: settings.execution,
			CandidateLimit: settings.candidateLimit, SemanticThreshold: settings.semanticThreshold, FoldThreshold: settings.foldThreshold,
			GraphStage: search.GraphStage(variant.Stage), GlobalFallback: variant.GlobalFallback, EvidencePathSelection: variant.EvidenceSelection}, slog.Default())
		if err != nil {
			return runMetrics{}, err
		}
		preparation := preparationMetrics{}
		var parts []measurement
		var duration time.Duration
		for _, dataset := range input.Datasets {
			started, before := time.Now(), backend.counter.calls.Load()
			err := withEvaluationGraph(ctx, backend.database, backend.worker, dataset.Contexts, len(dataset.Contexts.Contexts)*4+64, func(graph loadedGraph) error {
				preparation.DurationMS += float64(time.Since(started).Microseconds()) / 1000
				preparation.EmbeddingCalls += backend.counter.calls.Load() - before
				var footprint preparationMetrics
				if err := readVectorFootprint(ctx, connection, graph.GraphID, backend.worker.ModelID(), &footprint); err != nil {
					return err
				}
				preparation.VectorRows += footprint.VectorRows
				preparation.VectorBytes += footprint.VectorBytes
				measuring := time.Now()
				measured, err := measure(ctx, service, graph, dataset.Queries, options.Budget, options.MaxHops, options.MaxHopNodes)
				duration += time.Since(measuring)
				parts = append(parts, measured)
				return err
			})
			if err != nil {
				return runMetrics{}, err
			}
		}
		measured := mergeMeasurements(parts)
		return runMetrics{Stage: variant.Name, Repeat: repeat, DurationMS: duration.Milliseconds(), Preparation: &preparation, UseCases: measured.UseCases, QuerySamples: measured.QuerySamples, recovered: measured.Recovered}, nil
	})
	if err != nil {
		return err
	}
	runs := map[string][]runMetrics{}
	for _, run := range result.Runs {
		runs[run.Stage] = append(runs[run.Stage], run)
	}
	if err := validateMeasuredRuns(stages, runs); err != nil {
		return err
	}
	result.Judgements, result.Marginals = judge(stages, runs), marginals(stages, runs)
	result.Assessments = assessComparisons(axis, result.Judgements, runs)
	return writeReport(output, result)
}

// 소프트 삭제 전에 현재 그래프·모델의 행과 열 저장량만 측정한다.
func readVectorFootprint(ctx context.Context, connection *pgx.Conn, graphID model.ID, modelID string, preparation *preparationMetrics) error {
	if err := connection.QueryRow(ctx, `SELECT count(*), coalesce(sum(pg_column_size(embedding)), 0)
		FROM public.context_embedding WHERE graph_id=$1 AND model_id=$2`, graphID.String(), modelID).Scan(&preparation.VectorRows, &preparation.VectorBytes); err != nil {
		return fmt.Errorf("비교 벡터 저장량 측정: %w", err)
	}
	return nil
}

func executeComparisons(ctx context.Context, variants []comparisonVariant, repeats int, measure func(context.Context, comparisonVariant, int) (runMetrics, error)) ([]runMetrics, error) {
	var runs []runMetrics
	if len(variants) == 0 || repeats != initialEvaluationRepeats {
		return nil, fmt.Errorf("비교 조건과 10회 반복이 필요하다")
	}
	for repeat := 1; repeat <= repeats; repeat++ {
		for offset := range variants {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			variant := variants[(offset+repeat-1)%len(variants)]
			run, err := measure(ctx, variant, repeat)
			if err != nil {
				return nil, fmt.Errorf("조건 %s 회차 %d: %w", variant.Name, repeat, err)
			}
			runs = append(runs, run)
		}
	}
	return runs, nil
}

type comparisonAssessment struct {
	Condition         string `json:"condition"`
	UseCase           string `json:"use_case"`
	Status            string `json:"status"`
	Reason            string `json:"reason"`
	DefaultActivation bool   `json:"default_activation"`
}

// 관찰 결과는 운영 기본값을 승인하지 않는다. 최종 비용·효과는 EVAL-12/13이 소유한다.
func assessComparisons(axis string, judgements []stageJudgement, runs map[string][]runMetrics) []comparisonAssessment {
	var results []comparisonAssessment
	for _, judgement := range judgements {
		if judgement.ComparedTo == "" {
			continue
		}
		for _, useCase := range useCases {
			comparisons, present := judgement.Comparisons[useCase]
			if !present {
				continue
			}
			value := comparisonAssessment{Condition: judgement.Stage, UseCase: useCase, Status: "deferred", Reason: "insufficient_independent_units"}
			ready, improved, regressed, recallLoss := true, false, false, false
			required := map[string]bool{"recall": false, "reciprocal_rank": false, "latency_ms": false, "budget_used": false}
			for _, metric := range comparisons {
				if _, ok := required[metric.Metric]; ok {
					required[metric.Metric] = metric.Status == "measured" && metric.Samples >= 2
				}
				if metric.Status == "measured" && metric.Significant {
					regressed = regressed || (!metric.Improved && metric.Metric != "budget_used")
					recallLoss = recallLoss || (metric.Metric == "recall" && !metric.Improved)
					improved = improved || (metric.Improved && slices.Contains([]string{"recall", "reciprocal_rank", "evidence_completeness", "path_continuity", "budget_per_hit"}, metric.Metric))
				}
			}
			for _, measured := range required {
				ready = ready && measured
			}
			ready = ready && len(runs[judgement.Stage]) == initialEvaluationRepeats
			failed, slow := false, false
			for _, run := range runs[judgement.Stage] {
				metrics, ok := run.UseCases[useCase]
				if !ok {
					ready = false
					continue
				}
				ready = ready && validMetric(metrics.LatencyP95MS)
				slow = slow || metrics.LatencyP95MS > 500
				for _, count := range metrics.Failures {
					failed = failed || count > 0
				}
			}
			for _, run := range runs[judgement.ComparedTo] {
				for _, count := range run.UseCases[useCase].Failures {
					failed = failed || count > 0
				}
			}
			if ready {
				switch {
				case failed:
					value.Status, value.Reason = "rejected", "channel_failures"
				case slow:
					value.Status, value.Reason = "rejected", "latency_slo_exceeded"
				case axis == "index_targets" && recallLoss:
					value.Status, value.Reason = "rejected", "significant_recall_loss"
				case axis == "index_targets":
					value.Reason = "no_significant_recall_loss_final_acceptance_pending"
				case regressed:
					value.Status, value.Reason = "rejected", "significant_regression"
				case !improved:
					value.Status, value.Reason = "rejected", "no_significant_improvement"
				default:
					value.Reason = "improvement_observed_cost_and_public_acceptance_pending"
				}
			}
			results = append(results, value)
		}
	}
	return results
}
