package main

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type trialContinualInput struct {
	Code     string     `json:"code"`
	Contexts contextSet `json:"contexts"`
	Query    querySpec  `json:"query"`
	Stage    string     `json:"stage"`
}

func trialContinualInputs() []trialContinualInput {
	var result []trialContinualInput
	for _, s := range continualScenarios() {
		result = append(result, trialContinualInput{s.name, s.contexts, s.query, string(s.graphStage)})
	}
	return result
}

func (t *manifestTrial) readRole(role string, value any) error {
	raw, err := os.ReadFile(t.Inputs[role])
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value, json.RejectUnknownMembers(true))
}

func (t *manifestTrial) checkContexts(set contextSet) error {
	for _, c := range set.Contexts {
		if err := trialSafeCode(c.Key); err != nil {
			return err
		}
		t.Preparation.Contexts++
		switch c.Layer {
		case "source":
			t.Preparation.Source++
		case "derived":
			t.Preparation.Derived++
		case "event":
			t.Preparation.Events++
		}
	}
	return nil
}

func trialUnits(dataset string, queries []querySpec, prefix string) []manifestUnit {
	var units []manifestUnit
	for _, q := range queries {
		unit := q.IndependentUnit
		if unit == "" {
			unit = q.ID
		}
		kind := q.DatasetType
		if kind == "" {
			kind = q.UseCase
		}
		units = append(units, manifestUnit{Dataset: dataset, Query: prefix + q.ID, Task: unit, Type: kind, Independent: unit})
	}
	return units
}

func (t *manifestTrial) checkInputs(prepared bool) error {
	if prepared {
		return t.checkPrepared()
	}
	var units []manifestUnit
	version, role := "", ""
	switch t.Manifest.Mode {
	case "basic", "adaptive", "consistency", "adversarial":
		roles := []string{"contexts", "queries"}
		if t.Manifest.Mode == "adversarial" {
			roles = append(roles, "attacks")
		}
		if err := t.requireRoles(roles...); err != nil {
			return err
		}
		contexts, err := loadContextSet(t.Inputs["contexts"])
		if err != nil {
			return err
		}
		queries, err := loadQuerySet(t.Inputs["queries"], contexts)
		if err != nil {
			return err
		}
		if err := t.checkVersion("contexts", contexts.Version); err != nil {
			return err
		}
		if err := t.checkVersion("queries", queries.Version); err != nil {
			return err
		}
		if t.input("contexts").Dataset != t.input("queries").Dataset {
			return fmt.Errorf("같은 검색 입력의 데이터셋 코드 불일치")
		}
		if err := t.checkContexts(contexts); err != nil {
			return err
		}
		units = trialUnits(t.input("queries").Dataset, queries.Queries, "")
		if t.Manifest.Mode == "adversarial" {
			attacks, err := loadAttackSet(t.Inputs["attacks"], contexts, queries)
			if err != nil {
				return err
			}
			if err := t.checkVersion("attacks", attacks.Version); err != nil {
				return err
			}
			for _, sample := range attacks.Samples {
				if err := trialSafeCode(sample.SampleID); err != nil {
					return err
				}
				for _, path := range sample.ExpectedPaths {
					if err := trialSafeCode(path.PathID); err != nil {
						return err
					}
				}
			}
		}
	case "business":
		role = "business"
		if err := t.requireRoles(role); err != nil {
			return err
		}
		input, err := loadBusinessSet(t.Inputs[role])
		if err != nil {
			return err
		}
		version = input.Version
		for i := range input.Samples {
			code := fmt.Sprintf("sample-%d", i+1)
			units = append(units, manifestUnit{Dataset: t.input(role).Dataset, Query: code, Task: code, Type: "legacy_business", Independent: code})
		}
		if t.Manifest.Models.AgentVersion != input.Conditions.AgentVersion || t.Manifest.Models.AgentModel != input.Conditions.Model || *t.Manifest.Search.Budget != input.Conditions.ContextBudget {
			return fmt.Errorf("기존 업무 통제 불일치")
		}
	case "business_tasks":
		role = "business_tasks"
		if err := t.requireRoles(role); err != nil {
			return err
		}
		input, err := loadBusinessTasks(t.Inputs[role])
		if err != nil {
			return err
		}
		version = input.Version
		for _, task := range input.Tasks {
			units = append(units, manifestUnit{Dataset: t.input(role).Dataset, Query: task.Code, Task: task.Independent, Type: task.Type, Independent: task.Independent})
		}
		m, c := t.Manifest, input.Controls
		if m.Provenance != input.Provenance || m.Models.AgentVersion != c.Agent.Version || m.Models.AgentModel != c.Agent.Model || m.Models.PromptVersion != c.Agent.PromptFamily || *m.Search.Budget != *c.Budget || m.Execution.Preparation != c.Preparation || m.Execution.Cache != c.Cache || m.Execution.TimeoutMS != int(*c.TimeoutMS) || m.Cost.PricingVersion != input.Pricing.Version || m.Cost.Currency != input.Pricing.Currency || m.Cost.AsOf != input.Pricing.AsOf || m.Cost.Allocation != input.Pricing.Allocation {
			return fmt.Errorf("업무 과업의 모델/예산/준비/단가 판 불일치")
		}
	case "units":
		role = "unit_comparison"
		if err := t.requireRoles(role); err != nil {
			return err
		}
		input, err := loadUnitInput(t.Inputs[role])
		if err != nil {
			return err
		}
		version = input.Version
		for _, sample := range input.Samples {
			if err := t.checkContexts(contextSet{Contexts: sample.Sources}); err != nil {
				return err
			}
			for _, q := range sample.Queries {
				units = append(units, manifestUnit{Dataset: t.input(role).Dataset, Query: sample.Code + "." + q.ID, Task: sample.Code, Type: sample.Cohort, Independent: sample.Code})
			}
		}
		m, s := t.Manifest, input.Search
		if m.Provenance != input.Provenance || m.Models.AgentVersion != input.Agent.Version || m.Models.AgentModel != input.Agent.Model || m.Models.PromptVersion != input.Agent.PromptFamily || m.Cost.PricingVersion != input.Pricing || m.Cost.Currency != input.Currency || !t.sameSearch(s.Budget, s.MaxHops, s.MaxHopNodes, s.Execution, s.CandidateLimit, s.FoldThreshold, s.IndexTargets) || *m.Search.SemanticThreshold != s.SemanticThreshold || m.Models.Embedding != s.EmbeddingModel || !slices.Equal(m.Search.Stages, []string{s.Stage}) {
			return fmt.Errorf("검색 단위 통제 불일치")
		}
	case "embeddings":
		role = "embedding_comparison"
		if err := t.requireRoles(role); err != nil {
			return err
		}
		input, configs, err := loadEmbeddingInput(t.Inputs[role])
		if err != nil {
			return err
		}
		version = input.Version
		for _, sample := range input.Samples {
			if err := t.checkContexts(sample.Contexts); err != nil {
				return err
			}
			qs := slices.Clone(sample.Queries)
			for i := range qs {
				qs[i].IndependentUnit = sample.Unit
			}
			units = append(units, trialUnits(t.input(role).Dataset, qs, sample.Code+".")...)
		}
		s := input.Search
		if t.Manifest.Search.SemanticThreshold != nil || !slices.Equal(t.Manifest.Search.Stages, []string{"evidence"}) || !*t.Manifest.Search.GlobalFallback || !*t.Manifest.Search.EvidenceSelection {
			return fmt.Errorf("임베딩 비교의 조정 하한/고정 그래프 범위 불일치")
		}
		if t.Manifest.Provenance != input.Provenance || !t.sameSearch(s.Budget, s.MaxHops, s.MaxHopNodes, s.Execution, s.CandidateLimit, s.FoldThreshold, s.IndexTargets) || t.Manifest.Models.Embedding != configs[0].index.ModelID() || t.Manifest.Models.Dimension != configs[0].index.Dimension || t.Manifest.Models.VectorType != configs[0].index.VectorType {
			return fmt.Errorf("임베딩 통제 불일치")
		}
	case "originals":
		role = "original_reproduction"
		if err := t.requireRoles(role); err != nil {
			return err
		}
		input, _, err := loadOriginalInput(t.Inputs[role])
		if err != nil {
			return err
		}
		version = input.Version
		for _, sample := range input.Samples {
			units = append(units, manifestUnit{Dataset: t.input(role).Dataset, Query: sample.Code, Task: sample.Independent, Type: sample.Scenario, Independent: sample.Independent})
		}
		if t.Manifest.Provenance != input.Provenance || t.Manifest.Execution.RequestLimit != input.RequestLimit || t.Manifest.Execution.TimeoutMS < input.TimeoutMS {
			return fmt.Errorf("원본 조회 한도/출처 불일치")
		}
	case "continual":
		role = "continual_scenarios"
		if err := t.requireRoles(role); err != nil {
			return err
		}
		var inputs []trialContinualInput
		if err := t.readRole(role, &inputs); err != nil {
			return err
		}
		if !reflect.DeepEqual(inputs, trialContinualInputs()) {
			return fmt.Errorf("내장 지속 시나리오 불일치")
		}
		version = "continual-v1"
		for _, s := range inputs {
			if err := t.checkContexts(s.Contexts); err != nil {
				return err
			}
			units = append(units, manifestUnit{Dataset: t.input(role).Dataset, Query: s.Code, Task: s.Code, Type: s.Query.UseCase, Independent: s.Code})
		}
		if *t.Manifest.Search.Budget != continualBudget || t.Manifest.Search.IndexTargets != "all_layers" {
			return fmt.Errorf("지속 평가의 고정 예산/색인 대상 불일치")
		}
	default:
		return fmt.Errorf("측정 모드가 올바르지 않다")
	}
	if role != "" {
		if err := t.checkVersion(role, version); err != nil {
			return err
		}
	}
	return t.checkUnits(units)
}

func (t *manifestTrial) checkPrepared() error {
	if t.Manifest.Mode != "basic" {
		return fmt.Errorf("준비 입력은 그래프/색인 대상 통제 비교에만 쓴다")
	}
	if err := t.requireRoles("contexts", "queries", "preparation"); err != nil {
		return err
	}
	input, err := loadPreparedComparison(t.Private)
	if err != nil {
		return err
	}
	p := input.Preparation
	for _, role := range []string{"contexts", "queries", "preparation"} {
		i := t.input(role)
		if i.Version != preparationVersion || i.Dataset != p.Dataset || (p.Pin != nil && i.Commit != p.Pin.Commit) {
			return fmt.Errorf("준비 입력 판/commit 불일치")
		}
	}
	mapping := p.Mapping
	mapping.AnswerPolicy = "evaluation_only"
	if mapping != t.Manifest.Mapping || !reflect.DeepEqual(t.Manifest.Exclusions, p.Exclusions) {
		return fmt.Errorf("준비 매핑/제외 정책 불일치")
	}
	for _, dataset := range input.Datasets {
		if err := t.checkContexts(dataset.Contexts); err != nil {
			return err
		}
	}
	return t.checkUnits(p.Units)
}

func (t *manifestTrial) sameSearch(budget, hops, nodes int, execution string, limit int, fold float64, targets string) bool {
	s := t.Manifest.Search
	return *s.Budget == budget && *s.MaxHops == hops && *s.MaxHopNodes == nodes && s.ChannelExecution == execution && s.CandidateLimit == limit && *s.FoldThreshold == fold && s.IndexTargets == targets
}

func (t *manifestTrial) snapshotOriginals(original, snapshot string) error {
	seen := map[string]string{}
	var input originalInput
	raw, err := os.ReadFile(snapshot)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	for _, sample := range input.Samples {
		c := sample.Captured
		if c == nil {
			continue
		}
		if !filepath.IsLocal(c.File) || !manifestSHA256.MatchString(c.SHA256) {
			return fmt.Errorf("원본 snapshot 경계 오류")
		}
		if checksum, ok := seen[c.File]; ok {
			if checksum != c.SHA256 {
				return fmt.Errorf("원본 snapshot의 판이 충돌한다")
			}
			continue
		}
		seen[c.File] = c.SHA256
		dest := filepath.Join(t.Private, c.File)
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		if err := snapshotTrialInput(filepath.Join(filepath.Dir(original), c.File), dest, c.SHA256); err != nil {
			return err
		}
	}
	return nil
}

func (t *manifestTrial) arguments(prepared bool) ([]string, error) {
	m := t.Manifest
	s := m.Search
	for _, seed := range m.Execution.Seeds {
		if *seed.Supported {
			return nil, fmt.Errorf("실행기가 확인하지 못한 seed 지원을 선언할 수 없다")
		}
	}
	if !prepared {
		want := manifestMapping{Preprocessing: "direct-v1", Split: "direct-v1", Answers: "direct-v1", Evidence: "direct-v1", AnswerPolicy: "evaluation_only"}
		if m.Mapping != want || m.Exclusions.Policy != "direct-v1" || len(m.Exclusions.Items) > 0 {
			return nil, fmt.Errorf("직접 입력 매핑 판 불일치")
		}
	}
	if slices.Contains([]string{"basic", "adaptive", "consistency", "adversarial", "continual", "units", "embeddings"}, m.Mode) && (m.Execution.Preparation != "fresh_graph_no_explicit_warmup_shared_cache" || m.Execution.Cache != "shared_db_os_cache") {
		return nil, fmt.Errorf("실제 그래프 준비/공유 캐시 정책 불일치")
	}
	if m.Mode == "adversarial" && (!slices.Equal(s.Stages, []string{"relations"}) || !*s.GlobalFallback || *s.EvidenceSelection) {
		return nil, fmt.Errorf("적대적 검색의 고정 관계 범위 불일치")
	}
	if m.Mode == "consistency" && (!slices.Equal(s.Stages, []string{m.Consistency.Stage}) || !*s.GlobalFallback || *s.EvidenceSelection) {
		return nil, fmt.Errorf("일관성 검색 단계/전역 정책 불일치")
	}
	if m.Mode == "adaptive" && !*s.GlobalFallback {
		return nil, fmt.Errorf("적응형 전역 정책 불일치")
	}
	args := []string{"-out=" + filepath.Join(t.Private, "result.json")}
	for _, mode := range []struct{ mode, role, flag string }{{"business", "business", "business"}, {"business_tasks", "business_tasks", "business-tasks"}, {"units", "unit_comparison", "unit-comparison"}, {"embeddings", "embedding_comparison", "embedding-comparison"}, {"originals", "original_reproduction", "original-reproduction"}} {
		if m.Mode == mode.mode {
			if m.Mode == "units" {
				if _, err := t.checkedSettings(); err != nil {
					return nil, err
				}
			}
			return append(args, "-"+mode.flag+"="+t.Inputs[mode.role]), nil
		}
	}
	if _, err := t.checkedSettings(); err != nil {
		return nil, err
	}
	for _, v := range []struct {
		name  string
		value int
	}{{"repeat", m.Execution.Repeats}, {"max-hops", *s.MaxHops}, {"max-hop-nodes", *s.MaxHopNodes}} {
		args = append(args, "-"+v.name+"="+strconv.Itoa(v.value))
	}
	// 지속 평가의 예산은 고정값이며 입력 연결에서 manifest 선언과 대조했다.
	if m.Mode == "continual" {
		return append(args, "-continual"), nil
	}
	args = append(args, "-budget="+strconv.Itoa(*s.Budget))
	if prepared {
		args = append(args, "-prepared-input="+t.Private)
	} else {
		args = append(args, "-contexts="+t.Inputs["contexts"], "-queries="+t.Inputs["queries"])
	}
	switch m.Mode {
	case "basic":
		args = append(args, "-stages="+strings.Join(s.Stages, ","))
		if m.ComparisonAxis == "index_targets" {
			args = append(args, "-compare-index-targets")
		} else {
			args = append(args, "-index-targets="+s.IndexTargets)
		}
		variants, err := comparisonVariants(s.Stages, s.IndexTargets, m.ComparisonAxis == "index_targets")
		if err != nil {
			return nil, err
		}
		last := variants[len(variants)-1]
		if last.GlobalFallback != *s.GlobalFallback || last.EvidenceSelection != *s.EvidenceSelection {
			return nil, fmt.Errorf("그래프 단계의 전역/근거 정책 불일치")
		}
	case "adaptive":
		if len(s.Stages) != 1 {
			return nil, fmt.Errorf("적응형 비교에는 단계 하나가 필요하다")
		}
		args = append(args, "-adaptive", "-adaptive-stage="+s.Stages[0], "-adaptive-evidence="+strconv.FormatBool(*s.EvidenceSelection), "-direct-thresholds="+trialFloats(s.DirectThresholds), "-direct-margins="+trialFloats(s.DirectMargins), "-index-targets="+s.IndexTargets)
	case "consistency":
		c := m.Consistency
		args = append(args, "-consistency", "-consistency-stage="+c.Stage, "-consistency-requests="+strconv.Itoa(c.Requests), "-consistency-load-requests="+strconv.Itoa(c.LoadRequests), "-consistency-load="+trialInts(c.Concurrency), "-consistency-write-pauses="+trialInts(c.WritePauses), "-index-targets="+s.IndexTargets)
	case "adversarial":
		args = append(args, "-adversarial", "-attacks="+t.Inputs["attacks"], "-index-targets="+s.IndexTargets)
	default:
		return nil, fmt.Errorf("측정 모드가 올바르지 않다")
	}
	return args, nil
}

func (t *manifestTrial) checkedSettings() (settings, error) {
	cfg, err := loadSettings()
	if err != nil {
		return cfg, err
	}
	m := t.Manifest
	s := m.Search
	if !t.sameSearch(*s.Budget, *s.MaxHops, *s.MaxHopNodes, string(cfg.execution), cfg.candidateLimit, cfg.foldThreshold, s.IndexTargets) || *s.SemanticThreshold != cfg.semanticThreshold || m.Models.Embedding != cfg.index.ModelID() || m.Models.Dimension != cfg.index.Dimension || m.Models.VectorType != cfg.index.VectorType {
		return cfg, fmt.Errorf("실제 임베딩/검색 환경과 manifest 불일치")
	}
	return cfg, nil
}
func trialFloats(values []float64) string {
	var parts []string
	for _, v := range values {
		parts = append(parts, strconv.FormatFloat(v, 'g', -1, 64))
	}
	return strings.Join(parts, ",")
}
func trialInts(values []int) string {
	var parts []string
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ",")
}
