package main

import (
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// 기존 네 지표 입력과 독립된, 외부 통제 실행의 집계 전용 계약이다.
// 본문·관찰 이력·개인 식별자는 받지 않으며 해시는 수입한 기록의 결합을 검사한다.
type businessTaskInput struct {
	SchemaVersion int                     `json:"schema_version"`
	Version       string                  `json:"version"`
	Provenance    string                  `json:"provenance"`
	Experiment    string                  `json:"experiment"`
	Controls      businessTaskControls    `json:"controls"`
	Pricing       businessPricing         `json:"pricing"`
	Conditions    []businessTaskCondition `json:"conditions"`
	Tasks         []businessTask          `json:"tasks"`
}

type businessTaskControls struct {
	Agent       unitAgent `json:"agent"`
	Tools       string    `json:"tools_version"`
	Success     string    `json:"success_criteria_version"`
	Preparation string    `json:"preparation_policy"`
	Cache       string    `json:"cache_policy"`
	TimeoutMS   *int64    `json:"timeout_ms"`
	MaxTurns    *int      `json:"max_turns"`
	Budget      *int      `json:"context_budget_chars"`
}

type businessTaskCondition struct {
	Code       string `json:"code"`
	Lookup     *bool  `json:"lookup"`
	Irrelevant *bool  `json:"irrelevant"`
	Reduction  string `json:"reduction"`
}

type businessTask struct {
	Code            string            `json:"code"`
	Independent     string            `json:"independent_unit"`
	Type            string            `json:"type"`
	InputHash       string            `json:"input_sha256"`
	ObservationHash string            `json:"observation_sha256"`
	Runs            []businessTaskRun `json:"runs"`
}

type businessTaskRun struct {
	Condition         string             `json:"condition"`
	Repeat            int                `json:"repeat"`
	Order             int                `json:"order"`
	Trace             string             `json:"trace_version"`
	InputHash         string             `json:"input_sha256"`
	Status            string             `json:"status"`
	ElapsedMS         *int64             `json:"elapsed_ms"`
	Turns             *int               `json:"model_turns"`
	Tools             *int               `json:"tool_calls"`
	Interventions     *int               `json:"user_interventions"`
	EvidenceRefetches *int               `json:"evidence_refetches"`
	EvidenceAvailable *bool              `json:"evidence_available"`
	Costs             []businessCostItem `json:"costs"`
	PreparationMS     *int64             `json:"preparation_ms,omitzero"`
	ReindexMS         *int64             `json:"reindex_ms,omitzero"`
	IndexState        string             `json:"index_state"`
}

func businessTaskHash(input businessTaskInput, task businessTask, condition businessTaskCondition) string {
	raw, _ := json.Marshal(struct {
		Version                                      string
		Experiment                                   string
		Controls                                     businessTaskControls
		Pricing                                      businessPricing
		Code, Independent, Type, Input, Observations string
		Condition                                    businessTaskCondition
	}{input.Version, input.Experiment, input.Controls, input.Pricing, task.Code, task.Independent, task.Type, task.InputHash, task.ObservationHash, condition}, json.Deterministic(true))
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func loadBusinessTasks(path string) (businessTaskInput, error) {
	f, err := os.Open(path)
	if err != nil {
		return businessTaskInput{}, fmt.Errorf("업무 과업 입력을 읽을 수 없다")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil || len(raw) > 32<<20 {
		return businessTaskInput{}, fmt.Errorf("업무 과업 입력은 읽을 수 있는 32 MiB 이하 파일이어야 한다")
	}
	var input businessTaskInput
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)); err != nil {
		return input, fmt.Errorf("업무 과업 JSON 형식 또는 허용 필드가 올바르지 않다")
	}
	return input, validateBusinessTasks(input)
}

func validateBusinessTasks(input businessTaskInput) error {
	invalid := func(field string) error {
		return fmt.Errorf("업무 과업의 %s 항목이 누락되었거나 올바르지 않다", field)
	}
	if input.SchemaVersion != 1 || !manifestCode.MatchString(input.Version) || !slices.Contains([]string{"synthetic", "agent_trace"}, input.Provenance) {
		return invalid("판/출처")
	}
	c := input.Controls
	for _, code := range []string{c.Agent.Version, c.Agent.PromptFamily, c.Tools, c.Success, c.Preparation, c.Cache} {
		if !manifestCode.MatchString(code) {
			return invalid("통제 판")
		}
	}
	if !manifestModel.MatchString(c.Agent.Model) || strings.Contains(c.Agent.Model, "://") || c.TimeoutMS == nil || *c.TimeoutMS < 1 || *c.TimeoutMS > 86400000 || c.MaxTurns == nil || *c.MaxTurns < 1 || c.Budget == nil || *c.Budget < 0 {
		return invalid("모델/실행 한도/문자 예산")
	}
	if err := validateBusinessPricing(input.Pricing); err != nil {
		return err
	}
	names := businessConditionNames(input.Experiment)
	if len(names) == 0 || len(input.Conditions) != len(names) || len(input.Tasks) == 0 {
		return invalid("비교 조건")
	}
	for i, condition := range input.Conditions {
		if condition.Code != names[i] || condition.Lookup == nil || condition.Irrelevant == nil {
			return invalid("조건 순서/조회/무관 컨텍스트")
		}
		lookup, irrelevant, reduction := true, false, "original"
		switch input.Experiment {
		case "lookup":
			lookup = i == 1
		case "irrelevant":
			irrelevant = i == 1
		case "reduction":
			reduction = names[i]
		}
		if *condition.Lookup != lookup || *condition.Irrelevant != irrelevant || condition.Reduction != reduction {
			return invalid("독립 통제")
		}
	}
	seen, unitTypes, inputUnits := map[string]bool{}, map[string]string{}, map[string]string{}
	for _, task := range input.Tasks {
		if !manifestCode.MatchString(task.Code) || seen[task.Code] || !manifestCode.MatchString(task.Independent) || !manifestSHA256.MatchString(task.InputHash) || !manifestSHA256.MatchString(task.ObservationHash) || !slices.Contains([]string{"resume", "handoff", "fact", "multi_evidence"}, task.Type) {
			return invalid("과업 식별/해시/유형")
		}
		seen[task.Code] = true
		if previous := unitTypes[task.Independent]; previous != "" && previous != task.Type {
			return invalid("독립 단위 유형")
		}
		unitTypes[task.Independent] = task.Type
		if unit := inputUnits[task.InputHash]; unit != "" && unit != task.Independent {
			return invalid("동일 과업의 독립 단위 분리")
		}
		inputUnits[task.InputHash] = task.Independent
		if len(task.Runs) != len(names)*initialEvaluationRepeats {
			return invalid("조건별 10회")
		}
		runs := map[string]bool{}
		states := map[int]string{}
		for _, run := range task.Runs {
			i := slices.Index(names, run.Condition)
			key := fmt.Sprintf("%s/%d", run.Condition, run.Repeat)
			if i < 0 || run.Repeat < 1 || run.Repeat > initialEvaluationRepeats || runs[key] || run.Order != (i-(run.Repeat-1)%len(names)+len(names))%len(names)+1 {
				return invalid("대응 회차/순환 순서")
			}
			runs[key] = true
			if !slices.Contains([]string{"initial", "prepared"}, run.IndexState) || (states[run.Repeat] != "" && states[run.Repeat] != run.IndexState) {
				return invalid("최초/준비 실행 통제")
			}
			states[run.Repeat] = run.IndexState
			for _, duration := range []*int64{run.PreparationMS, run.ReindexMS} {
				if duration != nil && (*duration < 0 || *duration > 1e12) {
					return invalid("준비/재색인 시간")
				}
			}
			if !manifestCode.MatchString(run.Trace) || run.InputHash != businessTaskHash(input, task, input.Conditions[i]) {
				return invalid("trace 결합")
			}
			if !slices.Contains([]string{"success", "failure", "timeout", "cancelled"}, run.Status) || run.ElapsedMS == nil || *run.ElapsedMS < 0 || *run.ElapsedMS > 1e12 || run.Turns == nil || *run.Turns < 0 || *run.Turns > *c.MaxTurns {
				return invalid("상태/시간/턴")
			}
			for _, count := range []*int{run.Tools, run.Interventions, run.EvidenceRefetches} {
				if count == nil || *count < 0 || *count > 1000000000 {
					return invalid("필수 횟수")
				}
			}
			if run.EvidenceAvailable == nil {
				return invalid("근거 재조회")
			}
			if err := validateBusinessCosts(run.Costs, input.Pricing); err != nil {
				return err
			}
			if run.Condition == "summary" {
				found := false
				for _, item := range run.Costs {
					found = found || (item.Component == "summary" && item.Status != "not_applicable")
				}
				if !found {
					return invalid("요약 비용")
				}
			}
		}
	}
	return nil
}

func businessConditionNames(experiment string) []string {
	switch experiment {
	case "lookup":
		return []string{"without_context", "with_context"}
	case "irrelevant":
		return []string{"relevant", "irrelevant"}
	case "reduction":
		return []string{"original", "summary", "masked"}
	default:
		return nil
	}
}

func runBusinessTasks(path, out string) error {
	startedAt := time.Now().UTC()
	input, err := loadBusinessTasks(path)
	if err != nil {
		return err
	}
	report := summarizeBusinessTasks(input)
	report.StartedAt = startedAt
	return writeReport(out, report)
}
