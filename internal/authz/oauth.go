package authz

import (
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// OAuthConfig는 구성 계층에서 형식 검증을 마친 고정 공개 resource와 사전 등록이다.
// 인증 계층은 요청 값을 이 등록과 대조하며 URI를 자동 등록하거나 외부에서 조회하지 않는다.
type OAuthConfig struct {
	Resource string
	Clients  map[string]OAuthClient
}

// OAuthClient는 공개 client의 허용 callback·resource와 포트 대조 정책이다.
type OAuthClient struct {
	RedirectURIs []string
	Resources    []string
	// LoopbackPortPolicy는 exact 또는 same_ip다. 빈 값의 기본 해석은 구성 계층이 맡는다.
	LoopbackPortPolicy string
}

// ErrInvalidAuthorizationTarget은 client·callback·resource의 사전 등록 결합 위반이다.
var ErrInvalidAuthorizationTarget = errors.New("invalid_authorization_target")

func copyOAuthConfig(input *OAuthConfig) (*OAuthConfig, error) {
	if input == nil {
		return nil, nil
	}
	if input.Resource == "" || len(input.Clients) == 0 {
		return nil, errors.New("OAuth 공개 구성 또는 사전 등록이 비어 있다")
	}
	result := &OAuthConfig{Resource: input.Resource, Clients: make(map[string]OAuthClient, len(input.Clients))}
	for id, client := range input.Clients {
		if id == "" || len(client.RedirectURIs) == 0 || len(client.Resources) == 0 || (client.LoopbackPortPolicy != "exact" && client.LoopbackPortPolicy != "same_ip") {
			return nil, errors.New("OAuth 사전 등록 구성이 올바르지 않다")
		}
		client.RedirectURIs, client.Resources = slices.Clone(client.RedirectURIs), slices.Clone(client.Resources)
		result.Clients[id] = client
	}
	return result, nil
}

// ValidateAuthorizationTarget은 오류 리디렉션 전에도 필요한 등록 결합을 대조한다.
// 미등록 callback으로 이동하거나 요청 값을 정규화해 허용 목록을 넓히지 않는다.
func (s *Service) ValidateAuthorizationTarget(clientID, callback, resource string) error {
	if s.oauth == nil {
		return ErrInvalidAuthorizationTarget
	}
	client, exists := s.oauth.Clients[clientID]
	if !exists || resource != s.oauth.Resource || !slices.Contains(client.Resources, resource) {
		return ErrInvalidAuthorizationTarget
	}
	for _, registered := range client.RedirectURIs {
		if callback == registered || (client.LoopbackPortPolicy == "same_ip" && sameLoopbackCallback(registered, callback)) {
			return nil
		}
	}
	return ErrInvalidAuthorizationTarget
}

func sameLoopbackCallback(registered, candidate string) bool {
	if strings.TrimSpace(candidate) != candidate || strings.Contains(candidate, "#") {
		return false
	}
	allowed, err := url.Parse(registered)
	if err != nil || allowed.Scheme != "http" {
		return false
	}
	requested, err := url.Parse(candidate)
	if err != nil || requested.Scheme != "http" || requested.User != nil || requested.Hostname() != allowed.Hostname() {
		return false
	}
	address, err := netip.ParseAddr(requested.Hostname())
	if err != nil || !address.IsLoopback() || address.Is4In6() || address.Zone() != "" {
		return false
	}
	port, err := strconv.ParseUint(requested.Port(), 10, 16)
	if err != nil || port == 0 {
		return false
	}
	// 원문 경로·query·escape를 유지하고 명시 포트만 등록 값으로 되돌려 비교한다.
	// net/url이 scheme을 소문자로 바꾸므로 String으로 재직렬화하지 않고 원문을 대조한다.
	scheme, authorityAndPath, ok := strings.Cut(candidate, "://")
	if !ok {
		return false
	}
	suffix, ok := strings.CutPrefix(authorityAndPath, requested.Host)
	return ok && scheme+"://"+allowed.Host+suffix == registered
}
