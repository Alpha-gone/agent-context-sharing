package authz

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// AuthorizationCodeStore는 코드의 저장과 최초 교환의 트랜잭션 경계다.
type AuthorizationCodeStore interface {
	CreateOAuthAuthorizationCode(context.Context, store.OAuthAuthorizationCode) error
	ExchangeOAuthAuthorizationCode(context.Context, store.OAuthCodeExchange, func() time.Time, func(store.OAuthAuthorizationCode, time.Time) (store.OAuthInitialGrant, error)) error
}

// AuthorizationRequest는 로그인·명시적 동의 뒤 코드에 결합할 요청이다.
type AuthorizationRequest struct {
	ClientID, RedirectURI, Resource, ResponseType, Scope, Challenge, ChallengeMethod string
}

// OAuthError는 자격·저장 원문을 담지 않는 표준 OAuth 오류다.
type OAuthError struct{ Code string }

func (err *OAuthError) Error() string { return err.Code }

var verifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// ValidateAuthorizationRequest는 등록 대조 후 code·단일 scope·S256만 허용한다.
func (s *Service) ValidateAuthorizationRequest(input AuthorizationRequest) error {
	if err := s.ValidateAuthorizationTarget(input.ClientID, input.RedirectURI, input.Resource); err != nil {
		return err
	}
	if input.ResponseType != "code" {
		return &OAuthError{Code: "unsupported_response_type"}
	}
	if input.Scope != "agent-context" {
		return &OAuthError{Code: "invalid_scope"}
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(input.Challenge)
	if input.ChallengeMethod != "S256" || err != nil || len(decoded) != sha256.Size {
		return &OAuthError{Code: "invalid_request"}
	}
	return nil
}

// IssueAuthorizationCode는 검증된 웹 계정·명시적 동의가 전달된 뒤에만 호출한다.
func (s *Service) IssueAuthorizationCode(ctx context.Context, account model.ID, input AuthorizationRequest) (code string, resultErr error) {
	defer func() {
		id := model.ID{}
		if resultErr == nil {
			id = account
		}
		s.authObservation(ctx, "authorization", id, input.ClientID, model.ID{}, resultErr)
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.ValidateAuthorizationRequest(input); err != nil {
		return "", err
	}
	if !account.IsV7() {
		return "", ErrInvalidCredential
	}
	if s.config.AuthorizationCodes == nil {
		return "", ErrUnavailable
	}
	raw, err := secret()
	if err != nil {
		return "", &credentialFailure{ErrUnavailable, err}
	}
	now := s.now().UTC().Truncate(time.Second)
	err = s.config.AuthorizationCodes.CreateOAuthAuthorizationCode(ctx, store.OAuthAuthorizationCode{
		Hash: sha256.Sum256([]byte(raw)), AccountID: account, ClientID: input.ClientID, RedirectURI: input.RedirectURI, Resource: input.Resource, Scope: input.Scope, Challenge: input.Challenge, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err != nil {
		return "", &credentialFailure{ErrUnavailable, err}
	}
	return raw, nil
}

// OAuthTokens는 token endpoint만 반환하는 자격이며 진단·도구 결과에 전달하지 않는다.
type OAuthTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	RefreshToken string `json:"refresh_token"`
}

// ExchangeAuthorizationCode는 키를 미리 읽어 잠금 중 추가 연결 확보를 피한다.
func (s *Service) ExchangeAuthorizationCode(ctx context.Context, raw, client, callback, resource, verifier string) (tokens OAuthTokens, resultErr error) {
	var verified store.OAuthGrantBinding
	defer func() {
		s.authObservation(ctx, "code_exchange", verified.AccountID, verified.ClientID, verified.ID, resultErr)
	}()
	if err := ctx.Err(); err != nil {
		return OAuthTokens{}, err
	}
	if raw == "" || !verifierPattern.MatchString(verifier) {
		return OAuthTokens{}, &OAuthError{Code: "invalid_grant"}
	}
	if err := s.ValidateAuthorizationTarget(client, callback, resource); err != nil {
		return OAuthTokens{}, &OAuthError{Code: "invalid_grant"}
	}
	if s.config.AuthorizationCodes == nil {
		return OAuthTokens{}, ErrUnavailable
	}
	signer, err := s.oauthSigner(ctx)
	if err != nil {
		return OAuthTokens{}, &credentialFailure{ErrUnavailable, err}
	}
	var result OAuthTokens
	err = s.config.AuthorizationCodes.ExchangeOAuthAuthorizationCode(ctx, store.OAuthCodeExchange{Hash: sha256.Sum256([]byte(raw)), ClientID: client, RedirectURI: callback, Resource: resource, Verifier: verifier}, s.now,
		func(code store.OAuthAuthorizationCode, now time.Time) (store.OAuthInitialGrant, error) {
			grantID, err := model.NewID()
			if err != nil {
				return store.OAuthInitialGrant{}, err
			}
			verified = store.OAuthGrantBinding{ID: grantID, AccountID: code.AccountID, ClientID: code.ClientID}
			var hash [32]byte
			result, hash, err = s.issueOAuthTokens(signer, store.OAuthGrantBinding{ID: grantID, AccountID: code.AccountID, ClientID: code.ClientID, Resource: code.Resource, Scope: code.Scope}, now)
			if err != nil {
				return store.OAuthInitialGrant{}, err
			}
			return store.OAuthInitialGrant{ID: grantID, RefreshHash: hash}, nil
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

// AuthorizationIssuer는 요청 Host가 아닌 고정 issuer다.
func (s *Service) AuthorizationIssuer() string { return s.config.Issuer }
