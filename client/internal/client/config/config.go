// Package config는 MCP 클라이언트의 기동 환경 변수를 한 번 검증해 보관한다.
package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
)

// Policy는 호스트에 공개할 도구 범위를 나타낸다.
type Policy string

const (
	// PolicyAll은 검증된 원격 도구를 모두 공개한다.
	PolicyAll Policy = "all"
	// PolicyReadOnly는 읽기 도구만 공개한다.
	PolicyReadOnly Policy = "read_only"
	// PolicyAllowlist는 명시된 도구만 공개한다.
	PolicyAllowlist Policy = "allowlist"
)

// Config는 기동 전에 검증한, 프로세스 수명 동안 불변인 설정이다.
type Config struct {
	remoteURL      string
	clientID       string
	agentID        uuid.UUID
	authTimeout    time.Duration
	requestTimeout time.Duration
	policy         Policy
	allowlist      []string
	publication    contract.Policy
}

// RemoteURL은 검증한 원격 MCP 주소를 반환한다.
func (c Config) RemoteURL() string { return c.remoteURL }

// ClientID는 등록된 공개 클라이언트 식별자를 반환한다.
func (c Config) ClientID() string { return c.clientID }

// AgentID는 기동 시 고정된 UUIDv7 행위 에이전트 식별자를 반환한다.
func (c Config) AgentID() uuid.UUID { return c.agentID }

// AuthTimeout은 브라우저 인가 전체의 제한 시간을 반환한다.
func (c Config) AuthTimeout() time.Duration { return c.authTimeout }

// RequestTimeout은 호출별 호스트 제한 시간이 없을 때의 제한 시간을 반환한다.
func (c Config) RequestTimeout() time.Duration { return c.requestTimeout }

// ToolPolicy는 도구 공개 정책을 반환한다.
func (c Config) ToolPolicy() Policy { return c.policy }

// ToolAllowlist는 검증된 도구 이름을 복사해 반환한다.
func (c Config) ToolAllowlist() []string { return slices.Clone(c.allowlist) }

// PublicationPolicy는 기동 시 검증한 목록·호출 공통 공개 정책을 반환한다.
func (c Config) PublicationPolicy() contract.Policy { return c.publication }

// Load는 환경 변수를 읽고 외부 접근 전에 모든 기동 값을 검증한다.
func Load(getenv func(string) string) (Config, error) {
	var cfg Config
	if getenv == nil {
		return cfg, fmt.Errorf("환경 변수 읽기 함수가 없습니다")
	}

	remote := getenv("AGENT_CONTEXT_CLIENT_REMOTE_URL")
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "" ||
		u.User != nil || u.Path != "/mcp" || u.EscapedPath() != "/mcp" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return cfg, fmt.Errorf("AGENT_CONTEXT_CLIENT_REMOTE_URL: query·fragment·사용자 정보가 없는 절대 HTTPS /mcp 주소가 필요합니다")
	}
	cfg.remoteURL = u.String()

	cfg.clientID = getenv("AGENT_CONTEXT_CLIENT_ID")
	if cfg.clientID == "" || strings.TrimSpace(cfg.clientID) != cfg.clientID {
		return Config{}, fmt.Errorf("AGENT_CONTEXT_CLIENT_ID: 공백이 아닌 등록 식별자가 필요합니다")
	}

	agentText := getenv("AGENT_CONTEXT_CLIENT_AGENT_ID")
	cfg.agentID, err = uuid.Parse(agentText)
	if err != nil || cfg.agentID.String() != agentText || cfg.agentID[6]>>4 != 7 || cfg.agentID[8]&0xc0 != 0x80 {
		return Config{}, fmt.Errorf("AGENT_CONTEXT_CLIENT_AGENT_ID: 표준 문자열 형식의 UUIDv7이 필요합니다")
	}

	cfg.authTimeout, err = positiveDuration(getenv("AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT"))
	if err != nil {
		return Config{}, fmt.Errorf("AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT: 양수 Go 기간이 필요합니다")
	}
	cfg.requestTimeout, err = positiveDuration(getenv("AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT"))
	if err != nil {
		return Config{}, fmt.Errorf("AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT: 양수 Go 기간이 필요합니다")
	}

	cfg.policy = Policy(getenv("AGENT_CONTEXT_CLIENT_TOOL_POLICY"))
	if cfg.policy == "" {
		cfg.policy = PolicyAll
	}
	list := getenv("AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST")
	if list != "" {
		for name := range strings.SplitSeq(list, ",") {
			cfg.allowlist = append(cfg.allowlist, strings.TrimSpace(name))
		}
	}
	cfg.publication, err = contract.NewPolicy(string(cfg.policy), cfg.allowlist)
	if err != nil {
		return Config{}, fmt.Errorf("AGENT_CONTEXT_CLIENT_TOOL_POLICY·AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST: %w", err)
	}
	return cfg, nil
}

func positiveDuration(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("유효한 양수 기간이 아닙니다")
	}
	return duration, nil
}
