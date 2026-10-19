package authz

import (
	"context"
	"errors"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// ErrInsufficientScope는 유효한 자격의 scope가 보호 요청에 부족함을 나타낸다.
var ErrInsufficientScope = errors.New("insufficient_scope")

// AccessTokenStore는 검증 캐시와 분리된 매 요청 공유 상태 경계다.
type AccessTokenStore interface {
	OAuthAccessAllowed(context.Context, store.OAuthAccessBinding, time.Time) (bool, error)
}

// BearerPrincipal에는 검증된 결합만 담고 토큰·등급·호스트 식별 정보를 넣지 않는다.
type BearerPrincipal struct {
	AccountID model.ID
	ClientID  string
	GrantID   model.ID
}

// VerifyBearer는 고정 resource·등록·공유 폐기를 확인한다. 등급은 다음 권한 경계가 조회한다.
func (s *Service) VerifyBearer(ctx context.Context, raw string) (BearerPrincipal, error) {
	if err := ctx.Err(); err != nil {
		return BearerPrincipal{}, err
	}
	if s.oauth == nil || s.config.AccessTokens == nil {
		return BearerPrincipal{}, ErrUnavailable
	}
	var access struct {
		Scope    string `json:"scope"`
		ClientID string `json:"client_id"`
		GrantID  string `json:"grant_id"`
	}
	claims, err := s.signedClaims(ctx, raw, s.oauth.Resource, &access)
	if err != nil {
		return BearerPrincipal{}, err
	}
	account, accountErr := model.ParseID(claims.Subject)
	grant, grantErr := model.ParseID(access.GrantID)
	client, registered := s.oauth.Clients[access.ClientID]
	if accountErr != nil || grantErr != nil || access.Scope == "" || !registered || !slices.Contains(client.Resources, s.oauth.Resource) || claims.Expiry.Time().Sub(claims.IssuedAt.Time()) > time.Hour {
		return BearerPrincipal{}, ErrInvalidCredential
	}
	allowed, err := s.config.AccessTokens.OAuthAccessAllowed(ctx, store.OAuthAccessBinding{
		AccountID: account, GrantID: grant, ClientID: access.ClientID, Resource: s.oauth.Resource, Scope: access.Scope, TokenID: claims.ID, IssuedAt: claims.IssuedAt.Time(),
	}, s.now().UTC())
	if err := ctx.Err(); err != nil {
		return BearerPrincipal{}, err
	}
	if err != nil {
		return BearerPrincipal{}, &credentialFailure{ErrUnavailable, err}
	}
	if !allowed || !s.now().UTC().Before(claims.Expiry.Time()) {
		return BearerPrincipal{}, ErrInvalidCredential
	}
	if access.Scope != "agent-context" {
		return BearerPrincipal{}, ErrInsufficientScope
	}
	return BearerPrincipal{AccountID: account, ClientID: access.ClientID, GrantID: grant}, nil
}
