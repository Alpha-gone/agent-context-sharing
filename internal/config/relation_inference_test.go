package config

import (
	"strings"
	"testing"
	"time"
)

func TestRelationInferenceConfiguration(t *testing.T) {
	values := validValues()
	cfg, err := loadValues(values)
	if err != nil || cfg.RelationInference.Mode != "off" || cfg.RelationInference.Timeout != 30*time.Second || cfg.RelationInference.CandidateLimit != 8 {
		t.Fatalf("기본 추론 구성=%+v, %v", cfg.RelationInference, err)
	}
	values["RELATION_INFERENCE_MODE"] = "shadow"
	values["RELATION_INFERENCE_PROVIDER"] = "provider"
	values["RELATION_INFERENCE_MODEL"] = "gpt-6-luna"
	values["RELATION_INFERENCE_BASE_URL"] = "https://provider.test/v1"
	values["RELATION_INFERENCE_API_KEY"] = "synthetic-secret"
	values["RELATION_INFERENCE_AUTO_CONFIRM_TYPES"] = "relates_to,part_of"
	values["RELATION_INFERENCE_ACCEPTED_PROFILE"] = strings.Repeat("a", 64)
	if _, err := loadValues(values); err != nil {
		t.Fatal(err)
	}
	for key, invalid := range map[string]string{
		"RELATION_INFERENCE_MODE": "unsafe", "RELATION_INFERENCE_PROVIDER": "", "RELATION_INFERENCE_MODEL": "",
		"RELATION_INFERENCE_BASE_URL": "http://synthetic-secret", "RELATION_INFERENCE_API_KEY": "",
		"RELATION_INFERENCE_TIMEOUT": "0s", "RELATION_INFERENCE_CANDIDATE_LIMIT": "21",
		"RELATION_INFERENCE_TARGET_CHARS": "0", "RELATION_INFERENCE_CANDIDATE_CHARS": "-1",
		"RELATION_INFERENCE_INPUT_CHARS": "1", "RELATION_INFERENCE_INDEX_WAIT": "0s",
		"RELATION_INFERENCE_AUTO_CONFIRM_TYPES": "causes", "RELATION_INFERENCE_ACCEPTED_PROFILE": strings.Repeat("A", 64),
	} {
		t.Run(key, func(t *testing.T) {
			old := values[key]
			values[key] = invalid
			rejectValues(t, values)
			values[key] = old
		})
	}
}
