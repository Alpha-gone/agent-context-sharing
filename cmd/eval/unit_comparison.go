package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

type unitReport struct {
	Version    string       `json:"version"`
	Provenance string       `json:"provenance"`
	Agent      unitAgent    `json:"agent"`
	Judge      unitJudge    `json:"judge"`
	Pricing    string       `json:"pricing_version"`
	Currency   string       `json:"currency"`
	Policies   []unitPolicy `json:"policies"`
	Search     unitSearch   `json:"search"`
	AsOf       time.Time    `json:"default_as_of"`
	Repeats    int          `json:"repeats"`
	Runs       []unitRun    `json:"runs"`
	Cohorts    []unitCohort `json:"cohorts"`
}

type unitRun struct {
	Sample  string            `json:"sample"`
	Cohort  string            `json:"cohort"`
	Review  unitReviewMetrics `json:"unit_completeness"`
	Reviews []unitReview      `json:"review_records"`
	Task    unitTaskMetrics   `json:"imported_task_trace"`
	Metrics runMetrics        `json:"retrieval"`
}

type unitReviewMetrics struct {
	Derived       int     `json:"derived_count"`
	Complete      int     `json:"complete_count"`
	Completeness  float64 `json:"complete_ratio"`
	HumanReviewed int     `json:"human_reviewed_count"`
	Disagreements int     `json:"human_disagreement_count"`
	MissingLinks  int     `json:"missing_evidence_links_count"`
}

type unitTaskMetrics struct {
	InputHash string  `json:"input_sha256"`
	Trace     string  `json:"trace_version"`
	Success   float64 `json:"success"`
	TotalCost float64 `json:"total_cost"`
}

func runUnitCommand(ctx context.Context, settings settings, input unitInput, output string) error {
	if err := validateUnitInput(input); err != nil {
		return err
	}
	s := input.Search
	if s.EmbeddingModel != settings.index.ModelID() || s.Execution != string(settings.execution) || s.CandidateLimit != settings.candidateLimit || s.SemanticThreshold != settings.semanticThreshold || s.FoldThreshold != settings.foldThreshold {
		return fmt.Errorf("검색 단위 입력과 실행 환경의 통제 조건이 다르다")
	}
	database, err := store.New(ctx, settings.databaseURL, settings.graphName, nil, nil, store.IndexTargets(s.IndexTargets))
	if err != nil {
		return err
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
	connection, err := pgx.Connect(ctx, settings.databaseURL)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = connection.Close(closeCtx)
	}()
	variants, err := comparisonVariants([]string{s.Stage}, s.IndexTargets, false)
	if err != nil {
		return err
	}
	config := variants[0]
	service, err := search.New(database, worker, search.Config{Execution: settings.execution, CandidateLimit: settings.candidateLimit,
		SemanticThreshold: settings.semanticThreshold, FoldThreshold: settings.foldThreshold, GraphStage: search.GraphStage(config.Stage),
		GlobalFallback: config.GlobalFallback, EvidencePathSelection: config.EvidenceSelection}, slog.Default())
	if err != nil {
		return err
	}
	result := unitReport{Version: input.Version, Provenance: input.Provenance, Agent: input.Agent, Judge: input.Judge,
		Pricing: input.Pricing, Currency: input.Currency, Policies: input.Policies, Search: input.Search, AsOf: time.Now().UTC(), Repeats: initialEvaluationRepeats}
	var conditions []comparisonVariant
	for _, policy := range input.Policies {
		conditions = append(conditions, comparisonVariant{Name: policy.Name})
	}
	_, err = executeComparisons(ctx, conditions, initialEvaluationRepeats, func(ctx context.Context, policy comparisonVariant, repeat int) (runMetrics, error) {
		for _, sample := range input.Samples {
			variant := sample.Variants[slices.IndexFunc(input.Policies, func(p unitPolicy) bool { return p.Name == policy.Name })]
			queries, err := controlledQueries(unitQueries(input, sample, variant), result.AsOf)
			if err != nil {
				return runMetrics{}, err
			}
			contexts := contextSet{Version: input.Version + "-" + sample.Code + "-" + policy.Name, Contexts: append(slices.Clone(sample.Sources), variant.Derived...)}
			prep := preparationMetrics{}
			task := variant.Tasks[repeat-1]
			run := unitRun{Sample: sample.Code, Cohort: sample.Cohort, Review: reviewUnit(sample, variant), Reviews: variant.Reviews, Task: unitTaskMetrics{InputHash: task.InputHash, Trace: task.Trace, Success: -1, TotalCost: -1}}
			if task := variant.Tasks[repeat-1]; task.Success != nil {
				run.Task.Success = 0
				if *task.Success {
					run.Task.Success = 1
				}
			}
			if cost := variant.Tasks[repeat-1].TotalCost; cost != nil {
				run.Task.TotalCost = *cost
			}
			started, before := time.Now(), counter.calls.Load()
			err = withEvaluationGraph(ctx, database, worker, contexts, len(contexts.Contexts)*4+64, func(graph loadedGraph) error {
				prep.DurationMS, prep.EmbeddingCalls = float64(time.Since(started).Microseconds())/1000, counter.calls.Load()-before
				if err := readVectorFootprint(ctx, connection, graph.GraphID, worker.ModelID(), &prep); err != nil {
					return err
				}
				measuring := time.Now()
				measured, err := measure(ctx, service, graph, queries, s.Budget, s.MaxHops, s.MaxHopNodes)
				if err != nil {
					return err
				}
				for _, metrics := range measured.UseCases {
					for _, failures := range metrics.Failures {
						if failures > 0 {
							return fmt.Errorf("검색 단위 비교의 검색 채널 실패")
						}
					}
				}
				run.Metrics = runMetrics{Stage: policy.Name, Repeat: repeat, Preparation: &prep, DurationMS: time.Since(measuring).Milliseconds(), UseCases: measured.UseCases, QuerySamples: measured.QuerySamples}
				return nil
			})
			if err != nil {
				return runMetrics{}, err
			}
			result.Runs = append(result.Runs, run)
		}
		return runMetrics{}, nil
	})
	if err != nil {
		return err
	}
	result.Cohorts, err = aggregateUnits(input, result.Runs)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeReport(output, result)
}

func reviewUnit(sample unitSample, variant unitVariant) unitReviewMetrics {
	result := unitReviewMetrics{Derived: len(variant.Derived)}
	elements := map[string]unitElement{}
	for _, element := range sample.Elements {
		elements[element.Code] = element
	}
	for i, review := range variant.Reviews {
		preserved := review.Preserved
		if review.Human != nil {
			result.HumanReviewed++
			preserved = review.Human.Preserved
			if !slices.Equal(slices.Sorted(slices.Values(preserved)), slices.Sorted(slices.Values(review.Preserved))) {
				result.Disagreements++
			}
		}
		complete := len(preserved) == len(review.Required)
		links := variant.Derived[i].Derived.DerivedFrom
		for _, key := range review.Required {
			for _, source := range elements[key].Evidence {
				if !slices.Contains(links, source) {
					complete = false
					result.MissingLinks++
				}
			}
		}
		if complete {
			result.Complete++
		}
	}
	result.Completeness = float64(result.Complete) / float64(result.Derived)
	return result
}
