package store

import (
	"context"
	"time"

	"agent_context_sharing/internal/model"
)

// OAuthAccessBinding은 서명 검증을 마친 접근 토큰의 공유 상태 대조 인자다.
type OAuthAccessBinding struct {
	AccountID, GrantID                 model.ID
	ClientID, Resource, Scope, TokenID string
	IssuedAt                           time.Time
}

// OAuthAccessAllowed는 한 스냅숏에서 계열 결합과 개별·계열 폐기를 확인한다.
// 갱신 자격 만료는 기존 접근 토큰의 수명을 줄이지 않는다. 호출도 갱신 활동이 아니다.
func (s *Store) OAuthAccessAllowed(ctx context.Context, input OAuthAccessBinding, now time.Time) (bool, error) {
	if !input.AccountID.IsV7() || !input.GrantID.IsV7() || input.ClientID == "" || input.Resource == "" || input.Scope == "" || input.TokenID == "" || input.IssuedAt.IsZero() || now.IsZero() {
		return false, ErrInvalidOAuthGrant
	}
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM public.oauth_grant
		WHERE grant_id=$1 AND account_id=$2 AND client_id=$3 AND resource=$4 AND scope=$5
		AND authorized_at <= $6 AND revoked_at IS NULL
	) AND NOT EXISTS (
		SELECT 1 FROM public.revoked_token WHERE token_id=$7 AND expires_at > $8
	)`, input.GrantID.String(), input.AccountID.String(), input.ClientID, input.Resource, input.Scope, input.IssuedAt, input.TokenID, now).Scan(&allowed)
	if err != nil {
		return false, storageError("oauth_access_lookup", err)
	}
	return allowed, nil
}
