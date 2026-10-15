package config

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"unicode"
)

// LoopbackPortPolicy는 OAuth 처리기가 callback을 대조할 때 적용할 포트 정책이다.
type LoopbackPortPolicy string

const (
	LoopbackSameIP LoopbackPortPolicy = "same_ip"
	LoopbackExact  LoopbackPortPolicy = "exact"
)

// OAuthClient는 공개 클라이언트의 검증된 사전 등록이다. 비밀을 포함하지 않는다.
// URI 원문과 포트 정책을 보존하며 실제 인가 시의 대조는 authz가 담당한다.
type OAuthClient struct {
	RedirectURIs       []string
	Resources          []string
	LoopbackPortPolicy LoopbackPortPolicy
}

func loadConnections(cfg *Config, env Environment) error {
	resource, err := parseHTTPURL("MCP_RESOURCE_URL", env("MCP_RESOURCE_URL"))
	if err != nil {
		return err
	}
	if resource.Scheme != "https" || resource.EscapedPath() != "/mcp" || resource.User != nil || strings.ContainsAny(env("MCP_RESOURCE_URL"), "?#") {
		return invalid("MCP_RESOURCE_URL", "query·fragment·사용자 정보 없는 HTTPS /mcp URI여야 한다", nil)
	}
	cfg.MCPResourceURL = resource
	origins, err := parseList("MCP_ALLOWED_ORIGINS", env("MCP_ALLOWED_ORIGINS"))
	if err != nil {
		return err
	}
	for _, origin := range origins {
		if _, err := parseOriginURL("MCP_ALLOWED_ORIGINS", origin); err != nil {
			return err
		}
	}
	cfg.MCPAllowedOrigins = origins
	cfg.OAuthClients, err = parseOAuthClients(env("OAUTH_CLIENT_REGISTRATIONS"), resource.String())
	return err
}

func parseOAuthClients(raw, resource string) (map[string]OAuthClient, error) {
	const key = "OAUTH_CLIENT_REGISTRATIONS"
	var entries map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, invalid(key, "등록 JSON 객체 형식이 아니다", err)
	}
	if len(entries) == 0 {
		return nil, invalid(key, "비어 있지 않은 등록 객체가 필요하다", nil)
	}
	clients := make(map[string]OAuthClient, len(entries))
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		if id == "" || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return nil, invalid(key, "client_id가 비어 있거나 공백·제어 문자가 있다", nil)
		}
		value := entries[id]
		if value.Kind() != jsontext.KindBeginObject {
			return nil, invalid(key, "각 등록 값은 객체여야 한다", nil)
		}
		var entry struct {
			RedirectURIs []string       `json:"redirect_uris"`
			Resources    []string       `json:"resources"`
			PortPolicy   jsontext.Value `json:"loopback_port_policy"`
		}
		if err := json.Unmarshal(value, &entry, json.RejectUnknownMembers(true)); err != nil {
			return nil, invalid(key, "등록 필드·타입·중복 이름이 올바르지 않다", err)
		}
		policy := LoopbackSameIP
		if len(entry.PortPolicy) > 0 {
			if entry.PortPolicy.Kind() != jsontext.KindString {
				return nil, invalid(key, "loopback_port_policy는 문자열이어야 한다", nil)
			}
			if err := json.Unmarshal(entry.PortPolicy, &policy); err != nil {
				return nil, invalid(key, "loopback_port_policy 형식이 아니다", err)
			}
		}
		if policy != LoopbackSameIP && policy != LoopbackExact {
			return nil, invalid(key, "loopback_port_policy는 same_ip 또는 exact여야 한다", nil)
		}
		if len(entry.RedirectURIs) == 0 || len(entry.Resources) == 0 {
			return nil, invalid(key, "callback·resource 목록은 비어 있을 수 없다", nil)
		}
		for i, callback := range entry.RedirectURIs {
			if slices.Contains(entry.RedirectURIs[:i], callback) || !validCallback(callback) {
				return nil, invalid(key, "callback이 중복되거나 허용된 절대 URI가 아니다", nil)
			}
		}
		for i, allowed := range entry.Resources {
			if allowed != resource || slices.Contains(entry.Resources[:i], allowed) {
				return nil, invalid(key, "resource가 중복되거나 고정 리소스와 다르다", nil)
			}
		}
		clients[id] = OAuthClient{RedirectURIs: entry.RedirectURIs, Resources: entry.Resources, LoopbackPortPolicy: policy}
	}
	return clients, nil
}

func validCallback(raw string) bool {
	if strings.TrimSpace(raw) != raw || strings.Contains(raw, "#") {
		return false
	}
	u, err := parseHTTPURL("OAUTH_CLIENT_REGISTRATIONS", raw)
	if err != nil || u.User != nil {
		return false
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	address, err := netip.ParseAddr(u.Hostname())
	return err == nil && address.IsLoopback() && !address.Is4In6() && address.Zone() == "" && u.Port() != ""
}
