package config

import (
	"maps"
	"strings"
	"testing"
)

func TestGeminiConfiguration(t *testing.T) {
	values := validValues()
	values["EMBEDDING_PROVIDER"] = "gemini"
	values["EMBEDDING_BASE_URL"] = "https://generativelanguage.googleapis.com"
	values["EMBEDDING_MODEL"] = "gemini-embedding-2"
	values["EMBEDDING_DIMENSION"] = "768"
	values["GEMINI_API_KEY"] = "synthetic-key"
	load := func() (Config, error) { return Load(func(name string) string { return values[name] }) }
	if cfg, err := load(); err != nil || cfg.EmbeddingProvider != "gemini" || cfg.EmbeddingDimension != 768 || cfg.GeminiAPIKey != "synthetic-key" {
		t.Fatalf("Gemini 구성: %v", err)
	}
	for _, test := range []struct{ name, value string }{
		{"GEMINI_API_KEY", ""}, {"GEMINI_API_KEY", "synthetic-key\nheader"},
		{"EMBEDDING_PROVIDER", "unknown"}, {"EMBEDDING_PROVIDER", ""}, {"EMBEDDING_MODEL", "other-model"},
		{"EMBEDDING_DIMENSION", "127"}, {"EMBEDDING_DIMENSION", "3072"},
		{"EMBEDDING_BASE_URL", "http://service.test"},
		{"EMBEDDING_BASE_URL", "https://service.test?key=synthetic-key"},
		{"EMBEDDING_BASE_URL", "https://synthetic-key@service.test"},
		{"EMBEDDING_BASE_URL", "https://%synthetic-key"},
	} {
		t.Run(test.name+test.value, func(t *testing.T) {
			old := values[test.name]
			values[test.name] = test.value
			defer func() { values[test.name] = old }()
			_, err := load()
			if err == nil || strings.Contains(err.Error(), "synthetic-key") {
				t.Fatalf("구성 거부·비밀 비노출: %v", err)
			}
		})
	}
	values["EMBEDDING_VECTOR_TYPE"], values["EMBEDDING_DIMENSION"] = "halfvec", "3072"
	if _, err := load(); err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddingProviderSelection(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		valid  bool
	}{
		{"명시적 Gemini", nil, true},
		{"미지원 제공자", map[string]string{"EMBEDDING_PROVIDER": "unsupported"}, false},
		{"제공자 생략", map[string]string{"EMBEDDING_PROVIDER": ""}, false},
		{"제공자 공백", map[string]string{"EMBEDDING_PROVIDER": " "}, false},
		{"잘못된 모델", map[string]string{"EMBEDDING_MODEL": "other-model"}, false},
		{"평문 주소", map[string]string{"EMBEDDING_BASE_URL": "http://service.test"}, false},
		{"대문자 HTTPS 호스트", map[string]string{"EMBEDDING_BASE_URL": "https://GENERATIVELANGUAGE.GOOGLEAPIS.COM.:443"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := validValues()
			maps.Copy(values, test.values)
			_, err := Load(func(name string) string { return values[name] })
			if (err == nil) != test.valid {
				t.Fatalf("제공자 구성 유효성 = %v, want %t", err, test.valid)
			}
		})
	}
}
