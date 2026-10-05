package main

import (
	"strings"
	"testing"
)

func TestAuditEmbeddingProvider(t *testing.T) {
	for _, test := range []struct {
		name, provider, address, model, modelID string
	}{
		{"명시적 Ollama", "ollama", "http://localhost:11434", "bge-m3", "bge-m3:vector:768"},
		{"키 없는 Gemini 감사", "gemini", "https://generativelanguage.googleapis.com", "gemini-embedding-2", "gemini:gemini-embedding-2:retrieval-v1:vector:768"},
		{"제공자 생략", "", "http://localhost:11434", "bge-m3", ""},
		{"Gemini 제공자 생략", "", "https://generativelanguage.googleapis.com", "gemini-embedding-2", ""},
		{"Gemini 모델 혼합", "ollama", "http://localhost:11434", "gemini-embedding-2", ""},
		{"Gemini 주소 혼합", "ollama", "https://generativelanguage.googleapis.com", "bge-m3", ""},
		{"Gemini 모델 오류", "gemini", "https://generativelanguage.googleapis.com", "bge-m3", ""},
		{"Gemini 평문 주소", "gemini", "http://service.test", "gemini-embedding-2", ""},
		{"주소 생략", "gemini", "", "gemini-embedding-2", ""},
		{"잘못된 비밀 URL", "gemini", "https://%synthetic-secret", "gemini-embedding-2", ""},
		{"URL 비밀 포함", "gemini", "https://service.test?key=synthetic-secret", "gemini-embedding-2", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range map[string]string{
				"EMBEDDING_PROVIDER": test.provider, "EMBEDDING_BASE_URL": test.address,
				"EMBEDDING_MODEL": test.model, "EMBEDDING_VECTOR_TYPE": "vector",
				"EMBEDDING_DIMENSION": "768", "GEMINI_API_KEY": "",
			} {
				t.Setenv(name, value)
			}
			cfg, err := loadEmbeddingConfig()
			if (err == nil) != (test.modelID != "") || (err != nil && strings.Contains(err.Error(), "synthetic-secret")) {
				t.Fatalf("감사 제공자 구성 수용·비밀 비노출 = %v", err)
			}
			if err == nil && cfg.ModelID() != test.modelID {
				t.Fatalf("감사 색인 식별자 = %q, want %q", cfg.ModelID(), test.modelID)
			}
		})
	}
}
