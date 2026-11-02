package authz

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"agent_context_sharing/internal/store"
)

// OAuthConfig는 고정 공개 resource와 동적 등록 저장소이다.
// 인증 계층은 요청 값을 이 등록과 대조하며 인가 요청으로 등록하거나 외부에서 조회하지 않는다.
type OAuthConfig struct {
	Resource string
	Clients  OAuthClientStore
}

// OAuthClientStore는 동적 등록 저장과 모든 인증 경로의 공통 조회 경계다.
type OAuthClientStore interface {
	RegisterOAuthClient(context.Context, store.OAuthClient) error
	OAuthClient(context.Context, string) (store.OAuthClient, error)
}

type OAuthClient = store.OAuthClient

// ErrInvalidAuthorizationTarget은 client·callback·resource의 동적 등록 결합 위반이다.
var ErrInvalidAuthorizationTarget = errors.New("invalid_authorization_target")

func copyOAuthConfig(input *OAuthConfig) (*OAuthConfig, error) {
	if input == nil {
		return nil, nil
	}
	if input.Resource == "" || input.Clients == nil {
		return nil, errors.New("OAuth 공개 구성 또는 등록 저장소가 없다")
	}
	return &OAuthConfig{Resource: input.Resource, Clients: input.Clients}, nil
}

func (s *Service) oauthClient(ctx context.Context, id string) (OAuthClient, error) {
	if err := ctx.Err(); err != nil {
		return OAuthClient{}, err
	}
	if s.oauth == nil || id == "" {
		return OAuthClient{}, ErrInvalidAuthorizationTarget
	}
	client, err := s.oauth.Clients.OAuthClient(ctx, id)
	if ctx.Err() != nil {
		return OAuthClient{}, ctx.Err()
	}
	if errors.Is(err, store.ErrNotFound) {
		return OAuthClient{}, ErrInvalidAuthorizationTarget
	}
	if err != nil {
		return OAuthClient{}, &credentialFailure{ErrUnavailable, err}
	}
	return client, ctx.Err()
}

// ValidateAuthorizationTarget은 오류 리디렉션 전에도 필요한 등록 결합을 대조한다.
// 미등록 callback으로 이동하거나 요청 값을 정규화해 허용 목록을 넓히지 않는다.
func (s *Service) ValidateAuthorizationTarget(ctx context.Context, clientID, callback, resource string) error {
	if s.oauth == nil {
		return ErrInvalidAuthorizationTarget
	}
	client, err := s.oauthClient(ctx, clientID)
	if err != nil {
		return err
	}
	if resource != s.oauth.Resource || !slices.Contains(client.Resources, resource) {
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
