package config

import (
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

// TestLoad은 유효한 개발 구성이 형식화된 값으로 읽히는지 확인한다.
func TestLoad(t *testing.T) {
	values := validValues()
	cfg, err := Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("구성 읽기: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.EmbeddingDimension != 1024 || cfg.SearchExecution != SearchExecutionParallel || cfg.SearchGraphStage != SearchGraphStageBaseline || !cfg.SearchGlobalFallback || cfg.SearchCandidateLimit != 50 || cfg.SearchSemanticThreshold != 0.7 || cfg.RelationAdjacencyWindow != time.Hour || cfg.RelationSimilarityThreshold != 0.8 || cfg.RelationProposalLimit != 10 {
		t.Fatalf("핵심 구성 값이 다르다: %+v", cfg)
	}
	if len(cfg.OAuthClients) != 1 || len(cfg.OAuthClients["agent-context-dev"]) != 1 || cfg.ResourceServerURL.String() != "https://service.test/mcp" || cfg.AuthorizationServerURL.String() != "https://issuer.test" || len(cfg.MCPAllowedOrigins) != 1 {
		t.Fatalf("OAuth 목록이 다르다: %+v", cfg)
	}
}

// TestLoadEvidencePathSelection은 근거 경로 보존 선택이 비어 있으면 꺼지고 명시한
// 값만 켜지는지 확인한다. 통제 비교 전 운영 기본값은 기존 통합 순위 절단이다.
func TestLoadEvidencePathSelection(t *testing.T) {
	values := validValues()
	cfg, err := Load(func(name string) string { return values[name] })
	if err != nil || cfg.SearchEvidencePathSelection {
		t.Fatalf("비어 있는 근거 경로 선택 = %v, %v", cfg.SearchEvidencePathSelection, err)
	}
	values["SEARCH_EVIDENCE_PATH_SELECTION_ENABLED"] = "true"
	cfg, err = Load(func(name string) string { return values[name] })
	if err != nil || !cfg.SearchEvidencePathSelection {
		t.Fatalf("켠 근거 경로 선택 = %v, %v", cfg.SearchEvidencePathSelection, err)
	}
}

func TestLoadAccountPlanLimits(t *testing.T) {
	accountID, err := model.NewID()
	if err != nil {
		t.Fatalf("UUIDv7 생성: %v", err)
	}
	values := validValues()
	values["ACCOUNT_PLAN_LIMITS"] = `{"` + accountID.String() + `":{"max_hops":6,"writes_per_minute":20}}`
	cfg, err := Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("플랜 구성 읽기: %v", err)
	}
	if got := cfg.AccountPlans.For(accountID); got.MaxHops != 6 || got.WritesPerMinute != 20 {
		t.Fatalf("계정 플랜 = %+v", got)
	}
	other, err := model.NewID()
	if err != nil {
		t.Fatalf("UUIDv7 생성: %v", err)
	}
	if got := cfg.AccountPlans.For(other); got != plan.Default() {
		t.Fatalf("기본 플랜 = %+v, want %+v", got, plan.Default())
	}
}

// TestLoadPlacementDefaultsToEmbedded는 「배치 조합」이 기본값으로 삼은 둘 다 내장이
// 구성 값을 비워 두었을 때 그대로 나오는지 확인한다.
func TestLoadPlacementDefaultsToEmbedded(t *testing.T) {
	values := validValues()
	delete(values, "INDEX_WORKER_PLACEMENT")
	delete(values, "AUTHORIZATION_SERVER_PLACEMENT")
	cfg, err := Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("구성 읽기: %v", err)
	}
	if cfg.IndexWorkerPlacement != PlacementEmbedded || cfg.AuthorizationServerPlacement != PlacementEmbedded {
		t.Fatalf("기본 배치 = %q, %q; 둘 다 embedded여야 한다", cfg.IndexWorkerPlacement, cfg.AuthorizationServerPlacement)
	}
}

// TestLoadIndexTargetsDefaultsToAllLayers는 「색인 대상 비교」에서 채택한
// 원천 포함 구성을 빈 값의 기본으로 유지하는지 확인한다.
func TestLoadIndexTargetsDefaultsToAllLayers(t *testing.T) {
	values := validValues()
	delete(values, "INDEX_TARGET_LAYERS")
	cfg, err := Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("구성 읽기: %v", err)
	}
	if cfg.IndexTargetLayers != IndexTargetLayersAll {
		t.Fatalf("기본 색인 대상 = %q; all_layers여야 한다", cfg.IndexTargetLayers)
	}
}

// TestLoadIndexTargetsAcceptsWithoutSource는 비교에 쓸 다른 구성이 실제로 읽히는지 본다.
func TestLoadIndexTargetsAcceptsWithoutSource(t *testing.T) {
	values := validValues()
	values["INDEX_TARGET_LAYERS"] = string(IndexTargetLayersWithoutSource)
	cfg, err := Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("구성 읽기: %v", err)
	}
	if cfg.IndexTargetLayers != IndexTargetLayersWithoutSource {
		t.Fatalf("색인 대상 = %q; without_source여야 한다", cfg.IndexTargetLayers)
	}
}

func TestLoadAcceptsFourPlacementCombinations(t *testing.T) {
	for _, worker := range []ComponentPlacement{PlacementEmbedded, PlacementExternal} {
		for _, authorization := range []ComponentPlacement{PlacementEmbedded, PlacementExternal} {
			values := validValues()
			values["INDEX_WORKER_PLACEMENT"] = string(worker)
			values["AUTHORIZATION_SERVER_PLACEMENT"] = string(authorization)
			cfg, err := Load(func(name string) string { return values[name] })
			if err != nil {
				t.Fatalf("색인 %s, 인가 %s 구성 읽기: %v", worker, authorization, err)
			}
			if cfg.IndexWorkerPlacement != worker || cfg.AuthorizationServerPlacement != authorization {
				t.Fatalf("배치 = %q, %q; want %q, %q", cfg.IndexWorkerPlacement, cfg.AuthorizationServerPlacement, worker, authorization)
			}
		}
	}
}

// TestLoadRejectsInvalidValues은 기동 전에 잘못된 배포 구성이 거부되는지 확인한다.
func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := map[string]func(map[string]string){
		"수신 주소 누락":        func(values map[string]string) { values["HTTP_ADDR"] = "" },
		"그래프 이름 형식 오류":    func(values map[string]string) { values["AGE_GRAPH_NAME"] = "bad-name" },
		"벡터 차원 오류":        func(values map[string]string) { values["EMBEDDING_DIMENSION"] = "0" },
		"색인 작업자 배치 오류":    func(values map[string]string) { values["INDEX_WORKER_PLACEMENT"] = "detached" },
		"인가 서버 배치 오류":     func(values map[string]string) { values["AUTHORIZATION_SERVER_PLACEMENT"] = "detached" },
		"검색 채널 실행 방식 오류":  func(values map[string]string) { values["SEARCH_CHANNEL_EXECUTION"] = "unknown" },
		"검색 후보 수 상한 오류":   func(values map[string]string) { values["SEARCH_CHANNEL_CANDIDATE_LIMIT"] = "0" },
		"검색 의미 유사도 하한 오류": func(values map[string]string) { values["SEARCH_SEMANTIC_SIMILARITY_THRESHOLD"] = "1.1" },
		"그래프 검색 단계 오류":    func(values map[string]string) { values["SEARCH_GRAPH_STAGE"] = "unknown" },
		"전역 전환 활성화 오류":    func(values map[string]string) { values["SEARCH_GLOBAL_FALLBACK_ENABLED"] = "unknown" },
		"근거 경로 선택 활성화 오류": func(values map[string]string) { values["SEARCH_EVIDENCE_PATH_SELECTION_ENABLED"] = "unknown" },
		"색인 대상 계층 오류":     func(values map[string]string) { values["INDEX_TARGET_LAYERS"] = "derived_only" },
		"관계 시간 인접 임계값 오류": func(values map[string]string) { values["RELATION_ADJACENCY_WINDOW"] = "0" },
		"관계 유사도 임계값 오류":   func(values map[string]string) { values["RELATION_SIMILARITY_THRESHOLD"] = "1.1" },
		"관계 후보 수 상한 오류":   func(values map[string]string) { values["RELATION_PROPOSAL_LIMIT"] = "0" },
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
		"플랜 구성 키 오류": func(values map[string]string) {
			values["ACCOUNT_PLAN_LIMITS"] = `{"0198e7c0-0000-7000-8000-000000000001":{"unknown":1}}`
		},
		"보호 리소스 경로 오류":     func(values map[string]string) { values["RESOURCE_SERVER_URL"] = "https://service.test/other" },
		"인가 서버 origin 오류":  func(values map[string]string) { values["AUTHORIZATION_SERVER_URL"] = "https://issuer.test/oauth" },
		"허용 Origin 경로 오류":  func(values map[string]string) { values["MCP_ALLOWED_ORIGINS"] = "https://client.test/callback" },
		"접기 임계값 범위 오류":     func(values map[string]string) { values["SEARCH_FOLD_SIMILARITY_THRESHOLD"] = "1.5" },
		"클라이언트 구성 누락":      func(values map[string]string) { values["OAUTH_CLIENTS"] = "" },
		"클라이언트 구성 JSON 오류": func(values map[string]string) { values["OAUTH_CLIENTS"] = "agent-context-dev" },
		"클라이언트 빈 목록":       func(values map[string]string) { values["OAUTH_CLIENTS"] = `{"agent-context-dev":[]}` },
		"클라이언트 redirect_uri 오류": func(values map[string]string) {
			values["OAUTH_CLIENTS"] = `{"agent-context-dev":["ftp://127.0.0.1/callback"]}`
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
		"HTTP_ADDR":                            ":8080",
		"DATABASE_URL":                         "postgres://user:pass@localhost:5432/app",
		"AGE_GRAPH_NAME":                       "agent_context",
		"EMBEDDING_BASE_URL":                   "http://localhost:11434",
		"EMBEDDING_MODEL":                      "bge-m3",
		"EMBEDDING_VECTOR_TYPE":                "vector",
		"EMBEDDING_DIMENSION":                  "1024",
		"SEARCH_CHANNEL_EXECUTION":             "parallel",
		"SEARCH_CHANNEL_CANDIDATE_LIMIT":       "50",
		"SEARCH_SEMANTIC_SIMILARITY_THRESHOLD": "0.70",
		"SEARCH_GRAPH_STAGE":                   "baseline",
		"SEARCH_GLOBAL_FALLBACK_ENABLED":       "true",
		"SEARCH_FOLD_SIMILARITY_THRESHOLD":     "0.90",
		"OAUTH_CLIENTS":                        `{"agent-context-dev":["http://127.0.0.1/callback"]}`,
		"RESOURCE_SERVER_URL":                  "https://service.test/mcp",
		"AUTHORIZATION_SERVER_URL":             "https://issuer.test",
		"MCP_ALLOWED_ORIGINS":                  "https://client.test",
		"BCRYPT_COST":                          "12",
		"RELATION_ADJACENCY_WINDOW":            "1h",
		"RELATION_SIMILARITY_THRESHOLD":        "0.8",
		"RELATION_PROPOSAL_LIMIT":              "10",
		"ACCOUNT_PLAN_LIMITS":                  "",
		"TLS_TERMINATION":                      "proxy",
		"TRUSTED_PROXY_CIDRS":                  "10.0.0.0/8",
		"TLS_CERT_FILE":                        "",
		"TLS_KEY_FILE":                         "",
	}
}
