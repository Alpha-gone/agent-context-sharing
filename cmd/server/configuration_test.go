package main

import (
	"errors"
	"maps"
	"os"
	"strings"
	"testing"

	"agent_context_sharing/internal/config"
)

func serverConfiguration(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			values[key] = strings.Trim(value, "'")
		}
	}
	values["GEMINI_API_KEY"] = "synthetic-key"
	return values
}

func TestConfigurationRejectsBeforeResourceStartup(t *testing.T) {
	base := serverConfiguration(t)
	for key, value := range map[string]string{
		"EMBEDDING_PROVIDER": "unsupported", "MCP_RESOURCE_URL": "http://resource.test/mcp",
		"MCP_ALLOWED_ORIGINS":    "*",
		"DIAGNOSTIC_INSTANCE_ID": "../slot", "DIAGNOSTIC_LOG_DIR": "/tmp/logs",
		"DIAGNOSTIC_LOG_LEVEL": "TRACE", "AUTHORIZATION_SERVER_URL": "https://host#",
		"SEARCH_SEMANTIC_SIMILARITY_THRESHOLD": "NaN", "TRUSTED_PROXY_CIDRS": "",
		"ACCOUNT_PLAN_LIMITS": `{"019a0000-abcd-7000-8000-000000000001":{},"019A0000-ABCD-7000-8000-000000000001":{}}`,
	} {
		t.Run(key, func(t *testing.T) {
			values := maps.Clone(base)
			values[key] = value
			starts := 0
			err := configureAndRun(func(key string) string { return values[key] }, func(config.Config) error {
				starts++
				return nil
			})
			if err == nil || starts != 0 || !strings.HasPrefix(err.Error(), "배포 구성 검증:") {
				t.Fatal("잘못된 구성이 자원 조립 전에 차단되지 않았다")
			}
		})
	}
}

func TestConfigurationPassesValidatedInputsOnce(t *testing.T) {
	values := serverConfiguration(t)
	expected := errors.New("합성 조립 실패")
	starts := 0
	err := configureAndRun(func(key string) string { return values[key] }, func(cfg config.Config) error {
		starts++
		if cfg.MCPResourceURL.String() != "https://localhost:8080/mcp" || cfg.Diagnostic.InstanceID != "development" {
			t.Fatal("검증된 조립 입력이 보존되지 않았다")
		}
		return expected
	})
	if starts != 1 || !errors.Is(err, expected) {
		t.Fatalf("조립 호출·원인 보존 = %d·%v", starts, err)
	}
}

func TestDatabaseURLRejectsBeforeResourceStartup(t *testing.T) {
	for _, raw := range []string{
		"postgres://user:synthetic-secret@localhost/db?application_name=%00",
		"postgres://user:synthetic-secret@localhost/db?pool_max_conns=4&pool_max_conns=synthetic-secret",
	} {
		values := serverConfiguration(t)
		values["DATABASE_URL"] = raw
		starts := 0
		err := configureAndRun(func(key string) string { return values[key] }, func(config.Config) error { starts++; return nil })
		field, ok := errors.AsType[*config.Error](err)
		if starts != 0 || !ok || field.Key != "DATABASE_URL" || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("접속 구성 오류의 조립 차단/비밀 비노출 위반")
		}
	}
}
