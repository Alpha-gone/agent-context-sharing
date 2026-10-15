package config

import (
	"slices"
	"strings"
	"unicode"
)

// allowedKeys에는 운영 SDD가 연 서버 구성만 둔다. Compose·시험·CLI 전용 키는
// 서버가 읽지 않으며, 환경 전체를 열거하거나 구성 원문을 오류에 담지 않는다.
var allowedKeys = []string{
	"HTTP_ADDR", "DATABASE_URL", "AGE_GRAPH_NAME",
	"EMBEDDING_PROVIDER", "GEMINI_API_KEY", "EMBEDDING_BASE_URL", "EMBEDDING_MODEL",
	"EMBEDDING_VECTOR_TYPE", "EMBEDDING_DIMENSION", "EMBEDDING_COLD_TABLESPACE",
	"SEARCH_CHANNEL_EXECUTION", "SEARCH_CHANNEL_CANDIDATE_LIMIT", "SEARCH_SEMANTIC_SIMILARITY_THRESHOLD",
	"SEARCH_FOLD_SIMILARITY_THRESHOLD", "SEARCH_GRAPH_STAGE", "SEARCH_GLOBAL_FALLBACK_ENABLED",
	"SEARCH_EVIDENCE_PATH_SELECTION_ENABLED", "SEARCH_QUERY_ADAPTIVE_ROUTING_ENABLED",
	"SEARCH_ADAPTIVE_DIRECT_SIMILARITY_THRESHOLD", "SEARCH_ADAPTIVE_DIRECT_MARGIN_THRESHOLD",
	"INDEX_TARGET_LAYERS", "INDEX_WORKER_PLACEMENT", "AUTHORIZATION_SERVER_URL", "BCRYPT_COST",
	"RELATION_ADJACENCY_WINDOW", "RELATION_SIMILARITY_THRESHOLD", "RELATION_PROPOSAL_LIMIT",
	"ACCOUNT_PLAN_LIMITS", "TLS_TERMINATION", "TRUSTED_PROXY_CIDRS", "TLS_CERT_FILE", "TLS_KEY_FILE",
	"MCP_RESOURCE_URL", "MCP_ALLOWED_ORIGINS", "OAUTH_CLIENT_REGISTRATIONS",
	"DIAGNOSTIC_LOG_DIR", "DIAGNOSTIC_INSTANCE_ID", "DIAGNOSTIC_LOG_LEVEL",
}

// AllowedKeys는 서버가 읽는 키의 독립된 목록을 반환한다.
func AllowedKeys() []string { return slices.Clone(allowedKeys) }

// Load는 허용된 키를 한 번씩 읽은 뒤 외부 자원에 접근하지 않고 검증한다.
// 실패하면 부분 구성을 반환하지 않는다.
func Load(env Environment) (Config, error) {
	if env == nil {
		return Config{}, invalid("config", "환경 변수 읽기 함수가 없다", nil)
	}
	values := make(map[string]string, len(allowedKeys))
	for _, name := range allowedKeys {
		values[name] = env(name)
	}
	return load(func(name string) string { return values[name] })
}

// Error는 입력 원문을 표시하지 않으면서 하위 해석 오류의 식별을 보존한다.
type Error struct {
	Key    string
	Reason string
	cause  error
}

func (err *Error) Error() string { return err.Key + ": " + err.Reason }
func (err *Error) Unwrap() error { return err.cause }

func invalid(key, reason string, cause error) error {
	return &Error{Key: key, Reason: reason, cause: cause}
}

func hasControl(value string) bool {
	return strings.ContainsFunc(value, unicode.IsControl)
}
