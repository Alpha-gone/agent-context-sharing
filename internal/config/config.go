// Package config는 배포 구성 값을 읽고 애플리케이션 기동 전에 검증한다.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"agent_context_sharing/internal/plan"
)

// Environment는 구성 값을 읽는 환경 변수 경계다. 테스트는 이 함수를 대체해 외부 환경에
// 의존하지 않고 구성 계약을 검증한다.
type Environment func(string) string

// TLSMode는 TLS를 어느 계층에서 끝내는지 나타낸다.
type TLSMode string

const (
	// TLSModeDirect는 애플리케이션이 TLS 연결을 직접 받는 배치를 뜻한다.
	TLSModeDirect TLSMode = "direct"
	// TLSModeProxy는 신뢰된 역방향 프록시가 TLS를 끝내는 배치를 뜻한다.
	TLSModeProxy TLSMode = "proxy"
)

// Config는 실행 중 필요한 배포 구성의 검증된 묶음이다. 비밀값은 로그로 전달하지 않는다.
type Config struct {
	// HTTPAddr 필드에는 HTTP 서버가 수신할 TCP 주소를 둔다.
	HTTPAddr string
	// DatabaseURL 필드에는 PostgreSQL 접속 문자열을 둔다.
	DatabaseURL string
	// GraphName 필드에는 단일 AGE 그래프 이름을 둔다.
	GraphName string
	// EmbeddingBaseURL 필드에는 임베딩 제공자 HTTP 주소를 둔다.
	EmbeddingBaseURL *url.URL
	// EmbeddingModel 필드에는 본문과 질의에 공통으로 쓸 모델 이름을 둔다.
	EmbeddingModel string
	// EmbeddingVectorType 필드에는 pgvector 저장 타입을 둔다.
	EmbeddingVectorType string
	// EmbeddingDimension 필드에는 모델 벡터 차원을 둔다.
	EmbeddingDimension int
	// OAuthClientIDs 필드에는 사전 등록한 OAuth 클라이언트 식별자를 둔다.
	OAuthClientIDs []string
	// OAuthRedirectURIs 필드에는 허용된 OAuth 콜백 주소를 둔다.
	OAuthRedirectURIs []*url.URL
	// BcryptCost 필드에는 새 계정 비밀번호 해시에 쓸 비용 계수를 둔다.
	BcryptCost int
	// AccountPlans 필드에는 계정별로 배정한 플랜 값을 둔다.
	AccountPlans plan.AccountPlans
	// TLSMode 필드에는 TLS 종단 배치를 둔다.
	TLSMode TLSMode
	// TrustedProxies 필드에는 전달 헤더를 신뢰할 역방향 프록시의 주소 대역을 둔다.
	// 비어 있으면 어떤 상대의 전달 헤더도 신뢰하지 않는다.
	TrustedProxies []netip.Prefix
	// TLSCertFile 필드에는 애플리케이션이 직접 TLS를 종단할 때 쓸 인증서 파일 경로를 둔다.
	TLSCertFile string
	// TLSKeyFile 필드에는 애플리케이션이 직접 TLS를 종단할 때 쓸 개인 키 파일 경로를 둔다.
	TLSKeyFile string
}

// graphNamePattern은 SQL 식별자 자리에 치환되는 AGE 그래프 이름을 제한한다.
var graphNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Load는 환경 변수에서 현재 단계에 필요한 SDD 배포 구성만 읽고 검증한다.
func Load(env Environment) (Config, error) {
	if env == nil {
		return Config{}, fmt.Errorf("환경 변수 읽기 함수가 없다")
	}

	cfg := Config{
		HTTPAddr:            strings.TrimSpace(env("HTTP_ADDR")),
		DatabaseURL:         strings.TrimSpace(env("DATABASE_URL")),
		GraphName:           strings.TrimSpace(env("AGE_GRAPH_NAME")),
		EmbeddingModel:      strings.TrimSpace(env("EMBEDDING_MODEL")),
		EmbeddingVectorType: strings.TrimSpace(env("EMBEDDING_VECTOR_TYPE")),
		TLSMode:             TLSMode(strings.TrimSpace(env("TLS_TERMINATION"))),
		TLSCertFile:         strings.TrimSpace(env("TLS_CERT_FILE")),
		TLSKeyFile:          strings.TrimSpace(env("TLS_KEY_FILE")),
	}

	if err := validateAddress(cfg.HTTPAddr); err != nil {
		return Config{}, fmt.Errorf("HTTP_ADDR: %w", err)
	}
	if err := validateDatabaseURL(cfg.DatabaseURL); err != nil {
		return Config{}, fmt.Errorf("DATABASE_URL: %w", err)
	}
	if !graphNamePattern.MatchString(cfg.GraphName) {
		return Config{}, fmt.Errorf("AGE_GRAPH_NAME %q가 영문 소문자, 숫자와 밑줄 형식이 아니다", cfg.GraphName)
	}

	baseURL, err := parseHTTPURL("EMBEDDING_BASE_URL", env("EMBEDDING_BASE_URL"))
	if err != nil {
		return Config{}, err
	}
	cfg.EmbeddingBaseURL = baseURL
	if cfg.EmbeddingModel == "" {
		return Config{}, fmt.Errorf("EMBEDDING_MODEL이 비어 있다")
	}
	if !slices.Contains([]string{"vector", "halfvec", "bit"}, cfg.EmbeddingVectorType) {
		return Config{}, fmt.Errorf("EMBEDDING_VECTOR_TYPE %q가 허용된 값이 아니다", cfg.EmbeddingVectorType)
	}
	dimension, err := strconv.Atoi(strings.TrimSpace(env("EMBEDDING_DIMENSION")))
	if err != nil || dimension <= 0 {
		return Config{}, fmt.Errorf("EMBEDDING_DIMENSION이 양의 정수가 아니다")
	}
	cfg.EmbeddingDimension = dimension

	clientIDs, err := parseList("OAUTH_CLIENT_IDS", env("OAUTH_CLIENT_IDS"))
	if err != nil {
		return Config{}, err
	}
	cfg.OAuthClientIDs = clientIDs
	redirects, err := parseURLs("OAUTH_REDIRECT_URIS", env("OAUTH_REDIRECT_URIS"))
	if err != nil {
		return Config{}, err
	}
	cfg.OAuthRedirectURIs = redirects
	bcryptCost, err := strconv.Atoi(strings.TrimSpace(env("BCRYPT_COST")))
	if err != nil || bcryptCost < 4 || bcryptCost > 31 {
		return Config{}, fmt.Errorf("BCRYPT_COST가 4에서 31 사이의 정수가 아니다")
	}
	cfg.BcryptCost = bcryptCost
	accountPlans, err := plan.ParseAccountPlans(strings.TrimSpace(env("ACCOUNT_PLAN_LIMITS")))
	if err != nil {
		return Config{}, fmt.Errorf("ACCOUNT_PLAN_LIMITS: %w", err)
	}
	cfg.AccountPlans = accountPlans

	trustedProxies, err := parsePrefixes("TRUSTED_PROXY_CIDRS", env("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return Config{}, err
	}
	cfg.TrustedProxies = trustedProxies
	switch cfg.TLSMode {
	case TLSModeDirect:
		if len(cfg.TrustedProxies) > 0 {
			return Config{}, fmt.Errorf("TLS_TERMINATION이 direct일 때 TRUSTED_PROXY_CIDRS는 비어 있어야 한다")
		}
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return Config{}, fmt.Errorf("TLS_TERMINATION이 direct일 때 TLS_CERT_FILE과 TLS_KEY_FILE이 필요하다")
		}
	case TLSModeProxy:
		if len(cfg.TrustedProxies) == 0 {
			return Config{}, fmt.Errorf("TLS_TERMINATION이 proxy일 때 TRUSTED_PROXY_CIDRS가 필요하다")
		}
		if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
			return Config{}, fmt.Errorf("TLS_TERMINATION이 proxy일 때 TLS_CERT_FILE과 TLS_KEY_FILE은 비어 있어야 한다")
		}
	default:
		return Config{}, fmt.Errorf("TLS_TERMINATION %q가 direct 또는 proxy가 아니다", cfg.TLSMode)
	}

	return cfg, nil
}

// validateAddress는 서버 수신 주소가 포트를 포함한 TCP 주소인지 확인한다.
func validateAddress(address string) error {
	if address == "" {
		return fmt.Errorf("값이 비어 있다")
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("TCP 주소 형식이 아니다: %w", err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("포트 %q가 유효하지 않다", port)
	}
	return nil
}

// validateDatabaseURL은 PostgreSQL 연결에 필요한 scheme과 host를 확인한다.
func validateDatabaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("값이 비어 있다")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 해석: %w", err)
	}
	if (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" {
		return fmt.Errorf("postgres 또는 postgresql scheme과 host가 필요하다")
	}
	return nil
}

// parseHTTPURL은 HTTP 또는 HTTPS URL 하나를 해석한다.
func parseHTTPURL(name, raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%s 해석: %w", name, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%s는 host가 있는 HTTP URL이어야 한다", name)
	}
	return parsed, nil
}

// parseList는 쉼표로 나눈 중복 없는 구성 값을 해석한다.
func parseList(name, raw string) ([]string, error) {
	var values []string
	for value := range strings.SplitSeq(raw, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s에 빈 값이 있다", name)
		}
		if slices.Contains(values, value) {
			return nil, fmt.Errorf("%s에 중복 값 %q가 있다", name, value)
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s가 비어 있다", name)
	}
	return values, nil
}

// parsePrefixes는 전달 헤더를 신뢰할 주소 대역 목록을 해석한다. 빈 값은 신뢰할 상대가
// 없다는 뜻이므로 오류가 아니라 빈 목록으로 다룬다.
func parsePrefixes(name, raw string) ([]netip.Prefix, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	values, err := parseList(name, raw)
	if err != nil {
		return nil, err
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := parsePrefix(name, value)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

// parsePrefix는 대역 또는 단일 주소 하나를 정규화된 접두로 해석한다. 호스트 비트가 남은
// 대역은 거부한다. 조용히 정규화하면 운영자가 의도한 범위와 실제 신뢰 범위가 달라진다.
func parsePrefix(name, value string) (netip.Prefix, error) {
	if address, err := netip.ParseAddr(value); err == nil {
		unmapped := address.Unmap()
		return netip.PrefixFrom(unmapped, unmapped.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%s의 %q가 주소 또는 CIDR 대역이 아니다", name, value)
	}
	if prefix.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("%s의 %q는 IPv4 대역으로 적어야 한다", name, value)
	}
	if prefix.Masked() != prefix {
		return netip.Prefix{}, fmt.Errorf("%s의 %q에 호스트 비트가 남아 있다. %s로 적는다", name, value, prefix.Masked())
	}
	return prefix, nil
}

// parseURLs는 쉼표로 나눈 HTTP URL 목록을 해석한다.
func parseURLs(name, raw string) ([]*url.URL, error) {
	values, err := parseList(name, raw)
	if err != nil {
		return nil, err
	}
	urls := make([]*url.URL, 0, len(values))
	for _, value := range values {
		parsed, err := parseHTTPURL(name, value)
		if err != nil {
			return nil, err
		}
		urls = append(urls, parsed)
	}
	return urls, nil
}
