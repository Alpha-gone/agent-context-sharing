package config

import (
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9_](?:[A-Za-z0-9_.-]*[A-Za-z0-9_.])?$`)

func validateAddress(address string) error {
	if hasControl(address) {
		return invalid("HTTP_ADDR", "TCP 주소 형식이 아니다", nil)
	}
	address = strings.TrimSpace(address)
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return invalid("HTTP_ADDR", "포트를 포함한 TCP 주소가 아니다", err)
	}
	if _, err := netip.ParseAddr(host); host != "" && err != nil && !hostnamePattern.MatchString(host) {
		return invalid("HTTP_ADDR", "유효한 TCP host가 아니다", nil)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return invalid("HTTP_ADDR", "유효한 TCP 포트가 아니다", err)
	}
	return nil
}

func validateDatabaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return invalid("DATABASE_URL", "URL 형식이 아니다", err)
	}
	if (u.Scheme != "postgres" && u.Scheme != "postgresql") || !validHost(u) || strings.Contains(raw, "#") {
		return invalid("DATABASE_URL", "host가 있는 PostgreSQL URL이 아니다", nil)
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return invalid("DATABASE_URL", "query 형식이 아니다", err)
	}
	return nil
}

func parseHTTPURL(name, raw string) (*url.URL, error) {
	if hasControl(raw) {
		return nil, invalid(name, "URL에 제어 문자가 있다", nil)
	}
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalid(name, "URL 형식이 아니다", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || !validHost(u) {
		return nil, invalid(name, "host가 있는 절대 HTTP URL이 아니다", nil)
	}
	return u, nil
}

// net/url은 URL 구문을 해석하지만 포트 범위·빈 host·zone을 모두 제한하지는 않는다.
func validHost(u *url.URL) bool {
	if u.Hostname() == "" || u.Opaque != "" || strings.ContainsAny(u.Hostname(), "%*\\") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	// 대괄호 host는 literal IPv6만 받는다.
	if strings.HasPrefix(u.Host, "[") {
		address, err := netip.ParseAddr(u.Hostname())
		if err != nil || !address.Is6() {
			return false
		}
	} else if !hostnamePattern.MatchString(u.Hostname()) {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return false
		}
	}
	return true
}

func parseOriginURL(name, raw string) (*url.URL, error) {
	u, err := parseHTTPURL(name, raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.Path != "" || u.User != nil || strings.ContainsAny(raw, "?#") {
		return nil, invalid(name, "경로·query·fragment·사용자 정보 없는 HTTPS origin이어야 한다", nil)
	}
	return u, nil
}

func parseList(name, raw string) ([]string, error) {
	var values []string
	for value := range strings.SplitSeq(raw, ",") {
		value = strings.TrimSpace(value)
		if value == "" || hasControl(value) {
			return nil, invalid(name, "빈 값 또는 제어 문자가 있다", nil)
		}
		if slices.Contains(values, value) {
			return nil, invalid(name, "중복 값이 있다", nil)
		}
		values = append(values, value)
	}
	return values, nil
}

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
		if slices.Contains(prefixes, prefix) {
			return nil, invalid(name, "정규화 후 중복 주소 대역이 있다", nil)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func parsePrefix(name, value string) (netip.Prefix, error) {
	if address, err := netip.ParseAddr(value); err == nil && address.Zone() == "" {
		address = address.Unmap()
		return netip.PrefixFrom(address, address.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, invalid(name, "주소 또는 CIDR 대역이 아니다", err)
	}
	if prefix.Addr().Is4In6() || prefix.Masked() != prefix {
		return netip.Prefix{}, invalid(name, "IPv4-mapped 대역 또는 호스트 비트가 남은 대역이다", nil)
	}
	return prefix, nil
}
