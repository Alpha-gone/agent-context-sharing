package main

import (
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"strings"

	"agent_context_sharing/internal/search"
)

// 원천과 질의는 표본에 한 번만 선언한다. 두 조건이 바꿀 수 있는 것은 파생과
// 그 경계에 대응하는 정답 매핑뿐이며, 판정 주석은 그래프에 적재하지 않는다.
type unitInput struct {
	Version    string       `json:"version"`
	Provenance string       `json:"provenance"`
	Agent      unitAgent    `json:"agent"`
	Judge      unitJudge    `json:"judge"`
	Pricing    string       `json:"pricing_version"`
	Currency   string       `json:"currency"`
	Search     unitSearch   `json:"search"`
	Policies   []unitPolicy `json:"policies"`
	Samples    []unitSample `json:"samples"`
}

type unitSearch struct {
	Stage             string  `json:"stage"`
	Budget            int     `json:"budget"`
	MaxHops           int     `json:"max_hops"`
	MaxHopNodes       int     `json:"max_hop_nodes"`
	IndexTargets      string  `json:"index_targets"`
	EmbeddingModel    string  `json:"embedding_model"`
	Execution         string  `json:"channel_execution"`
	CandidateLimit    int     `json:"candidate_limit"`
	SemanticThreshold float64 `json:"semantic_threshold"`
	FoldThreshold     float64 `json:"fold_threshold"`
}

type unitAgent struct {
	Version      string `json:"version"`
	Model        string `json:"model"`
	PromptFamily string `json:"prompt_family"`
}

type unitJudge struct {
	Version string `json:"version"`
	Model   string `json:"model"`
	Prompt  string `json:"prompt_version"`
}

type unitPolicy struct {
	Name     string `json:"name"`
	Boundary string `json:"boundary_version"`
}

type unitSample struct {
	Code     string        `json:"code"`
	Cohort   string        `json:"cohort"`
	Sources  []contextSpec `json:"sources"`
	Elements []unitElement `json:"required_elements"`
	Queries  []querySpec   `json:"queries"`
	Variants []unitVariant `json:"variants"`
}

type unitElement struct {
	Code     string   `json:"code"`
	Kind     string   `json:"kind"`
	Meaning  string   `json:"meaning"`
	Evidence []string `json:"source_evidence"`
}

type unitVariant struct {
	Policy  string        `json:"policy"`
	Derived []contextSpec `json:"derived"`
	Reviews []unitReview  `json:"reviews"`
	Answers []unitAnswer  `json:"answers"`
	Tasks   []unitTask    `json:"task_runs"`
}

type unitReview struct {
	Key       string       `json:"key"`
	SHA256    string       `json:"input_sha256"`
	Required  []string     `json:"required_elements"`
	Preserved []string     `json:"preserved_elements"`
	Human     *humanReview `json:"human_review,omitzero"`
}

type humanReview struct {
	Reviewer  string   `json:"reviewer_code"`
	Version   string   `json:"review_version"`
	Preserved []string `json:"preserved_elements"`
}

type unitAnswer struct {
	Query    string     `json:"query"`
	Answers  []string   `json:"answers"`
	Required []string   `json:"required"`
	Paths    [][]string `json:"paths"`
}

// 외부 에이전트의 과업 실행 기록이다. 검색 정답 반환만으로 과업 성공을 대체하지
// 않으며, 비용은 모든 항목이 포함된 총액만 받는다. nil은 미측정이다.
type unitTask struct {
	Repeat    int      `json:"repeat"`
	InputHash string   `json:"input_sha256"`
	Trace     string   `json:"trace_version"`
	Success   *bool    `json:"success,omitzero"`
	TotalCost *float64 `json:"total_cost,omitzero"`
}

func unitReviewHash(judge unitJudge, sample unitSample, spec contextSpec, required []string) string {
	raw, _ := json.Marshal(struct {
		Judge    unitJudge
		Sources  []contextSpec
		Elements []unitElement
		Derived  contextSpec
		Required []string
	}{judge, sample.Sources, sample.Elements, spec, required})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func unitTaskHash(input unitInput, sample unitSample, variant unitVariant) string {
	raw, _ := json.Marshal(struct {
		Agent   unitAgent
		Sources []contextSpec
		Queries []querySpec
		Derived []contextSpec
		Policy  unitPolicy
		Search  unitSearch
		Answers []unitAnswer
		Sample  string
	}{input.Agent, sample.Sources, sample.Queries, variant.Derived, input.Policies[slices.IndexFunc(input.Policies, func(p unitPolicy) bool { return p.Name == variant.Policy })], input.Search, variant.Answers, sample.Code})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func loadUnitInput(path string) (unitInput, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return unitInput{}, fmt.Errorf("검색 단위 입력 읽기: %w", err)
	}
	var input unitInput
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)); err != nil {
		return unitInput{}, fmt.Errorf("검색 단위 JSON 형식 또는 허용 필드가 올바르지 않다")
	}
	if err := validateUnitInput(input); err != nil {
		return unitInput{}, err
	}
	return input, nil
}

func validateUnitInput(input unitInput) error {
	invalid := func(field string) error {
		return fmt.Errorf("검색 단위 %s가 누락되었거나 올바르지 않다", field)
	}
	for _, code := range []string{input.Version, input.Agent.Version, input.Agent.PromptFamily, input.Judge.Version, input.Judge.Prompt, input.Pricing} {
		if !manifestCode.MatchString(code) {
			return invalid("판")
		}
	}
	for _, model := range []string{input.Agent.Model, input.Judge.Model} {
		if !manifestModel.MatchString(model) || strings.Contains(model, "://") {
			return invalid("모델")
		}
	}
	s := input.Search
	if _, err := comparisonVariants([]string{s.Stage}, s.IndexTargets, false); err != nil {
		return invalid("검색 단계")
	}
	if s.Budget < 0 || s.MaxHops < 0 || s.MaxHopNodes < 0 || !manifestModel.MatchString(s.EmbeddingModel) || !slices.Contains([]string{string(search.ExecutionParallel), string(search.ExecutionSequential)}, s.Execution) || s.CandidateLimit < 1 || !manifestRatio(&s.SemanticThreshold) || !manifestRatio(&s.FoldThreshold) || s.FoldThreshold == 0 {
		return invalid("검색 통제")
	}
	if !slices.Contains([]string{"synthetic", "agent_trace"}, input.Provenance) || len(input.Currency) != 3 || len(input.Policies) != 2 || len(input.Samples) == 0 {
		return invalid("통제 조건")
	}
	for _, r := range input.Currency {
		if r < 'A' || r > 'Z' {
			return invalid("통화")
		}
	}
	if input.Policies[0].Name != "current" || input.Policies[1].Name != "semantic" || input.Policies[0].Boundary == input.Policies[1].Boundary {
		return invalid("경계 정책")
	}
	for _, policy := range input.Policies {
		if !manifestCode.MatchString(policy.Boundary) {
			return invalid("경계 판")
		}
	}
	seen := map[string]bool{}
	for _, sample := range input.Samples {
		if !manifestCode.MatchString(sample.Code) || seen[sample.Code] || !slices.Contains([]string{"conversation", "code_document"}, sample.Cohort) || len(sample.Sources) == 0 || len(sample.Elements) == 0 || len(sample.Queries) == 0 || len(sample.Variants) != 2 {
			return invalid("표본")
		}
		seen[sample.Code] = true
		sources, elements := map[string]bool{}, map[string]unitElement{}
		for _, source := range sample.Sources {
			if !manifestCode.MatchString(source.Key) || sources[source.Key] || source.Layer != "source" || source.Source == nil || source.Source.OccurredAt.IsZero() {
				return invalid("원천")
			}
			if (sample.Cohort == "conversation") != (source.Source.Channel == "conversation") {
				return invalid("코드·문서와 대화 분리")
			}
			sources[source.Key] = true
		}
		for _, element := range sample.Elements {
			if !manifestCode.MatchString(element.Code) || elements[element.Code].Code != "" || element.Meaning == "" || len(element.Evidence) == 0 || !slices.Contains([]string{"subject", "condition", "exception", "negation", "reference", "multi_turn_decision"}, element.Kind) {
				return invalid("필수 의미 요소")
			}
			if !uniqueUnitCodes(element.Evidence, sources) {
				return invalid("의미 요소 근거")
			}
			elements[element.Code] = element
		}
		for i, variant := range sample.Variants {
			if variant.Policy != input.Policies[i].Name || len(variant.Derived) == 0 || len(variant.Derived) != len(variant.Reviews) || len(variant.Answers) != len(sample.Queries) {
				return invalid("조건")
			}
			contexts := contextSet{Version: input.Version + "-" + sample.Code + "-" + variant.Policy, Contexts: append(slices.Clone(sample.Sources), variant.Derived...)}
			if _, err := validateContextSet(contexts, "검색 단위"); err != nil {
				return invalid("컨텍스트")
			}
			covered := map[string]bool{}
			for j, derived := range variant.Derived {
				if !manifestCode.MatchString(derived.Key) || derived.Layer != "derived" || !uniqueUnitCodes(derived.Derived.DerivedFrom, sources) {
					return invalid("파생 근거")
				}
				metadata, base := *derived.Derived, *sample.Variants[0].Derived[0].Derived
				metadata.DerivedFrom, base.DerivedFrom = nil, nil
				actual, _ := json.Marshal(metadata)
				expected, _ := json.Marshal(base)
				if string(actual) != string(expected) {
					return invalid("파생 경계 외 속성 변경")
				}
				review := variant.Reviews[j]
				if review.Key != derived.Key || len(review.Required) == 0 || review.Preserved == nil || review.SHA256 != unitReviewHash(input.Judge, sample, derived, review.Required) {
					return invalid("완전성 검토 연결")
				}
				required := map[string]bool{}
				for _, key := range review.Required {
					if elements[key].Code == "" || required[key] {
						return invalid("완전성 요소")
					}
					required[key], covered[key] = true, true
				}
				if !uniqueUnitCodes(review.Preserved, required) {
					return invalid("자동 판정")
				}
				if human := review.Human; human != nil && (!manifestCode.MatchString(human.Reviewer) || !manifestCode.MatchString(human.Version) || human.Preserved == nil || !uniqueUnitCodes(human.Preserved, required)) {
					return invalid("사람 검토")
				}
			}
			if len(covered) != len(elements) {
				return invalid("의미 요소 전체 범위")
			}
			queries := unitQueries(input, sample, variant)
			for j, query := range sample.Queries {
				if !manifestCode.MatchString(query.ID) || query.IndependentUnit != "" || query.DatasetType != "" || query.AsOf == nil || (query.Scope != "" && query.Scope != "auto") || len(query.Answers)+len(query.Required)+len(query.Paths) != 0 || variant.Answers[j].Query != query.ID {
					return invalid("공유 질의와 정답 매핑")
				}
			}
			if _, err := validateQuerySet(queries, contexts, "검색 단위"); err != nil {
				return invalid("질의")
			}
			if len(variant.Tasks) != initialEvaluationRepeats {
				return invalid("과업 10회 기록")
			}
			for j, task := range variant.Tasks {
				if task.Repeat != j+1 || task.InputHash != unitTaskHash(input, sample, variant) || !manifestCode.MatchString(task.Trace) || (task.TotalCost != nil && !validMetric(*task.TotalCost)) {
					return invalid("과업 기록")
				}
			}
		}
	}
	return nil
}

func uniqueUnitCodes(values []string, known map[string]bool) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if !known[value] || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func unitQueries(input unitInput, sample unitSample, variant unitVariant) querySet {
	queries := querySet{Version: input.Version, Queries: slices.Clone(sample.Queries)}
	for i := range queries.Queries {
		q, answer := &queries.Queries[i], variant.Answers[i]
		q.ID, q.IndependentUnit, q.DatasetType = sample.Code+"/"+q.ID, sample.Code, sample.Cohort
		q.Answers, q.Required, q.Paths = answer.Answers, answer.Required, answer.Paths
	}
	return queries
}
