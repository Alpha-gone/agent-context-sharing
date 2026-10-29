package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

type embeddingThresholds struct {
	Relation float64 `json:"relation"`
	Global   float64 `json:"global"`
}

type embeddingScore struct {
	Target   string  `json:"target"`
	Score    float64 `json:"score"`
	Relevant bool    `json:"relevant"`
}

type embeddingSignal struct {
	Query    string  `json:"query"`
	Keyword  bool    `json:"keyword_candidate_present"`
	Semantic bool    `json:"semantic_candidate_present"`
	Top      float64 `json:"semantic_top"`
	Expected bool    `json:"global_expected"`
}

type embeddingRun struct {
	Variant     string             `json:"variant"`
	Split       string             `json:"split"`
	Sample      string             `json:"sample"`
	Unit        string             `json:"independent_unit"`
	Repeat      int                `json:"repeat"`
	Preparation preparationMetrics `json:"preparation"`
	Calls       int64              `json:"measurement_embedding_http_attempts"`
	DurationMS  float64            `json:"measurement_duration_ms"`
	Cost        float64            `json:"embedding_cost"`
	Relations   []embeddingScore   `json:"relation_scores"`
	Signals     []embeddingSignal  `json:"global_signals"`
	Metrics     *runMetrics        `json:"retrieval,omitzero"`
}

type embeddingCondition struct {
	Code         string              `json:"code"`
	ModelID      string              `json:"model_id"`
	RelationGrid []float64           `json:"relation_thresholds"`
	GlobalGrid   []float64           `json:"global_thresholds"`
	Selected     embeddingThresholds `json:"selected"`
}

type embeddingReport struct {
	Version     string                         `json:"version"`
	Provenance  string                         `json:"provenance"`
	Axis        string                         `json:"axis"`
	StartedAt   time.Time                      `json:"started_at"`
	AsOf        time.Time                      `json:"as_of"`
	Search      embeddingSearch                `json:"search"`
	Policy      string                         `json:"preparation_policy"`
	Repeats     int                            `json:"repeats"`
	Conditions  []embeddingCondition           `json:"conditions"`
	Runs        []embeddingRun                 `json:"runs"`
	Retrieval   []stageJudgement               `json:"retrieval_judgements"`
	Estimates   map[string]map[string]estimate `json:"threshold_resource_estimates"`
	Comparisons []comparison                   `json:"threshold_resource_comparisons"`
	Status      string                         `json:"status"`
	Reason      string                         `json:"reason"`
	Activated   bool                           `json:"default_activation"`
}

type embeddingBackend struct {
	comparisonBackend
}

func runEmbeddingCommand(ctx context.Context, path, output string) error {
	input, configs, err := loadEmbeddingInput(path)
	if err != nil {
		return err
	}
	return runEmbeddingInput(ctx, input, configs, output)
}

func runEmbeddingInput(ctx context.Context, input embeddingInput, configs []settings, output string) error {
	if err := validateEmbeddingInput(input); err != nil {
		return err
	}
	if len(configs) != 2 {
		return fmt.Errorf("임베딩 비교 구성 누락")
	}
	for i, cfg := range configs {
		v := input.Variants[i]
		if err := cfg.index.Validate(); err != nil {
			return err
		}
		if cfg.index.ModelID() != (index.Config{Provider: v.Provider, Model: v.Model, Dimension: v.Dimension, VectorType: v.VectorType}).ModelID() || cfg.graphName != input.GraphName || cfg.databaseURL == "" {
			return fmt.Errorf("임베딩 비교 구성 불일치")
		}
		if cfg.index.BaseURL.String() != configs[0].index.BaseURL.String() {
			return fmt.Errorf("임베딩 비교 제공자 endpoint가 다르다")
		}
	}
	backends := make([]embeddingBackend, 2)
	// 두 스키마를 먼저 확인하므로 두 번째 구성이 잘못되어도 그래프를 만들지 않는다.
	for i, cfg := range configs {
		database, err := store.New(ctx, cfg.databaseURL, cfg.graphName, nil, nil, store.IndexTargets(input.Search.IndexTargets))
		if err != nil {
			return fmt.Errorf("임베딩 비교 DB 준비 실패")
		}
		defer database.Close()
		if err := database.CheckEmbeddingSchema(ctx, cfg.index.VectorType, cfg.index.Dimension); err != nil {
			return err
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		defer transport.CloseIdleConnections()
		counter := &embeddingCounter{next: transport}
		worker, err := index.New(database, cfg.index, &http.Client{Transport: counter, Timeout: 30 * time.Second}, slog.Default())
		if err != nil {
			return err
		}

		backends[i] = embeddingBackend{comparisonBackend{database, worker, counter}}
	}
	result := embeddingReport{Version: input.Version, Provenance: input.Provenance, Axis: input.Axis, StartedAt: time.Now().UTC(), AsOf: input.AsOf, Search: input.Search, Policy: "fresh_graph_no_explicit_warmup_shared_cache", Repeats: initialEvaluationRepeats}
	for i, v := range input.Variants {
		result.Conditions = append(result.Conditions, embeddingCondition{Code: v.Code, ModelID: configs[i].index.ModelID(), RelationGrid: v.RelationThresholds, GlobalGrid: v.GlobalThresholds})
	}
	for _, split := range []string{"tuning", "evaluation"} {
		for repeat := 1; repeat <= initialEvaluationRepeats; repeat++ {
			for offset := range input.Variants {
				i := (offset + repeat - 1) % 2
				for _, sample := range input.Samples {
					if sample.Split != split {
						continue
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					run, err := measureEmbeddingSample(ctx, input, sample, backends[i], result.Conditions[i], repeat)
					if err != nil {
						return fmt.Errorf("임베딩 비교 %s/%s/%s 회차 %d: %w", split, input.Variants[i].Code, sample.Code, repeat, err)
					}
					result.Runs = append(result.Runs, run)
					trialRoundDone(ctx, input.Variants[i].Code, sample.Code, repeat)
				}
			}
		}
		if split == "tuning" {
			for i, v := range input.Variants {
				selected, err := tuneEmbeddingThresholds(input, v, result.Runs)
				if err != nil {
					return err
				}
				result.Conditions[i].Selected = selected
			}
		}
	}
	if err := aggregateEmbedding(input, &result); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeReport(output, result)
}

func measureEmbeddingSample(ctx context.Context, input embeddingInput, sample embeddingSample, backend embeddingBackend, condition embeddingCondition, repeat int) (embeddingRun, error) {
	run := embeddingRun{Variant: condition.Code, Split: sample.Split, Sample: sample.Code, Unit: sample.Unit, Repeat: repeat, Cost: -1}
	set := sample.Contexts
	set.Version = input.Version + "-" + sample.Code + "-" + condition.Code
	started, before := time.Now(), backend.counter.calls.Load()
	err := withEvaluationGraph(ctx, backend.database, backend.worker, set, len(set.Contexts)*4+64, func(graph loadedGraph) error {
		run.Preparation.DurationMS = float64(time.Since(started).Microseconds()) / 1000
		run.Preparation.EmbeddingCalls = backend.counter.calls.Load() - before
		if err := readVectorFootprint(ctx, backend.database, graph.GraphID, backend.worker.ModelID(), &run.Preparation); err != nil {
			return err
		}
		start, calls := time.Now(), backend.counter.calls.Load()
		var err error
		run.Relations, err = embeddingRelationScores(ctx, backend, graph, sample)
		if err != nil {
			return err
		}
		queries := querySet{Version: input.Version, Queries: slices.Clone(sample.Queries)}
		for j := range queries.Queries {
			q := &queries.Queries[j]
			q.ID = sample.Code + "." + q.ID
			q.Scope = "auto"
			q.AsOf = new(input.AsOf)
			q.IndependentUnit = sample.Unit
		}
		for j, q := range queries.Queries {
			vector, err := backend.worker.Embed(ctx, q.WorkContext)
			if err != nil {
				return err
			}
			semantic, err := backend.database.SemanticCandidates(ctx, graph.GraphID, backend.worker.ModelID(), vector, input.AsOf, input.Search.CandidateLimit)
			if err != nil {
				return err
			}
			keywords, err := backend.database.KeywordCandidates(ctx, graph.GraphID, q.WorkContext, input.AsOf, input.Search.CandidateLimit)
			if err != nil {
				return err
			}
			signal := embeddingSignal{Query: q.ID, Keyword: len(keywords) > 0, Semantic: len(semantic) > 0, Top: -1, Expected: sample.GlobalExpected[sample.Queries[j].ID]}
			if len(semantic) > 0 {
				signal.Top = semantic[0].Similarity
			}
			run.Signals = append(run.Signals, signal)
		}
		if sample.Split == "evaluation" {
			s := input.Search
			service, err := search.New(backend.database, backend.worker, search.Config{Execution: search.Execution(s.Execution), CandidateLimit: s.CandidateLimit, SemanticThreshold: condition.Selected.Global, FoldThreshold: s.FoldThreshold, GraphStage: search.GraphStageGlobal, GlobalFallback: true, EvidencePathSelection: true}, slog.Default())
			if err != nil {
				return err
			}
			measured, err := measure(ctx, service, graph, queries, s.Budget, s.MaxHops, s.MaxHopNodes)
			if err != nil {
				return err
			}
			for _, metrics := range measured.UseCases {
				for _, failures := range metrics.Failures {
					if failures > 0 {
						return fmt.Errorf("임베딩 비교 검색 채널 실패")
					}
				}
			}
			run.Metrics = &runMetrics{Stage: condition.Code, Repeat: repeat, UseCases: measured.UseCases, QuerySamples: measured.QuerySamples}
		}
		run.Calls = backend.counter.calls.Load() - calls
		run.DurationMS = float64(time.Since(start).Microseconds()) / 1000
		return nil
	})
	return run, err
}

// 저장된 동일 그래프·모델의 사건 벡터만 읽는다. 전체 주석 후보의 점수 순서는
// 제안 실행과 같은 거리·UUID 동률 순서이며 관계 상태를 만들지 않는다.
func embeddingRelationScores(ctx context.Context, b embeddingBackend, g loadedGraph, s embeddingSample) ([]embeddingScore, error) {
	ids, keys := []model.ID{}, map[model.ID]string{}
	for key := range s.Related {
		id := g.Keys[key]
		ids = append(ids, id)
		keys[id] = key
	}
	scores, err := b.database.EmbeddingSimilarities(ctx, g.GraphID, b.worker.ModelID(), g.Keys[s.Anchor], ids)
	if err != nil {
		return nil, fmt.Errorf("관계 점수 읽기 실패: %w", err)
	}
	var result []embeddingScore
	for _, score := range scores {
		if !validMetric(score.Similarity+1) || score.Similarity > 1 {
			return nil, fmt.Errorf("관계 점수 범위 오류")
		}
		key := keys[score.ContextID]
		result = append(result, embeddingScore{key, score.Similarity, s.Related[key]})
	}
	if len(result) != len(s.Related) {
		return nil, fmt.Errorf("관계 점수 벡터 누락")
	}
	return result, nil
}
