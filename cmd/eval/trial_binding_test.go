package main

import (
	"crypto/sha256"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func trialEmbedded(t *testing.T, mode, role, version string, value any, units []manifestUnit) (executionManifest, trialBindings) {
	t.Helper()
	m := validManifest()
	m.Mode = mode
	m.TrialID = "embedded-" + mode
	m.Provenance = "synthetic"
	m.Mapping = manifestMapping{Preprocessing: "direct-v1", Split: "direct-v1", Answers: "direct-v1", Evidence: "direct-v1", AnswerPolicy: "evaluation_only"}
	m.Units = units
	m.Exclusions = manifestExclusions{Policy: "direct-v1", Total: len(units), Items: []manifestExclusion{}}
	m.Execution.Preparation, m.Execution.Cache = "fresh_graph_no_explicit_warmup_shared_cache", "shared_db_os_cache"
	m.Execution.Seeds = []manifestSeed{{Component: "embedding", Supported: new(false)}}
	path := writeManifestFixture(t, value)
	raw, _ := os.ReadFile(path)
	m.Inputs = []manifestInput{{Code: "input", Dataset: "embedded", Role: role, Version: version, Commit: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}}
	return m, trialBindings{"input": path}
}

func TestManifestTrialEmbeddedBindings(t *testing.T) {
	for _, mode := range []string{"units", "embeddings", "continual"} {
		t.Run(mode, func(t *testing.T) {
			var m executionManifest
			var bindings trialBindings
			switch mode {
			case "units":
				input := unitFixture(t, 1)
				var units []manifestUnit
				for _, sample := range input.Samples {
					for _, q := range sample.Queries {
						units = append(units, manifestUnit{Dataset: "embedded", Query: sample.Code + "." + q.ID, Task: sample.Code, Type: sample.Cohort, Independent: sample.Code})
					}
				}
				m, bindings = trialEmbedded(t, mode, "unit_comparison", input.Version, input, units)
				m.Models.AgentVersion, m.Models.AgentModel, m.Models.PromptVersion = input.Agent.Version, input.Agent.Model, input.Agent.PromptFamily
				m.Models.Embedding = input.Search.EmbeddingModel
				m.Cost.PricingVersion, m.Cost.Currency = input.Pricing, input.Currency
				s := input.Search
				m.Search.Stages = []string{s.Stage}
				m.Search.Budget, m.Search.MaxHops, m.Search.MaxHopNodes = new(s.Budget), new(s.MaxHops), new(s.MaxHopNodes)
				m.Search.ChannelExecution, m.Search.CandidateLimit = s.Execution, s.CandidateLimit
				m.Search.SemanticThreshold, m.Search.FoldThreshold = new(s.SemanticThreshold), new(s.FoldThreshold)
				m.Search.IndexTargets = s.IndexTargets
			case "embeddings":
				input := embeddingFixture(t)
				var units []manifestUnit
				for _, sample := range input.Samples {
					queries := slices.Clone(sample.Queries)
					for i := range queries {
						queries[i].IndependentUnit = sample.Unit
					}
					units = append(units, trialUnits("embedded", queries, sample.Code+".")...)
				}
				m, bindings = trialEmbedded(t, mode, "embedding_comparison", input.Version, input, units)
				s := input.Search
				m.Search.Stages = []string{"evidence"}
				m.Search.GlobalFallback, m.Search.EvidenceSelection = new(true), new(true)
				m.Search.SemanticThreshold = nil
				m.Search.Budget, m.Search.MaxHops, m.Search.MaxHopNodes = new(s.Budget), new(s.MaxHops), new(s.MaxHopNodes)
				m.Search.ChannelExecution, m.Search.CandidateLimit = s.Execution, s.CandidateLimit
				m.Search.FoldThreshold = new(s.FoldThreshold)
				for _, v := range input.Variants {
					t.Setenv(v.DatabaseEnv, "postgres://synthetic.invalid")
					t.Setenv(v.EndpointEnv, "https://embedding.invalid")
					t.Setenv(v.KeyEnv, "synthetic-key")
				}
			case "continual":
				inputs := trialContinualInputs()
				var units []manifestUnit
				for _, s := range inputs {
					units = append(units, manifestUnit{Dataset: "embedded", Query: s.Code, Task: s.Code, Type: s.Query.UseCase, Independent: s.Code})
				}
				m, bindings = trialEmbedded(t, mode, "continual_scenarios", "continual-v1", inputs, units)
				raw, _ := json.Marshal(inputs, json.Deterministic(true))
				m.Inputs[0].SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
				bindings["input"] = "builtin:continual-v1"
				m.Search.Budget = new(continualBudget)
			}
			if err := m.validate(); err != nil {
				t.Fatal(err)
			}
			trial, err := newManifestTrial(m, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := trial.finish(errors.New("binding_test_complete")); err == nil {
					t.Error("입력 검사만으로 정상 평가를 생성했다")
				}
			}()
			if err := trial.bindInputs(bindings, ""); err != nil {
				t.Fatal(err)
			}
			if trial.Preparation.Units != len(m.Units) || trial.Preparation.Independent < 2 {
				t.Fatal("복합 문항/독립 단위 연결 누락")
			}
			m.Units[0].Type = "wrong-type"
			trial.Manifest = m
			if err := trial.checkInputs(false); err == nil {
				t.Fatal("복합 입력의 문항 매핑 불일치 허용")
			}
		})
	}
}

func TestManifestTrialLegacyAggregateBinding(t *testing.T) {
	input := businessSet{Version: "legacy-v1", Conditions: businessConditions{AgentVersion: "agent-v1", Model: "model-v1", ContextBudget: 4000}}
	var units []manifestUnit
	for i := range 3 {
		input.Samples = append(input.Samples, businessTestSample(100, 50, 2, 1, 2, 1, 0, 0.5))
		code := fmt.Sprintf("sample-%d", i+1)
		units = append(units, manifestUnit{Dataset: "embedded", Query: code, Task: code, Type: "legacy_business", Independent: code})
	}
	m, b := trialEmbedded(t, "business", "business", input.Version, input, units)
	m.Models.AgentVersion, m.Models.AgentModel = input.Conditions.AgentVersion, input.Conditions.Model
	m.Search.Budget = new(4000)
	m.Cost.Items = nil
	for _, c := range businessComponents {
		m.Cost.Items = append(m.Cost.Items, manifestCostItem{Code: c, Status: "unmeasured"})
	}
	root := t.TempDir()
	if err := runManifestTrial(writeManifestFixture(t, m), root, b, ""); err != nil {
		t.Fatal(err)
	}
	assertTrialFiles(t, root, m.TrialID, true)
	var summary trialSummary
	readTrialFile(t, root, m.TrialID, "summary.json", &summary)
	if summary.Source != "imported_aggregate_trace" || summary.Decisions[0].Status != "deferred" {
		t.Fatal("기존 네 지표로 확장 과업 효과를 대신했다")
	}
}

func TestManifestTrialDecisionGuards(t *testing.T) {
	m, _, _ := trialFixture(t, "lookup")
	m.Provenance = "observed"
	trial := manifestTrial{Manifest: m, Preparation: trialPreparation{Independent: 3}}
	cost := trialCost{Total: businessCostTotal{Status: "unmeasured", Total: -1}}
	for _, test := range []struct{ status, want string }{{"candidate", "deferred"}, {"rejected", "not_accepted"}, {"deferred", "deferred"}} {
		if got := trial.decision("candidate", "fact", test.status, "reason", cost); got.Status != test.want {
			t.Fatalf("미측정/악화 판정: %+v", got)
		}
	}
	cost.Total.Status = "measured"
	trial.Preparation.Independent = 1
	if got := trial.decision("candidate", "fact", "candidate", "reason", cost); got.Status != "deferred" {
		t.Fatal("독립 표본 부족 채택")
	}
}
