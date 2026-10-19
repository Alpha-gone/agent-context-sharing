package authz

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"time"

	"agent_context_sharing/internal/store"
)

// RefreshTokenStore는 회전·재사용 폐기와 서명을 같은 성공 경계에 묶는다.
type RefreshTokenStore interface {
	RotateOAuthRefreshToken(context.Context, store.OAuthRefreshExchange, func() time.Time, func(store.OAuthGrantBinding, time.Time) ([32]byte, error)) error
}

// Refresh는 현재 등록과 인가 결합을 유지하며 이전 갱신 자격을 한 번만 소비한다.
func (s *Service) Refresh(ctx context.Context, raw, clientID, resource, scope string) (OAuthTokens, error) {
	if err := ctx.Err(); err != nil {
		return OAuthTokens{}, err
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) != 32 || len(raw) != 43 || s.oauth == nil {
		return OAuthTokens{}, &OAuthError{Code: "invalid_grant"}
	}
	client, found := s.oauth.Clients[clientID]
	if !found || resource != s.oauth.Resource || !slices.Contains(client.Resources, resource) {
		return OAuthTokens{}, &OAuthError{Code: "invalid_grant"}
	}
	if scope != "" && scope != "agent-context" {
		return OAuthTokens{}, &OAuthError{Code: "invalid_scope"}
	}
	if s.config.RefreshTokens == nil {
		return OAuthTokens{}, ErrUnavailable
	}
	signer, err := s.oauthSigner(ctx)
	if err != nil {
		return OAuthTokens{}, &credentialFailure{ErrUnavailable, err}
	}
	var result OAuthTokens
	err = s.config.RefreshTokens.RotateOAuthRefreshToken(ctx, store.OAuthRefreshExchange{Hash: sha256.Sum256([]byte(raw)), ClientID: clientID, Resource: resource, Scope: scope}, s.now,
		func(grant store.OAuthGrantBinding, now time.Time) ([32]byte, error) {
			var hash [32]byte
			var err error
			result, hash, err = s.issueOAuthTokens(signer, grant, now)
			return hash, err
		})
	if err := ctx.Err(); err != nil {
		return OAuthTokens{}, err
	}
	if errors.Is(err, store.ErrInvalidOAuthGrant) {
		return OAuthTokens{}, &OAuthError{Code: "invalid_grant"}
	}
	if err != nil {
		return OAuthTokens{}, &credentialFailure{ErrUnavailable, err}
	}
	return result, nil
}
