package config

import "testing"

// TestLoad은 유효한 개발 구성이 형식화된 값으로 읽히는지 확인한다.
func TestLoad(t *testing.T) {
	values := validValues()
	cfg, err := Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("구성 읽기: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.EmbeddingDimension != 1024 {
		t.Fatalf("핵심 구성 값이 다르다: %+v", cfg)
	}
	if len(cfg.OAuthClientIDs) != 1 || len(cfg.OAuthRedirectURIs) != 1 {
		t.Fatalf("OAuth 목록이 다르다: %+v", cfg)
	}
}

// TestLoadRejectsInvalidValues은 기동 전에 잘못된 배포 구성이 거부되는지 확인한다.
func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := map[string]func(map[string]string){
		"수신 주소 누락":     func(values map[string]string) { values["HTTP_ADDR"] = "" },
		"그래프 이름 형식 오류": func(values map[string]string) { values["AGE_GRAPH_NAME"] = "bad-name" },
		"벡터 차원 오류":     func(values map[string]string) { values["EMBEDDING_DIMENSION"] = "0" },
		"직접 TLS의 전달 헤더 신뢰": func(values map[string]string) {
			values["TLS_TERMINATION"] = "direct"
			values["TRUST_FORWARDED_PROTO"] = "true"
		},
		"직접 TLS의 인증서 누락": func(values map[string]string) {
			values["TLS_TERMINATION"] = "direct"
			values["TRUST_FORWARDED_PROTO"] = "false"
		},
		"프록시 TLS의 전달 헤더 미신뢰": func(values map[string]string) {
			values["TRUST_FORWARDED_PROTO"] = "false"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			values := validValues()
			mutate(values)
			if _, err := Load(func(key string) string { return values[key] }); err == nil {
				t.Fatal("잘못된 구성이 허용됐다")
			}
		})
	}
}

// TestLoadDirectTLS는 직접 TLS 종단에 필요한 인증서 파일 구성이 읽히는지 확인한다.
func TestLoadDirectTLS(t *testing.T) {
	values := validValues()
	values["TLS_TERMINATION"] = "direct"
	values["TRUST_FORWARDED_PROTO"] = "false"
	values["TLS_CERT_FILE"] = "/run/secrets/server.crt"
	values["TLS_KEY_FILE"] = "/run/secrets/server.key"
	if _, err := Load(func(key string) string { return values[key] }); err != nil {
		t.Fatalf("직접 TLS 구성 읽기: %v", err)
	}
}

// validValues는 각 테스트가 독립적으로 바꿀 수 있는 유효한 배포 구성을 만든다.
func validValues() map[string]string {
	return map[string]string{
		"HTTP_ADDR":             ":8080",
		"DATABASE_URL":          "postgres://user:pass@localhost:5432/app",
		"AGE_GRAPH_NAME":        "agent_context",
		"EMBEDDING_BASE_URL":    "http://localhost:11434",
		"EMBEDDING_MODEL":       "bge-m3",
		"EMBEDDING_VECTOR_TYPE": "vector",
		"EMBEDDING_DIMENSION":   "1024",
		"OAUTH_CLIENT_IDS":      "agent-context-dev",
		"OAUTH_REDIRECT_URIS":   "http://127.0.0.1/callback",
		"TLS_TERMINATION":       "proxy",
		"TRUST_FORWARDED_PROTO": "true",
		"TLS_CERT_FILE":         "",
		"TLS_KEY_FILE":          "",
	}
}
