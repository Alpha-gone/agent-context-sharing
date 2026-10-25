package main

import (
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/search"
)

type embeddingInput struct {
	Version    string             `json:"version"`
	Provenance string             `json:"provenance"`
	Axis       string             `json:"axis"`
	GraphName  string             `json:"graph_name"`
	AsOf       time.Time          `json:"as_of"`
	Search     embeddingSearch    `json:"search"`
	Variants   []embeddingVariant `json:"variants"`
	Samples    []embeddingSample  `json:"samples"`
}

type embeddingSearch struct {
	Budget         int     `json:"budget"`
	MaxHops        int     `json:"max_hops"`
	MaxHopNodes    int     `json:"max_hop_nodes"`
	Execution      string  `json:"channel_execution"`
	CandidateLimit int     `json:"candidate_limit"`
	RelationLimit  int     `json:"relation_limit"`
	FoldThreshold  float64 `json:"fold_threshold"`
	IndexTargets   string  `json:"index_targets"`
}

type embeddingVariant struct {
	Code               string    `json:"code"`
	Provider           string    `json:"provider"`
	Model              string    `json:"model"`
	Dimension          int       `json:"dimension"`
	VectorType         string    `json:"vector_type"`
	DatabaseEnv        string    `json:"database_env"`
	EndpointEnv        string    `json:"endpoint_env"`
	KeyEnv             string    `json:"key_env"`
	RelationThresholds []float64 `json:"relation_thresholds"`
	GlobalThresholds   []float64 `json:"global_thresholds"`
}

type embeddingSample struct {
	Code           string          `json:"code"`
	Unit           string          `json:"independent_unit"`
	Split          string          `json:"split"`
	Contexts       contextSet      `json:"contexts"`
	Queries        []querySpec     `json:"queries"`
	GlobalExpected map[string]bool `json:"global_expected"`
	Anchor         string          `json:"relation_anchor"`
	Related        map[string]bool `json:"related"`
}

func loadEmbeddingInput(path string) (embeddingInput, []settings, error) {
	var input embeddingInput
	file, err := os.Open(path)
	if err != nil {
		return input, nil, fmt.Errorf("임베딩 비교 입력 읽기 실패")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 16<<20+1))
	if err != nil || len(raw) > 16<<20 {
		return input, nil, fmt.Errorf("임베딩 비교 입력 크기 오류")
	}
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)); err != nil {
		return input, nil, fmt.Errorf("임베딩 비교 JSON 형식 오류")
	}
	if err := validateEmbeddingInput(input); err != nil {
		return input, nil, err
	}
	configs := make([]settings, 0, 2)
	for _, v := range input.Variants {
		endpoint, err := url.Parse(os.Getenv(v.EndpointEnv))
		if err != nil {
			return input, nil, fmt.Errorf("임베딩 비교 접속 환경 오류")
		}
		cfg := index.Config{Provider: v.Provider, Model: v.Model, Dimension: v.Dimension, VectorType: v.VectorType, BaseURL: endpoint, APIKey: os.Getenv(v.KeyEnv)}
		if err := cfg.Validate(); err != nil {
			return input, nil, err
		}
		database := os.Getenv(v.DatabaseEnv)
		if database == "" {
			return input, nil, fmt.Errorf("임베딩 비교 DB 환경 누락")
		}
		configs = append(configs, settings{databaseURL: database, graphName: input.GraphName, index: cfg})
	}
	return input, configs, nil
}

func validateEmbeddingInput(in embeddingInput) error {
	invalid := func() error {
		return fmt.Errorf("임베딩 비교의 통제 조건·표본·조정/최종 분리가 올바르지 않다")
	}
	if !manifestCode.MatchString(in.Version) || !manifestCode.MatchString(in.GraphName) || !slices.Contains([]string{"synthetic", "observed"}, in.Provenance) || !slices.Contains([]string{"model", "dimension", "compression"}, in.Axis) || in.AsOf.IsZero() || in.AsOf.Location() != time.UTC || len(in.Variants) != 2 || len(in.Samples) < 2 || len(in.Samples) > 32 {
		return invalid()
	}
	s := in.Search
	if s.Budget < 0 || s.MaxHops < 0 || s.MaxHopNodes < 0 || s.CandidateLimit < 1 || s.CandidateLimit > 256 || s.RelationLimit < 1 || s.RelationLimit > 24 || !manifestRatio(&s.FoldThreshold) || s.FoldThreshold == 0 || !slices.Contains([]string{"all_layers", "without_source"}, s.IndexTargets) || !slices.Contains([]string{string(search.ExecutionParallel), string(search.ExecutionSequential)}, s.Execution) {
		return invalid()
	}
	for _, v := range in.Variants {
		if !manifestCode.MatchString(v.Code) || !originalEnvName.MatchString(v.DatabaseEnv) || !originalEnvName.MatchString(v.EndpointEnv) || !originalEnvName.MatchString(v.KeyEnv) {
			return invalid()
		}
		cfg := index.Config{Provider: v.Provider, Model: v.Model, Dimension: v.Dimension, VectorType: v.VectorType, BaseURL: &url.URL{Scheme: "https", Host: "embedding.invalid"}}
		if err := cfg.ValidateMetadata(); err != nil {
			return invalid()
		}
		for i, grid := range [][]float64{v.RelationThresholds, v.GlobalThresholds} {
			if len(grid) < 2 || len(grid) > 64 {
				return invalid()
			}
			seen := map[float64]bool{}
			for _, x := range grid {
				if !validMetric(x+1) || x > 1 || (i == 1 && x < 0) || seen[x] {
					return invalid()
				}
				seen[x] = true
			}
		}
	}
	a, b := in.Variants[0], in.Variants[1]
	if a.Code == b.Code || a.Provider != b.Provider || (a.Model != b.Model) != (in.Axis == "model") || (a.Dimension != b.Dimension) != (in.Axis == "dimension") || (a.VectorType != b.VectorType) != (in.Axis == "compression") {
		return invalid()
	}
	units, locators, codes := map[string]string{}, map[string]string{}, map[string]bool{}
	splits := map[string]int{}
	for _, sample := range in.Samples {
		if !manifestCode.MatchString(sample.Code) || !manifestCode.MatchString(sample.Unit) || codes[sample.Code] || !slices.Contains([]string{"tuning", "evaluation"}, sample.Split) || len(sample.Contexts.Contexts) > 64 || len(sample.Queries) == 0 || len(sample.Queries) > 32 {
			return invalid()
		}
		codes[sample.Code] = true
		splits[sample.Split]++
		if old := units[sample.Unit]; old != "" && old != sample.Split {
			return invalid()
		}
		units[sample.Unit] = sample.Split
		if _, err := validateContextSet(sample.Contexts, ""); err != nil {
			return invalid()
		}
		if _, err := validateQuerySet(querySet{Version: in.Version, Queries: sample.Queries}, sample.Contexts, ""); err != nil {
			return invalid()
		}
		events := map[string]bool{}
		for _, c := range sample.Contexts.Contexts {
			if !manifestCode.MatchString(c.Key) {
				return invalid()
			}
			if c.Layer == "event" {
				events[c.Key] = true
			}
			if c.Source != nil {
				if c.Source.Locator == "" {
					return invalid()
				}
				if old := locators[c.Source.Locator]; old != "" && old != sample.Split {
					return invalid()
				}
				locators[c.Source.Locator] = sample.Split
			}
		}
		if len(events) < 2 || len(events) > 24 || !events[sample.Anchor] || len(sample.Related) != len(events)-1 {
			return invalid()
		}
		for key := range sample.Related {
			if key == sample.Anchor || !events[key] {
				return invalid()
			}
		}
		if len(sample.GlobalExpected) != len(sample.Queries) {
			return invalid()
		}
		for _, q := range sample.Queries {
			if !manifestCode.MatchString(q.ID) || (q.IndependentUnit != "" && q.IndependentUnit != sample.Unit) || (q.Scope != "" && q.Scope != "auto") || (q.AsOf != nil && !q.AsOf.Equal(in.AsOf)) {
				return invalid()
			}
			if _, ok := sample.GlobalExpected[q.ID]; !ok {
				return invalid()
			}
		}
	}
	if splits["tuning"] == 0 || splits["evaluation"] == 0 {
		return invalid()
	}
	return nil
}
