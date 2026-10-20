package authz

import (
	"context"
	"crypto/sha256"
	"time"

	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// oauthSigner는 저장 트랜잭션 밖에서 키를 준비해 잠금 중 추가 풀 연결을 피한다.
func (s *Service) oauthSigner(ctx context.Context) (jose.Signer, error) {
	key, err := s.activeKey(ctx)
	if err != nil {
		return nil, err
	}
	private, err := privateSigningKey(key)
	if err != nil {
		return nil, err
	}
	return jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", key.ID))
}

// issueOAuthTokens는 최초 교환·갱신 회전이 공유하는 DB 접근 없는 발급 경계다.
// 저장 콜백에는 갱신 해시만 돌려주고 원문은 커밋 성공 뒤 token 응답으로만 전달한다.
func (s *Service) issueOAuthTokens(signer jose.Signer, grant store.OAuthGrantBinding, now time.Time) (OAuthTokens, [32]byte, error) {
	refresh, err := secret()
	if err != nil {
		return OAuthTokens{}, [32]byte{}, err
	}
	jti, err := secret()
	if err != nil {
		return OAuthTokens{}, [32]byte{}, err
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: s.config.Issuer, Subject: grant.AccountID.String(), Audience: jwt.Audience{grant.Resource}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)), ID: jti}).Claims(map[string]any{"scope": grant.Scope, "client_id": grant.ClientID, "grant_id": grant.ID.String()}).Serialize()
	if err != nil {
		return OAuthTokens{}, [32]byte{}, err
	}
	return OAuthTokens{AccessToken: raw, TokenType: "Bearer", ExpiresIn: 3600, Scope: grant.Scope, RefreshToken: refresh}, sha256.Sum256([]byte(refresh)), nil
}
