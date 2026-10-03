package config

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

func validEnvironment() map[string]string {
	return map[string]string{
		"AGENT_CONTEXT_CLIENT_REMOTE_URL":      "https://example.com/mcp",
		"AGENT_CONTEXT_CLIENT_ID":              "registered-client",
		"AGENT_CONTEXT_CLIENT_AGENT_ID":        "0198e7c0-0000-7000-8000-000000000001",
		"AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT":    "5m",
		"AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "30s",
	}
}

func TestLoadValidConfiguration(t *testing.T) {
	env := validEnvironment()
	cfg, err := Load(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RemoteURL() != "https://example.com/mcp" || cfg.ClientID() != "registered-client" ||
		cfg.AgentID().String() != env["AGENT_CONTEXT_CLIENT_AGENT_ID"] ||
		cfg.AuthTimeout() != 5*time.Minute || cfg.RequestTimeout() != 30*time.Second ||
		cfg.ToolPolicy() != PolicyAll || len(cfg.ToolAllowlist()) != 0 {
		t.Fatalf("검증된 구성이 예상과 다릅니다: policy=%q", cfg.ToolPolicy())
	}
	env["AGENT_CONTEXT_CLIENT_TOOL_POLICY"] = "allowlist"
	env["AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST"] = "graph_list, node_get"
	cfg, err = Load(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	list := cfg.ToolAllowlist()
	if len(list) != 2 || list[0] != "graph_list" || list[1] != "node_get" {
		t.Fatalf("allowlist = %v", list)
	}
	list[0] = "graph_create"
	if cfg.ToolAllowlist()[0] != "graph_list" {
		t.Fatal("구성의 allowlist가 외부 변경에 노출됐습니다")
	}
}

func TestLoadRejectsInvalidConfigurationWithoutLeakingValues(t *testing.T) {
	tests := []struct {
		name, key, value string
	}{
		{"missing remote", "AGENT_CONTEXT_CLIENT_REMOTE_URL", ""},
		{"http", "AGENT_CONTEXT_CLIENT_REMOTE_URL", "http://example.com/mcp"},
		{"wrong path", "AGENT_CONTEXT_CLIENT_REMOTE_URL", "https://example.com/other"},
		{"escaped path", "AGENT_CONTEXT_CLIENT_REMOTE_URL", "https://example.com/%6dcp"},
		{"query", "AGENT_CONTEXT_CLIENT_REMOTE_URL", "https://example.com/mcp?secret=private"},
		{"empty query", "AGENT_CONTEXT_CLIENT_REMOTE_URL", "https://example.com/mcp?"},
		{"userinfo", "AGENT_CONTEXT_CLIENT_REMOTE_URL", "https://private@example.com/mcp"},
		{"fragment", "AGENT_CONTEXT_CLIENT_REMOTE_URL", "https://example.com/mcp#private"},
		{"empty client", "AGENT_CONTEXT_CLIENT_ID", ""},
		{"spaced client", "AGENT_CONTEXT_CLIENT_ID", " registered-client"},
		{"wrong UUID version", "AGENT_CONTEXT_CLIENT_AGENT_ID", "550e8400-e29b-41d4-a716-446655440000"},
		{"noncanonical UUID", "AGENT_CONTEXT_CLIENT_AGENT_ID", "0198E7C0-0000-7000-8000-000000000001"},
		{"zero auth timeout", "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT", "0s"},
		{"negative request timeout", "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT", "-1s"},
		{"invalid policy", "AGENT_CONTEXT_CLIENT_TOOL_POLICY", "private"},
		{"unexpected allowlist", "AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST", "graph_list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnvironment()
			env[tt.key] = tt.value
			_, err := Load(func(key string) string { return env[key] })
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("%s 오류가 없습니다: %v", tt.key, err)
			}
			if strings.Contains(tt.value, "private") && strings.Contains(err.Error(), "private") {
				t.Fatal("구성 오류에 민감한 입력값이 노출됐습니다")
			}
		})
	}
}

func TestLoadRejectsInvalidAllowlist(t *testing.T) {
	for _, list := range []string{"", ",graph_list", "graph_list,", "graph_list,,node_get", "graph_list,graph_list", "unknown"} {
		t.Run(list, func(t *testing.T) {
			env := validEnvironment()
			env["AGENT_CONTEXT_CLIENT_TOOL_POLICY"] = "allowlist"
			env["AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST"] = list
			if _, err := Load(func(key string) string { return env[key] }); err == nil {
				t.Fatal("유효하지 않은 allowlist를 수락했습니다")
			}
		})
	}
}

func TestAllowedToolNamesMatchSharedContract(t *testing.T) {
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(contract.Manifest(), &tools); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		env := validEnvironment()
		env["AGENT_CONTEXT_CLIENT_TOOL_POLICY"] = "allowlist"
		env["AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST"] = tool.Name
		cfg, err := Load(func(key string) string { return env[key] })
		if err != nil || !cfg.PublicationPolicy().Allows(tool.Name) {
			t.Fatalf("공개 계약 도구 %q가 구성 정책에 없습니다: %v", tool.Name, err)
		}
	}
}

func TestLoadedPolicyMatchesContractClassification(t *testing.T) {
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(contract.Manifest(), &tools); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"all", "read_only", "allowlist"} {
		env := validEnvironment()
		env["AGENT_CONTEXT_CLIENT_TOOL_POLICY"] = mode
		if mode == "allowlist" {
			env["AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST"] = "graph_list, node_create"
		}
		cfg, err := Load(func(key string) string { return env[key] })
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range tools {
			kind, ok := contract.Classify(tool.Name)
			if !ok {
				t.Fatalf("분류되지 않은 도구: %s", tool.Name)
			}
			want := mode == "all" || mode == "read_only" && kind == contract.Read || mode == "allowlist" && (tool.Name == "graph_list" || tool.Name == "node_create")
			if cfg.PublicationPolicy().Allows(tool.Name) != want {
				t.Fatalf("%s/%s의 정책이 다릅니다", mode, tool.Name)
			}
		}
	}
}

func TestInvalidPolicyIsConfigurationError(t *testing.T) {
	for _, tc := range []struct{ mode, list string }{
		{"private", ""}, {"allowlist", ""}, {"all", "graph_list"},
		{"allowlist", "unknown"}, {"allowlist", "graph_list, graph_list"},
		{"allowlist", "graph_list,"}, {"allowlist", " "},
	} {
		env := validEnvironment()
		env["AGENT_CONTEXT_CLIENT_TOOL_POLICY"], env["AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST"] = tc.mode, tc.list
		_, err := Load(func(key string) string { return env[key] })
		if !errors.Is(err, contract.ErrConfiguration) || errors.Is(err, contract.ErrProtocol) {
			t.Fatalf("잘못된 정책의 오류 분류가 다릅니다: %v", err)
		}
		if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "unknown") {
			t.Fatal("오류에 입력값이 노출됐습니다")
		}
	}
}
