package config

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestConfigDoesNotReadPreRegisteredClients(t *testing.T) {
	values := validValues()
	values["OAUTH_CLIENT_REGISTRATIONS"] = "invalid legacy input"
	if _, err := Load(func(key string) string {
		if key == "OAUTH_CLIENT_REGISTRATIONS" {
			t.Fatal("사전 등록 환경 변수를 읽었습니다")
		}
		return values[key]
	}); err != nil {
		t.Fatal(err)
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
