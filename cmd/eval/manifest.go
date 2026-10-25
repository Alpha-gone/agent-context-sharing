package main

import (
	json "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const initialEvaluationRepeats = 10
const maxManifestBytes = 4 << 20

// executionManifest는 데이터 준비와 비교 실행에 앞서 고정하는 통제 명세다.
// 검사 성공은 파일 checksum 대조나 실제 측정 완료를 뜻하지 않는다.
type executionManifest struct {
	SchemaVersion int                  `json:"schema_version"`
	TrialID       string               `json:"trial_id"`
	Mode          string               `json:"mode"`
	Inputs        []manifestInput      `json:"inputs"`
	Mapping       manifestMapping      `json:"mapping"`
	Units         []manifestUnit       `json:"units"`
	Models        manifestModels       `json:"models"`
	Search        manifestSearch       `json:"search"`
	Execution     manifestExecution    `json:"execution"`
	Exclusions    manifestExclusions   `json:"exclusions"`
	Cost          manifestCost         `json:"cost"`
	Conversion    *manifestConversion  `json:"conversion,omitzero"`
	Consistency   *manifestConsistency `json:"consistency,omitzero"`
}

type manifestInput struct {
	Code    string `json:"code"`
	Dataset string `json:"dataset"`
	Role    string `json:"role"`
	Version string `json:"version"`
	Commit  string `json:"repository_commit"`
	SHA256  string `json:"sha256"`
}

type manifestMapping struct {
	Preprocessing string `json:"preprocessing_version"`
	Split         string `json:"split_version"`
	Answers       string `json:"answer_mapping_version"`
	Evidence      string `json:"evidence_mapping_version"`
	AnswerPolicy  string `json:"annotation_policy"`
}

type manifestUnit struct {
	Dataset      string `json:"dataset"`
	Query        string `json:"query_code"`
	Conversation string `json:"conversation_code,omitzero"`
	Task         string `json:"task_code,omitzero"`
	Type         string `json:"type"`
	Independent  string `json:"independent_unit"`
}

type manifestModels struct {
	AgentVersion  string `json:"agent_version"`
	AgentModel    string `json:"agent_model"`
	PromptVersion string `json:"prompt_version"`
	Embedding     string `json:"embedding_model"`
	Dimension     int    `json:"dimension"`
	VectorType    string `json:"vector_type"`
	Compression   string `json:"compression"`
}

type manifestSearch struct {
	Stages            []string  `json:"stages"`
	IndexTargets      string    `json:"index_targets"`
	ChannelExecution  string    `json:"channel_execution"`
	EvidenceSelection *bool     `json:"evidence_path_selection"`
	GlobalFallback    *bool     `json:"global_fallback"`
	Budget            *int      `json:"budget"`
	MaxHops           *int      `json:"max_hops"`
	MaxHopNodes       *int      `json:"max_hop_nodes"`
	CandidateLimit    int       `json:"channel_candidate_limit"`
	SemanticThreshold *float64  `json:"semantic_similarity_threshold"`
	FoldThreshold     *float64  `json:"fold_threshold"`
	DirectThresholds  []float64 `json:"direct_thresholds,omitzero"`
	DirectMargins     []float64 `json:"direct_margins,omitzero"`
}

type manifestExecution struct {
	Repeats         int            `json:"repeats"`
	Order           string         `json:"order"`
	RequestLimit    int            `json:"request_limit"`
	IndexRoundLimit int            `json:"index_round_limit"`
	TimeoutMS       int            `json:"timeout_ms"`
	Preparation     string         `json:"preparation_version"`
	Cache           string         `json:"cache_policy"`
	Seeds           []manifestSeed `json:"seeds"`
}

type manifestSeed struct {
	Component string `json:"component"`
	Supported *bool  `json:"supported"`
	Value     *int64 `json:"value,omitzero"`
}

type manifestExclusions struct {
	Policy string              `json:"policy_version"`
	Total  int                 `json:"total_queries"`
	Items  []manifestExclusion `json:"items"`
}

type manifestExclusion struct {
	Dataset string `json:"dataset"`
	Query   string `json:"query_code"`
	Reason  string `json:"reason_code"`
}

type manifestCost struct {
	PricingVersion string             `json:"pricing_version"`
	AsOf           string             `json:"as_of"`
	Currency       string             `json:"currency"`
	Allocation     string             `json:"allocation_version"`
	Items          []manifestCostItem `json:"items"`
}

type manifestCostItem struct {
	Code   string   `json:"code"`
	Status string   `json:"status"`
	Value  *float64 `json:"value,omitzero"`
}

type manifestConversion struct {
	Format  string `json:"format"`
	UseCase string `json:"use_case,omitzero"`
}

type manifestConsistency struct {
	Stage        string `json:"stage"`
	Requests     int    `json:"requests"`
	LoadRequests int    `json:"load_requests"`
	Concurrency  []int  `json:"concurrency"`
	WritePauses  []int  `json:"write_pauses_ms"`
}

var manifestCode = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var manifestModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,255}$`)
var manifestSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var manifestCommit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func loadManifest(path string) (executionManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return executionManifest{}, fmt.Errorf("manifest 읽기: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return executionManifest{}, fmt.Errorf("manifest 읽기: %w", err)
	}
	if len(raw) > maxManifestBytes {
		return executionManifest{}, fmt.Errorf("manifest는 4 MiB 이하여야 한다")
	}
	var manifest executionManifest
	if err := json.Unmarshal(raw, &manifest, json.RejectUnknownMembers(true)); err != nil {
		// 알 수 없는 필드나 잘못된 JSON의 원문을 오류·결과에 복사하지 않는다.
		return executionManifest{}, fmt.Errorf("manifest JSON 형식 또는 허용 필드가 올바르지 않다")
	}
	if err := manifest.validate(); err != nil {
		return executionManifest{}, err
	}
	return manifest, nil
}

func (m executionManifest) validate() error {
	invalid := func(field string) error {
		return fmt.Errorf("manifest %s가 누락되었거나 올바르지 않다", field)
	}
	if m.SchemaVersion != 1 || !manifestCode.MatchString(m.TrialID) {
		return invalid("schema_version/trial_id")
	}
	if !slices.Contains([]string{"basic", "convert", "business", "continual", "adaptive", "consistency", "adversarial", "units", "originals", "embeddings"}, m.Mode) {
		return invalid("mode")
	}
	if (m.Mode == "convert") != (m.Conversion != nil) || (m.Mode == "consistency") != (m.Consistency != nil) {
		return invalid("모드별 옵션")
	}
	if m.Conversion != nil {
		if err := validateConversion(m.Conversion.Format, m.Conversion.UseCase); err != nil {
			return err
		}
	}
	if len(m.Inputs) == 0 || len(m.Units) == 0 {
		return invalid("inputs/units")
	}
	datasets, inputs, roles := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, input := range m.Inputs {
		if !manifestCode.MatchString(input.Code) || !manifestCode.MatchString(input.Dataset) || !manifestCode.MatchString(input.Version) || inputs[input.Code] ||
			!manifestCommit.MatchString(input.Commit) || !manifestSHA256.MatchString(input.SHA256) ||
			!slices.Contains([]string{"source", "contexts", "queries", "answers", "evidence", "attacks", "business", "unit_comparison", "original_reproduction", "embedding_comparison"}, input.Role) {
			return invalid("inputs")
		}
		inputs[input.Code], datasets[input.Dataset] = true, true
		roles[input.Dataset+"/"+input.Role] = true
	}
	for _, version := range []string{m.Mapping.Preprocessing, m.Mapping.Split, m.Mapping.Answers, m.Mapping.Evidence} {
		if !manifestCode.MatchString(version) {
			return invalid("mapping")
		}
	}
	if m.Mapping.AnswerPolicy != "evaluation_only" {
		return invalid("mapping.annotation_policy")
	}
	queries, groups, independent := map[string]bool{}, map[string]string{}, map[string]string{}
	for _, unit := range m.Units {
		query := unit.Dataset + "/" + unit.Query
		if !datasets[unit.Dataset] || !manifestCode.MatchString(unit.Query) || !manifestCode.MatchString(unit.Type) || !manifestCode.MatchString(unit.Independent) || queries[query] ||
			(unit.Conversation == "") == (unit.Task == "") {
			return invalid("units")
		}
		switch m.Mode {
		case "convert":
			if !roles[unit.Dataset+"/source"] {
				return invalid("inputs source checksum")
			}
		case "business":
			if !roles[unit.Dataset+"/business"] {
				return invalid("inputs business checksum")
			}
		case "units":
			if !roles[unit.Dataset+"/unit_comparison"] {
				return invalid("inputs unit comparison checksum")
			}
		case "embeddings":
			if !roles[unit.Dataset+"/embedding_comparison"] {
				return invalid("inputs embedding comparison checksum")
			}
		case "originals":
			if !roles[unit.Dataset+"/original_reproduction"] {
				return invalid("inputs original reproduction checksum")
			}
		default:
			if !roles[unit.Dataset+"/contexts"] || !roles[unit.Dataset+"/queries"] || (m.Mode == "adversarial" && !roles[unit.Dataset+"/attacks"]) {
				return invalid("inputs contexts/queries/attacks checksums")
			}
		}
		kind, code := "conversation", unit.Conversation
		if unit.Task != "" {
			kind, code = "task", unit.Task
		}
		if !manifestCode.MatchString(code) {
			return invalid("units conversation/task")
		}
		group, id := unit.Dataset+"/"+kind+"/"+code, unit.Dataset+"/"+unit.Independent
		if (groups[group] != "" && groups[group] != id) || (independent[id] != "" && independent[id] != group) {
			return invalid("units.independent_unit")
		}
		queries[query], groups[group], independent[id] = true, id, group
	}
	for _, code := range []string{m.Models.AgentVersion, m.Models.PromptVersion, m.Models.Compression} {
		if !manifestCode.MatchString(code) {
			return invalid("models")
		}
	}
	for _, model := range []string{m.Models.AgentModel, m.Models.Embedding} {
		if !manifestModel.MatchString(model) || strings.Contains(model, "://") {
			return invalid("models model IDs")
		}
	}
	if m.Models.Dimension < 1 || !slices.Contains([]string{"vector", "halfvec"}, m.Models.VectorType) || (m.Models.VectorType == "vector" && m.Models.Dimension > 2000) || (m.Models.VectorType == "halfvec" && m.Models.Dimension > 4000) {
		return invalid("models dimension/vector_type")
	}
	s := m.Search
	if parsed, err := parseStages(strings.Join(s.Stages, ",")); err != nil || !slices.Equal(parsed, s.Stages) {
		return invalid("search.stages")
	}
	if !slices.Contains([]string{"all_layers", "without_source"}, s.IndexTargets) || !slices.Contains([]string{"parallel", "sequential"}, s.ChannelExecution) ||
		s.EvidenceSelection == nil || s.GlobalFallback == nil || s.Budget == nil || *s.Budget < 0 || s.MaxHops == nil || *s.MaxHops < 0 || s.MaxHopNodes == nil || *s.MaxHopNodes < 0 || s.CandidateLimit < 1 ||
		!manifestRatio(s.SemanticThreshold) || !manifestRatio(s.FoldThreshold) || *s.FoldThreshold == 0 {
		return invalid("search")
	}
	if m.Mode == "adaptive" {
		if len(s.DirectThresholds) == 0 || len(s.DirectMargins) == 0 {
			return invalid("search adaptive thresholds")
		}
		for _, value := range append(slices.Clone(s.DirectThresholds), s.DirectMargins...) {
			if !manifestRatio(&value) {
				return invalid("search adaptive thresholds")
			}
		}
	} else if len(s.DirectThresholds) > 0 || len(s.DirectMargins) > 0 {
		return invalid("search adaptive mode")
	}
	if m.Consistency != nil {
		if err := validateConsistencyOptions(*m.Consistency, s.CandidateLimit); err != nil {
			return err
		}
	}
	e := m.Execution
	if e.Repeats != initialEvaluationRepeats || e.Order != "round_robin" || e.RequestLimit < 1 || e.IndexRoundLimit < 1 || e.TimeoutMS < 1 ||
		!manifestCode.MatchString(e.Preparation) || !manifestCode.MatchString(e.Cache) || len(e.Seeds) == 0 {
		return invalid("execution")
	}
	seedComponents := map[string]bool{}
	for _, seed := range e.Seeds {
		if !manifestCode.MatchString(seed.Component) || seedComponents[seed.Component] || seed.Supported == nil || *seed.Supported != (seed.Value != nil) {
			return invalid("execution.seeds")
		}
		seedComponents[seed.Component] = true
	}
	if !manifestCode.MatchString(m.Exclusions.Policy) || m.Exclusions.Items == nil || m.Exclusions.Total != len(m.Units)+len(m.Exclusions.Items) {
		return invalid("exclusions")
	}
	for _, item := range m.Exclusions.Items {
		key := item.Dataset + "/" + item.Query
		if !datasets[item.Dataset] || !manifestCode.MatchString(item.Query) || !manifestCode.MatchString(item.Reason) || queries[key] {
			return invalid("exclusions.items")
		}
		queries[key] = true
	}
	if !manifestCode.MatchString(m.Cost.PricingVersion) || !manifestCode.MatchString(m.Cost.Allocation) || len(m.Cost.Currency) != 3 || strings.Trim(m.Cost.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || len(m.Cost.Items) == 0 {
		return invalid("cost")
	}
	if _, err := time.Parse(time.DateOnly, m.Cost.AsOf); err != nil {
		return invalid("cost.as_of")
	}
	costCodes := map[string]bool{}
	allCosts := []string{"agent", "embedding", "summary", "search", "storage"}
	for _, item := range m.Cost.Items {
		if !slices.Contains(allCosts, item.Code) || costCodes[item.Code] || !slices.Contains([]string{"measured", "unmeasured", "not_applicable"}, item.Status) ||
			(item.Status == "measured") != (item.Value != nil) || (item.Value != nil && (math.IsNaN(*item.Value) || math.IsInf(*item.Value, 0) || *item.Value < 0)) {
			return invalid("cost.items")
		}
		costCodes[item.Code] = true
	}
	if len(costCodes) != len(allCosts) {
		return invalid("cost.items coverage")
	}
	return nil
}

func manifestRatio(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && *value >= 0 && *value <= 1
}

func validateConversion(format, useCase string) error {
	if !slices.Contains([]string{convertHippoRAG, convertOwn, convertOwnGlobal, convertOwnGlobalAuto, convertOwnAdversarial}, format) {
		return fmt.Errorf("변환 형식을 알 수 없다")
	}
	if useCase != "" && (!slices.Contains(useCases, useCase) || format == convertOwnAdversarial) {
		return fmt.Errorf("변환 사용 사례가 올바르지 않다: own-adversarial에는 필터를 사용할 수 없다")
	}
	return nil
}

func validateConsistencyOptions(options manifestConsistency, candidateLimit int) error {
	if err := validateConsistencyCandidateLimit(candidateLimit); err != nil {
		return err
	}
	if !slices.Contains([]string{"references", "relations", "global"}, options.Stage) || options.Requests < 1 || options.LoadRequests < 1 || len(options.Concurrency) == 0 || len(options.WritePauses) == 0 {
		return fmt.Errorf("일관성 stage·요청 수·부하·쓰기 간격이 올바르지 않다")
	}
	for _, value := range options.Concurrency {
		if value < 1 {
			return fmt.Errorf("일관성 concurrency는 양수여야 한다")
		}
	}
	for _, value := range options.WritePauses {
		if value < 0 {
			return fmt.Errorf("일관성 write_pauses_ms는 0 이상이어야 한다")
		}
	}
	return nil
}
