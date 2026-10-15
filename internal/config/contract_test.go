package config

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func loadValues(values map[string]string) (Config, error) {
	return Load(func(key string) string { return values[key] })
}

func rejectValues(t *testing.T, values map[string]string) error {
	t.Helper()
	cfg, err := loadValues(values)
	if err == nil || !reflect.ValueOf(cfg).IsZero() {
		t.Fatal("잘못된 구성이 거부되지 않았거나 부분 구성이 반환됐다")
	}
	if strings.Contains(fmt.Sprintf("%+v", err), "synthetic-secret") {
		t.Fatal("구성 오류가 입력 원문을 노출했다")
	}
	return err
}

func TestConfigReadsOnlyAllowedKeysOnce(t *testing.T) {
	values := validValues()
	values["TOKEN_LIFETIME"] = "synthetic-secret"
	reads := map[string]int{}
	_, err := Load(func(key string) string { reads[key]++; return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	keys := AllowedKeys()
	if len(keys) != 38 || len(reads) != len(keys) {
		t.Fatalf("허용 키·읽은 키 수 = %d·%d", len(keys), len(reads))
	}
	for _, key := range keys {
		if reads[key] != 1 {
			t.Fatalf("%s 읽기 횟수 = %d", key, reads[key])
		}
	}
	keys[0] = "TOKEN_LIFETIME"
	if slices.Contains(AllowedKeys(), "TOKEN_LIFETIME") {
		t.Fatal("반환 목록이 내부 허용 목록을 변경했다")
	}
	if _, err := Load(nil); err == nil {
		t.Fatal("환경 경계 누락을 허용했다")
	}
}

func TestConfigExamplesAndAllowlistMatchDesign(t *testing.T) {
	design, err := os.ReadFile("../../spec/service/agent-context/sdd/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../../.env.example", "../../.env.prod.example"} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			values := validValues()
			for line := range strings.SplitSeq(string(data), "\n") {
				if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
					values[key] = strings.Trim(value, "'")
				}
			}
			values["GEMINI_API_KEY"] = "synthetic-key"
			if _, err := loadValues(values); err != nil {
				t.Fatal(err)
			}
			for _, key := range AllowedKeys() {
				if !strings.Contains(string(design), "\n"+key+"\n") {
					t.Fatalf("SDD 허용 목록 누락: %s", key)
				}
				if path == "../../.env.example" && !strings.Contains(string(data), "\n"+key+"=") {
					t.Fatalf("개발 환경 예시 누락: %s", key)
				}
			}
		})
	}
}

func TestConfigRejectsMissingRequiredKeys(t *testing.T) {
	for _, key := range []string{
		"HTTP_ADDR", "DATABASE_URL", "AGE_GRAPH_NAME", "EMBEDDING_PROVIDER", "GEMINI_API_KEY",
		"EMBEDDING_BASE_URL", "EMBEDDING_MODEL", "EMBEDDING_VECTOR_TYPE", "EMBEDDING_DIMENSION",
		"SEARCH_CHANNEL_EXECUTION", "SEARCH_CHANNEL_CANDIDATE_LIMIT", "SEARCH_SEMANTIC_SIMILARITY_THRESHOLD",
		"SEARCH_FOLD_SIMILARITY_THRESHOLD", "SEARCH_GRAPH_STAGE", "SEARCH_GLOBAL_FALLBACK_ENABLED",
		"AUTHORIZATION_SERVER_URL", "BCRYPT_COST", "RELATION_ADJACENCY_WINDOW", "RELATION_SIMILARITY_THRESHOLD",
		"RELATION_PROPOSAL_LIMIT", "TLS_TERMINATION", "TRUSTED_PROXY_CIDRS", "MCP_RESOURCE_URL",
		"MCP_ALLOWED_ORIGINS", "OAUTH_CLIENT_REGISTRATIONS", "DIAGNOSTIC_INSTANCE_ID",
	} {
		t.Run(key, func(t *testing.T) {
			values := validValues()
			delete(values, key)
			rejectValues(t, values)
		})
	}
}

func TestConfigRejectsInvalidBoundariesWithoutInputEcho(t *testing.T) {
	for key, invalid := range map[string][]string{
		"HTTP_ADDR":                      {"synthetic-secret", ":synthetic-secret", ":65536", "synthetic-secret%:8080", ":80\n"},
		"DATABASE_URL":                   {"https://synthetic-secret", "postgres://synthetic-secret%", "postgres://host:65536/db", "postgres:///db", "postgres://host/db?x=%synthetic-secret", "postgres://host/db#synthetic-secret"},
		"AGE_GRAPH_NAME":                 {"synthetic-secret"},
		"EMBEDDING_COLD_TABLESPACE":      {"synthetic-secret;drop table"},
		"EMBEDDING_PROVIDER":             {"synthetic-secret"},
		"EMBEDDING_VECTOR_TYPE":          {"synthetic-secret", "bit"},
		"EMBEDDING_DIMENSION":            {"127", "3073", "1.0", "9223372036854775808"},
		"GEMINI_API_KEY":                 {"synthetic-secret\n", "synthetic-secret\rheader", "synthetic-secret\x00"},
		"EMBEDDING_BASE_URL":             {"https://host#", "https://host?", "https://synthetic-secret@host", "https://:443", "https://host:0", "https://host:65536", "https://[synthetic-secret]", "https://[fe80::1%25eth0]", "https://host\n"},
		"SEARCH_CHANNEL_EXECUTION":       {"synthetic-secret"},
		"SEARCH_CHANNEL_CANDIDATE_LIMIT": {"-1", "1.5", "9223372036854775808"},
		"SEARCH_GRAPH_STAGE":             {"synthetic-secret"},
		"INDEX_TARGET_LAYERS":            {"synthetic-secret"},
		"INDEX_WORKER_PLACEMENT":         {"synthetic-secret"},
		"AUTHORIZATION_SERVER_URL":       {"http://host", "https://host/", "https://host?", "https://host#", "https://synthetic-secret@host", "https://%synthetic-secret"},
		"BCRYPT_COST":                    {"3", "32", "synthetic-secret"},
		"RELATION_ADJACENCY_WINDOW":      {"-1h", "999999999999999h", "synthetic-secret"},
		"RELATION_PROPOSAL_LIMIT":        {"-1", "1.5", "9223372036854775808"},
		"TLS_TERMINATION":                {"synthetic-secret"},
		"TRUSTED_PROXY_CIDRS":            {"synthetic-secret", "10.0.0.1/8", "::ffff:10.0.0.0/104", "fe80::1%eth0", "127.0.0.1,127.0.0.1/32", "::ffff:127.0.0.1,127.0.0.1"},
		"MCP_RESOURCE_URL":               {"http://host/mcp", "https://host/", "https://host/mcp/", "https://host/%6dcp", "https://host/mcp?", "https://host/mcp#", "https://synthetic-secret@host/mcp", "https://*.test/mcp"},
		"MCP_ALLOWED_ORIGINS":            {"http://host", "https://host/", "https://host?", "https://host#", "*", "https://*.test", "https://host,", "https://host,https://host", "https://synthetic-secret@host"},
		"DIAGNOSTIC_LOG_DIR":             {"relative", "/", "/var/log/../logs", "/var/log//logs", "/var/log/./logs", "/var/log/logs/", "/tmp/log", "/var/tmp/log", "/private/tmp/log", "/private/var/tmp/log", "/private/var/folders/log", "/var/folders/log", "/dev/shm/log", "/srv/assets/log", "/srv/public/log", "/srv/STATIC/log", "/var/log/secret\\dir", "/var/log/secret\n"},
		"DIAGNOSTIC_INSTANCE_ID":         {".", "..", "../synthetic-secret", "a/b", "a\\b", "a.b", "a b", "슬롯", strings.Repeat("a", 65), "slot\n"},
		"DIAGNOSTIC_LOG_LEVEL":           {"info", "TRACE", "synthetic-secret", "INFO\n"},
	} {
		for i, value := range invalid {
			t.Run(fmt.Sprintf("%s/%d", key, i), func(t *testing.T) {
				values := validValues()
				values[key] = value
				rejectValues(t, values)
			})
		}
	}
}

func TestConfigRejectsNonFiniteThresholds(t *testing.T) {
	for _, key := range []string{"SEARCH_SEMANTIC_SIMILARITY_THRESHOLD", "SEARCH_FOLD_SIMILARITY_THRESHOLD", "RELATION_SIMILARITY_THRESHOLD", "SEARCH_ADAPTIVE_DIRECT_SIMILARITY_THRESHOLD", "SEARCH_ADAPTIVE_DIRECT_MARGIN_THRESHOLD"} {
		for _, value := range []string{"NaN", "Inf", "-Inf", "1e309"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				values := validValues()
				maps.Copy(values, map[string]string{"SEARCH_QUERY_ADAPTIVE_ROUTING_ENABLED": "true", "SEARCH_GRAPH_STAGE": "relations", "SEARCH_ADAPTIVE_DIRECT_SIMILARITY_THRESHOLD": "0.7", "SEARCH_ADAPTIVE_DIRECT_MARGIN_THRESHOLD": "0.1"})
				values[key] = value
				rejectValues(t, values)
			})
		}
	}
}

func TestConfigDefaultsAndIndependentConnections(t *testing.T) {
	values := validValues()
	values["MCP_ALLOWED_ORIGINS"] = "https://browser.test, https://other.test:8443"
	values["EMBEDDING_COLD_TABLESPACE"] = "cold_storage"
	values["SEARCH_ADAPTIVE_DIRECT_SIMILARITY_THRESHOLD"] = "NaN"
	values["SEARCH_ADAPTIVE_DIRECT_MARGIN_THRESHOLD"] = "unused"
	cfg, err := loadValues(values)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthorizationServerURL.String() != "https://issuer.test" || cfg.MCPResourceURL.String() != "https://resource.test/mcp" || len(cfg.MCPAllowedOrigins) != 2 || cfg.OAuthClients["test-client"].LoopbackPortPolicy != LoopbackExact {
		t.Fatal("독립된 issuer·resource·Origin·정확한 callback 정책이 보존되지 않았다")
	}
	if cfg.Diagnostic != (DiagnosticConfig{Directory: "/var/log/agent-context", InstanceID: "test-slot", Level: "INFO"}) || cfg.EmbeddingColdTablespace != "cold_storage" || cfg.SearchAdaptiveRouting || cfg.SearchEvidencePathSelection || cfg.SearchAdaptiveDirectThreshold != 0 || cfg.SearchAdaptiveMarginThreshold != 0 {
		t.Fatal("기본값 또는 비활성 적응형 임계값 처리 불일치")
	}
	cfg.MCPAllowedOrigins[0] = "changed"
	client := cfg.OAuthClients["test-client"]
	client.RedirectURIs[0] = "changed"
	cfg.MCPResourceURL.Host = "changed"
	fresh, err := loadValues(values)
	if err != nil || fresh.MCPAllowedOrigins[0] == "changed" || fresh.OAuthClients["test-client"].RedirectURIs[0] == "changed" || fresh.MCPResourceURL.Host == "changed" {
		t.Fatal("다른 구성 로드에 가변 상태가 공유됐다")
	}
}

func TestConfigPreservesSafeParserCauses(t *testing.T) {
	values := validValues()
	values["DATABASE_URL"] = "postgres://synthetic-secret%"
	err := rejectValues(t, values)
	if _, ok := errors.AsType[*url.Error](err); !ok {
		t.Fatal("URL 해석 원인이 소실됐다")
	}
	values = validValues()
	values["OAUTH_CLIENT_REGISTRATIONS"] = `{"synthetic-secret":{},"synthetic-secret":{}}`
	if !errors.Is(rejectValues(t, values), jsontext.ErrDuplicateName) {
		t.Fatal("JSON 중복 이름 원인이 소실됐다")
	}
	values = validValues()
	values["ACCOUNT_PLAN_LIMITS"] = `{"019a0000-abcd-7000-8000-000000000001":{},"019A0000-ABCD-7000-8000-000000000001":{}}`
	err = rejectValues(t, values)
	if strings.Contains(err.Error(), "019a0000") || strings.Contains(err.Error(), "019A0000") {
		t.Fatal("정규화 중복 계정이 오류에 노출됐다")
	}
}
