package main

import (
	"strings"
	"testing"
)

func TestLoadSettingsEmbeddingProvider(t *testing.T) {
	for _, test := range []struct {
		name, provider, address, model, key string
		valid                               bool
	}{
		{"명시적 Gemini", "gemini", "https://generativelanguage.googleapis.com", "gemini-embedding-2", "synthetic-secret", true},
		{"미지원 제공자", "unsupported", "https://generativelanguage.googleapis.com", "gemini-embedding-2", "synthetic-secret", false},
		{"Gemini 제공자 생략", "", "https://generativelanguage.googleapis.com", "gemini-embedding-2", "synthetic-secret", false},
		{"Gemini 모델 오류", "gemini", "https://generativelanguage.googleapis.com", "other-model", "synthetic-secret", false},
		{"Gemini 키 생략", "gemini", "https://generativelanguage.googleapis.com", "gemini-embedding-2", "", false},
		{"키 앞 개행", "gemini", "https://generativelanguage.googleapis.com", "gemini-embedding-2", "\nsynthetic-secret", false},
		{"키 뒤 개행", "gemini", "https://generativelanguage.googleapis.com", "gemini-embedding-2", "synthetic-secret\n", false},
		{"키 탭", "gemini", "https://generativelanguage.googleapis.com", "gemini-embedding-2", "synthetic-secret\t", false},
		{"빈 host", "gemini", "https://:443", "gemini-embedding-2", "synthetic-secret", false},
		{"빈 포트", "gemini", "https://service.test:", "gemini-embedding-2", "synthetic-secret", false},
		{"포트 0", "gemini", "https://service.test:0", "gemini-embedding-2", "synthetic-secret", false},
		{"포트 초과", "gemini", "https://service.test:65536", "gemini-embedding-2", "synthetic-secret", false},
		{"잘못된 비밀 URL", "gemini", "https://%synthetic-secret", "gemini-embedding-2", "synthetic-secret", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range map[string]string{
				"DATABASE_URL": "postgres://unused", "AGE_GRAPH_NAME": "agent_context",
				"EMBEDDING_PROVIDER": test.provider, "EMBEDDING_BASE_URL": test.address,
				"EMBEDDING_MODEL": test.model, "GEMINI_API_KEY": test.key,
				"EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": "768",
				"SEARCH_CHANNEL_EXECUTION": "", "SEARCH_CHANNEL_CANDIDATE_LIMIT": "",
				"SEARCH_SEMANTIC_SIMILARITY_THRESHOLD": "", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "",
			} {
				t.Setenv(name, value)
			}
			loaded, err := loadSettings()
			if (err == nil) != test.valid || (err != nil && strings.Contains(err.Error(), "synthetic-secret")) {
				t.Fatalf("평가 제공자 구성 수용·비밀 비노출 = %v, want %t", err, test.valid)
			}
			if err == nil && loaded.index.Provider != test.provider {
				t.Fatalf("평가 제공자 = %q, want %q", loaded.index.Provider, test.provider)
			}
		})
	}
}

func TestIDX03DimensionErrorDoesNotDiscloseInput(t *testing.T) {
	for name, value := range map[string]string{
		"DATABASE_URL": "postgres://unused", "AGE_GRAPH_NAME": "agent_context",
		"EMBEDDING_BASE_URL": "https://generativelanguage.googleapis.com", "EMBEDDING_DIMENSION": "synthetic-secret",
	} {
		t.Setenv(name, value)
	}
	_, err := loadSettings()
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("차원 오류의 원문 비노출=%v", err)
	}
}
