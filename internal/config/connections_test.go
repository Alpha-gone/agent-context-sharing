package config

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"strings"
	"testing"
)

func registrations(t *testing.T, callbacks []string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"test-client": map[string]any{
		"redirect_uris": callbacks, "resources": []string{"https://resource.test/mcp"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestOAuthRegistrationRejectsMalformedObjects(t *testing.T) {
	for i, raw := range []string{
		"", "null", "[]", "{}", "1", `"text"`, `{"client":`, `{} {}`,
		`{"client":null}`, `{"client":[]}`, `{"client":1}`, `{"client":{}}`,
		`{"":{}}`, `{"client id":{}}`, `{"client\u00a0id":{}}`, `{"client\u0000id":{}}`,
		`{"client":{},"cl\u0069ent":{}}`,
		`{"client":{"redirect_uris":null,"resources":["https://resource.test/mcp"]}}`,
		`{"client":{"redirect_uris":[],"resources":["https://resource.test/mcp"]}}`,
		`{"client":{"redirect_uris":[null],"resources":["https://resource.test/mcp"]}}`,
		`{"client":{"redirect_uris":[1],"resources":["https://resource.test/mcp"]}}`,
		`{"client":{"redirect_uris":"https://host/callback","resources":["https://resource.test/mcp"]}}`,
		`{"client":{"redirect_uris":["https://host/callback"],"resources":null}}`,
		`{"client":{"redirect_uris":["https://host/callback"],"resources":[]}}`,
		`{"client":{"redirect_uris":["https://host/callback"],"resources":[null]}}`,
		`{"client":{"redirect_uris":["https://host/callback"],"resources":["https://other.test/mcp"]}}`,
		`{"client":{"redirect_uris":["https://host/callback"],"resources":["https://resource.test/mcp","https://resource.test/mcp"]}}`,
		`{"client":{"redirect_uris":["https://host/callback"],"resources":["https://resource.test/mcp"],"synthetic-secret":true}}`,
		`{"client":{"Redirect_uris":["https://host/callback"],"resources":["https://resource.test/mcp"]}}`,
		`{"client":{"redirect_uris":["https://host/callback"],"redirect_uris":["https://host/other"],"resources":["https://resource.test/mcp"]}}`,
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			values := validValues()
			values["OAUTH_CLIENT_REGISTRATIONS"] = raw
			rejectValues(t, values)
		})
	}
}

func TestOAuthRegistrationRejectsUnsafeCallbacks(t *testing.T) {
	for i, uri := range []string{
		"", "/callback", "//host/callback", "ftp://host/callback", "http://host:49152/callback",
		"http://localhost:49152/callback", "http://192.0.2.1:49152/callback", "http://[::]:49152/callback",
		"http://127.0.0.1/callback", "http://127.0.0.1:0/callback", "http://127.0.0.1:65536/callback",
		"http://[::ffff:127.0.0.1]:49152/callback", "http://[::1%25lo0]:49152/callback",
		"https://host/callback#", "https://host/callback#fragment", "https://synthetic-secret@host/callback",
		" https://host/callback", "https://host/callback\n", "https://%synthetic-secret/callback",
		"https://host/callback?x=%synthetic-secret", "https://*.test/callback",
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			values := validValues()
			values["OAUTH_CLIENT_REGISTRATIONS"] = registrations(t, []string{uri})
			rejectValues(t, values)
		})
	}
	values := validValues()
	values["OAUTH_CLIENT_REGISTRATIONS"] = registrations(t, []string{"https://host/callback", "https://host/callback"})
	rejectValues(t, values)
}

func TestOAuthRegistrationPreservesCallbackAndPortPolicy(t *testing.T) {
	for _, uri := range []string{"https://host/callback?state=fixed", "https://host:8443/callback", "http://127.0.0.1:49152/callback", "http://[::1]:49152/callback"} {
		t.Run(uri, func(t *testing.T) {
			values := validValues()
			values["OAUTH_CLIENT_REGISTRATIONS"] = registrations(t, []string{uri})
			cfg, err := loadValues(values)
			if err != nil {
				t.Fatal(err)
			}
			client := cfg.OAuthClients["test-client"]
			if len(client.RedirectURIs) != 1 || client.RedirectURIs[0] != uri || client.LoopbackPortPolicy != LoopbackSameIP {
				t.Fatal("callback 원문 또는 기본 포트 정책이 달라졌다")
			}
		})
	}
	for _, policy := range []string{`null`, `""`, `true`, `[]`, `{}`, `"synthetic-secret"`} {
		values := validValues()
		values["OAUTH_CLIENT_REGISTRATIONS"] = strings.ReplaceAll(values["OAUTH_CLIENT_REGISTRATIONS"], `"exact"`, policy)
		rejectValues(t, values)
	}
	values := validValues()
	values["OAUTH_CLIENT_REGISTRATIONS"] = `{"first":{"redirect_uris":["https://host/callback"],"resources":["https://resource.test/mcp"]},"second":{"redirect_uris":["http://127.0.0.1:49152/callback"],"resources":["https://resource.test/mcp"],"loopback_port_policy":"exact"}}`
	cfg, err := loadValues(values)
	if err != nil || len(cfg.OAuthClients) != 2 || cfg.OAuthClients["first"].LoopbackPortPolicy != LoopbackSameIP || cfg.OAuthClients["second"].LoopbackPortPolicy != LoopbackExact {
		t.Fatal("서로 다른 등록과 정책이 보존되지 않았다")
	}
}

func TestConfigAcceptsBoundaryCombinations(t *testing.T) {
	for i, overrides := range []map[string]string{
		{"HTTP_ADDR": "[::1]:0"},
		{"DATABASE_URL": "postgresql://user:pass@[::1]:5432/db?sslmode=require"},
		{"EMBEDDING_DIMENSION": "128"}, {"EMBEDDING_DIMENSION": "2000"},
		{"EMBEDDING_DIMENSION": "3072", "EMBEDDING_VECTOR_TYPE": "halfvec"},
		{"SEARCH_SEMANTIC_SIMILARITY_THRESHOLD": "0", "RELATION_SIMILARITY_THRESHOLD": "-1"},
		{"SEARCH_SEMANTIC_SIMILARITY_THRESHOLD": "1", "RELATION_SIMILARITY_THRESHOLD": "1", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "1"},
		{"SEARCH_CHANNEL_EXECUTION": "sequential", "SEARCH_CHANNEL_CANDIDATE_LIMIT": "1"},
		{"SEARCH_QUERY_ADAPTIVE_ROUTING_ENABLED": "true", "SEARCH_GRAPH_STAGE": "references", "SEARCH_ADAPTIVE_DIRECT_SIMILARITY_THRESHOLD": "0", "SEARCH_ADAPTIVE_DIRECT_MARGIN_THRESHOLD": "1"},
		{"TLS_TERMINATION": "direct", "TRUSTED_PROXY_CIDRS": "", "TLS_CERT_FILE": "/secrets/cert", "TLS_KEY_FILE": "/secrets/key"},
		{"BCRYPT_COST": "4"}, {"BCRYPT_COST": "31"},
		{"DIAGNOSTIC_INSTANCE_ID": "x"}, {"DIAGNOSTIC_INSTANCE_ID": strings.Repeat("x", 64)},
		{"DIAGNOSTIC_LOG_DIR": "/srv/persistent/agent-context", "DIAGNOSTIC_LOG_LEVEL": "DEBUG"},
		{"DIAGNOSTIC_LOG_LEVEL": "WARN"}, {"DIAGNOSTIC_LOG_LEVEL": "ERROR"},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			values := validValues()
			maps.Copy(values, overrides)
			if _, err := loadValues(values); err != nil {
				t.Fatal(err)
			}
		})
	}
}
