// 업무 효과 평가의 집계 입력과 판정을 담당한다.
//
// 통제 과업의 본문과 참여자 식별 정보는 이 도구의 입력 형식에 없다. 같은 과업을
// 컨텍스트 조회 없이 수행한 값과 조회하며 수행한 값만 짝지어, 개선폭과 95%
// 신뢰구간을 계산한다.
package main

import (
	json "encoding/json/v2"
	"fmt"
	"math"
	"os"
	"time"
)

type businessSet struct {
	Version    string             `json:"version"`
	Conditions businessConditions `json:"conditions"`
	Samples    []businessSample   `json:"samples"`
}

// businessConditions는 두 조건에서 같게 유지한 실행 조건이다. 과업 집합은 Version이
// 식별하고, 개인이나 실제 업무 내용을 가리키는 값은 받지 않는다.
type businessConditions struct {
	AgentVersion  string `json:"agent_version"`
	Model         string `json:"model"`
	ContextBudget int    `json:"context_budget"`
}

type businessSample struct {
	WithoutContext businessMetrics `json:"without_context"`
	WithContext    businessMetrics `json:"with_context"`
}

type businessMetrics struct {
	CompletionTimeMS   int64   `json:"completion_time_ms"`
	ReworkCount        int     `json:"rework_count"`
	ResumeInteractions int     `json:"resume_interactions"`
	CollaborationUsage float64 `json:"collaboration_usage"`
}

type businessReport struct {
	StartedAt         time.Time          `json:"started_at"`
	Version           string             `json:"dataset_version"`
	Conditions        businessConditions `json:"conditions"`
	BaselineCondition string             `json:"baseline_condition"`
	CurrentCondition  string             `json:"current_condition"`
	Samples           int                `json:"samples"`
	Comparisons       []comparison       `json:"comparisons"`
	Passed            bool               `json:"passed"`
}

var businessMetricNames = []struct {
	name          string
	lowerIsBetter bool
	value         func(businessMetrics) float64
}{
	{"completion_time_ms", true, func(value businessMetrics) float64 { return float64(value.CompletionTimeMS) }},
	{"rework_count", true, func(value businessMetrics) float64 { return float64(value.ReworkCount) }},
	{"resume_interactions", true, func(value businessMetrics) float64 { return float64(value.ResumeInteractions) }},
	{"collaboration_usage", false, func(value businessMetrics) float64 { return value.CollaborationUsage }},
}

func runBusiness(path, outPath string) error {
	set, err := loadBusinessSet(path)
	if err != nil {
		return err
	}
	comparisons, passed := judgeBusiness(set.Samples)
	report := businessReport{
		StartedAt:         time.Now().UTC(),
		Version:           set.Version,
		Conditions:        set.Conditions,
		BaselineCondition: "without_context",
		CurrentCondition:  "with_context",
		Samples:           len(set.Samples),
		Comparisons:       comparisons,
		Passed:            passed,
	}
	return writeReport(outPath, report)
}

func loadBusinessSet(path string) (businessSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return businessSet{}, fmt.Errorf("업무 효과 집계 읽기: %w", err)
	}
	var set businessSet
	// 허용하지 않은 필드를 거부해 본문이나 개인 식별 정보가 실수로 입력 형식에
	// 추가되어도 조용히 무시한 채 평가 파일에 남는 일을 막는다.
	if err := json.Unmarshal(raw, &set, json.RejectUnknownMembers(true)); err != nil {
		return businessSet{}, fmt.Errorf("%s: 업무 효과 집계 해석: %w", path, err)
	}
	if err := validateBusinessSet(set); err != nil {
		return businessSet{}, fmt.Errorf("%s: %w", path, err)
	}
	return set, nil
}

func validateBusinessSet(set businessSet) error {
	if set.Version == "" {
		return fmt.Errorf("데이터셋 판을 비울 수 없다")
	}
	if set.Conditions.AgentVersion == "" || set.Conditions.Model == "" {
		return fmt.Errorf("agent_version과 model을 비울 수 없다")
	}
	if set.Conditions.ContextBudget < 0 {
		return fmt.Errorf("context_budget은 음수일 수 없다")
	}
	if len(set.Samples) < minimumRepeats {
		return fmt.Errorf("통제 과업 표본은 %d개 이상이어야 한다", minimumRepeats)
	}
	for index, sample := range set.Samples {
		if err := validateBusinessMetrics(index+1, "without_context", sample.WithoutContext); err != nil {
			return err
		}
		if err := validateBusinessMetrics(index+1, "with_context", sample.WithContext); err != nil {
			return err
		}
	}
	return nil
}

func validateBusinessMetrics(sample int, condition string, value businessMetrics) error {
	if value.CompletionTimeMS <= 0 {
		return fmt.Errorf("%d번 표본의 %s completion_time_ms는 양수여야 한다", sample, condition)
	}
	if value.ReworkCount < 0 || value.ResumeInteractions < 0 {
		return fmt.Errorf("%d번 표본의 %s 횟수 지표는 음수일 수 없다", sample, condition)
	}
	if math.IsNaN(value.CollaborationUsage) || math.IsInf(value.CollaborationUsage, 0) || value.CollaborationUsage < 0 || value.CollaborationUsage > 1 {
		return fmt.Errorf("%d번 표본의 %s collaboration_usage는 0 이상 1 이하여야 한다", sample, condition)
	}
	return nil
}

func judgeBusiness(samples []businessSample) ([]comparison, bool) {
	comparisons := make([]comparison, 0, len(businessMetricNames))
	passed := true
	for _, metric := range businessMetricNames {
		withoutContext := make([]float64, 0, len(samples))
		withContext := make([]float64, 0, len(samples))
		for _, sample := range samples {
			withoutContext = append(withoutContext, metric.value(sample.WithoutContext))
			withContext = append(withContext, metric.value(sample.WithContext))
		}
		difference, interval := pairedInterval(withoutContext, withContext)
		improved := difference > 0
		if metric.lowerIsBetter {
			improved = difference < 0
		}
		significant := math.Abs(difference) > interval
		comparisons = append(comparisons, comparison{
			Metric:      metric.name,
			Samples:     len(samples),
			Baseline:    mean(withoutContext),
			Current:     mean(withContext),
			Difference:  difference,
			Interval:    interval,
			Improved:    improved,
			Significant: significant,
		})
		passed = passed && improved && significant
	}
	return comparisons, passed
}
