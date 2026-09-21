package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJudgeBusinessRequiresSignificantImprovementInEveryMetric(t *testing.T) {
	samples := []businessSample{
		businessTestSample(100, 50, 3, 1, 5, 2, 0, 0.5),
		businessTestSample(110, 55, 4, 2, 6, 3, 0, 0.5),
		businessTestSample(90, 45, 5, 3, 7, 4, 0, 0.5),
	}
	comparisons, passed := judgeBusiness(samples)
	if !passed {
		t.Fatalf("네 지표가 모두 유의하게 개선되면 통과해야 한다: %+v", comparisons)
	}
	if len(comparisons) != len(businessMetricNames) {
		t.Fatalf("비교 지표 수가 다르다: %d", len(comparisons))
	}
	for _, value := range comparisons {
		if !value.Improved || !value.Significant || value.Samples != len(samples) {
			t.Fatalf("유의한 개선 판정이 아니다: %+v", value)
		}
	}
}

func TestJudgeBusinessFailsWhenOneMetricDoesNotImprove(t *testing.T) {
	samples := []businessSample{
		businessTestSample(100, 50, 3, 3, 5, 2, 0, 0.5),
		businessTestSample(110, 55, 4, 4, 6, 3, 0, 0.5),
		businessTestSample(90, 45, 5, 5, 7, 4, 0, 0.5),
	}
	if _, passed := judgeBusiness(samples); passed {
		t.Fatal("재작업이 개선되지 않았는데 통과했다")
	}
}

func TestLoadBusinessSetRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "business.json")
	body := `{
  "version": "business-v1",
  "conditions": {"agent_version": "agent-v1", "model": "model-v1", "context_budget": 4000},
  "samples": [
    {"body":"민감한 본문","without_context":{"completion_time_ms":100,"rework_count":1,"resume_interactions":1,"collaboration_usage":0},"with_context":{"completion_time_ms":50,"rework_count":0,"resume_interactions":0,"collaboration_usage":0.5}},
    {"without_context":{"completion_time_ms":100,"rework_count":1,"resume_interactions":1,"collaboration_usage":0},"with_context":{"completion_time_ms":50,"rework_count":0,"resume_interactions":0,"collaboration_usage":0.5}},
    {"without_context":{"completion_time_ms":100,"rework_count":1,"resume_interactions":1,"collaboration_usage":0},"with_context":{"completion_time_ms":50,"rework_count":0,"resume_interactions":0,"collaboration_usage":0.5}}
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBusinessSet(path); err == nil {
		t.Fatal("허용하지 않은 본문 필드를 거부하지 않았다")
	}
}

func TestValidateBusinessSetRejectsInsufficientOrInvalidSamples(t *testing.T) {
	valid := businessSet{
		Version:    "business-v1",
		Conditions: businessConditions{AgentVersion: "agent-v1", Model: "model-v1", ContextBudget: 4000},
		Samples: []businessSample{
			businessTestSample(100, 50, 1, 0, 1, 0, 0, 0.5),
			businessTestSample(100, 50, 1, 0, 1, 0, 0, 0.5),
			businessTestSample(100, 50, 1, 0, 1, 0, 0, 0.5),
		},
	}
	tooFew := valid
	tooFew.Samples = tooFew.Samples[:2]
	if err := validateBusinessSet(tooFew); err == nil {
		t.Fatal("세 개보다 적은 표본을 허용했다")
	}
	invalidRatio := valid
	invalidRatio.Samples = append([]businessSample(nil), valid.Samples...)
	invalidRatio.Samples[0].WithContext.CollaborationUsage = 1.1
	if err := validateBusinessSet(invalidRatio); err == nil {
		t.Fatal("범위를 벗어난 협업 활용 비율을 허용했다")
	}
}

func businessTestSample(withoutTime, withTime int64, withoutRework, withRework, withoutResume, withResume int, withoutCollaboration, withCollaboration float64) businessSample {
	return businessSample{
		WithoutContext: businessMetrics{
			CompletionTimeMS: withoutTime, ReworkCount: withoutRework,
			ResumeInteractions: withoutResume, CollaborationUsage: withoutCollaboration,
		},
		WithContext: businessMetrics{
			CompletionTimeMS: withTime, ReworkCount: withRework,
			ResumeInteractions: withResume, CollaborationUsage: withCollaboration,
		},
	}
}
