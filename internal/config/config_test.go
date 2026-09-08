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
		"직접 TLS의 신뢰 프록시 지정": func(values map[string]string) {
			values["TLS_TERMINATION"] = "direct"
			values["TLS_CERT_FILE"] = "/run/secrets/server.crt"
			values["TLS_KEY_FILE"] = "/run/secrets/server.key"
		},
		"직접 TLS의 인증서 누락": func(values map[string]string) {
			values["TLS_TERMINATION"] = "direct"
			values["TRUSTED_PROXY_CIDRS"] = ""
		},
		"프록시 TLS의 신뢰 대역 누락": func(values map[string]string) {
			values["TRUSTED_PROXY_CIDRS"] = ""
		},
		"신뢰 대역 형식 오류": func(values map[string]string) {
			values["TRUSTED_PROXY_CIDRS"] = "proxy.internal"
		},
		"신뢰 대역의 호스트 비트 잔존": func(values map[string]string) {
			values["TRUSTED_PROXY_CIDRS"] = "10.0.0.1/8"
		},
		"신뢰 대역 중복": func(values map[string]string) {
			values["TRUSTED_PROXY_CIDRS"] = "10.0.0.0/8,10.0.0.0/8"
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
	values["TRUSTED_PROXY_CIDRS"] = ""
	values["TLS_CERT_FILE"] = "/run/secrets/server.crt"
	values["TLS_KEY_FILE"] = "/run/secrets/server.key"
	cfg, err := Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("직접 TLS 구성 읽기: %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("직접 TLS에 신뢰 프록시가 남았다: %v", cfg.TrustedProxies)
	}
}

// TestLoadTrustedProxies는 대역과 단일 주소가 모두 비교 가능한 접두로 읽히는지 확인한다.
func TestLoadTrustedProxies(t *testing.T) {
	values := validValues()
	values["TRUSTED_PROXY_CIDRS"] = "10.0.0.0/8, 192.168.1.10, ::1"
	cfg, err := Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("신뢰 프록시 구성 읽기: %v", err)
	}
	want := []string{"10.0.0.0/8", "192.168.1.10/32", "::1/128"}
	if len(cfg.TrustedProxies) != len(want) {
		t.Fatalf("신뢰 프록시 개수 = %d, want %d", len(cfg.TrustedProxies), len(want))
	}
	for index, prefix := range cfg.TrustedProxies {
		if prefix.String() != want[index] {
			t.Fatalf("신뢰 프록시[%d] = %s, want %s", index, prefix, want[index])
		}
	}
}

// sharedGraphNamePattern은 config, migrate와 store가 함께 쓰기로 한 AGE 그래프 이름 경계다.
// 세 패키지가 각자 이 리터럴과의 일치를 확인하므로, 한 곳만 바꾸면 그 패키지의 테스트가
// 깨져 값이 갈라지는 것을 막는다. 값을 바꿀 때는 세 곳을 함께 고친다.
const sharedGraphNamePattern = `^[a-z_][a-z0-9_]*$`

// TestGraphNamePattern은 구성 검증의 그래프 이름 경계가 약속된 정규식과 같은지 확인한다.
// 이름 값의 수용·거부 사례는 TestLoad와 TestLoadRejectsInvalidValues가 이미 다룬다.
func TestGraphNamePattern(t *testing.T) {
	if got := graphNamePattern.String(); got != sharedGraphNamePattern {
		t.Fatalf("config의 그래프 이름 정규식 = %s, want %s", got, sharedGraphNamePattern)
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
		"BCRYPT_COST":           "12",
		"TLS_TERMINATION":       "proxy",
		"TRUSTED_PROXY_CIDRS":   "10.0.0.0/8",
		"TLS_CERT_FILE":         "",
		"TLS_KEY_FILE":          "",
	}
}
