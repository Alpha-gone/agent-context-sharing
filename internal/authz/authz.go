// Package authz는 계정 자격 증명, OAuth 인가 코드, JWT와 폐기 목록을 관리한다.
package authz

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/crypto/bcrypt"
)

const (
	accessTokenLifetime       = time.Hour
	webSessionLifetime        = 12 * time.Hour
	authorizationCodeLifetime = time.Minute
	minPasswordRunes          = 8
	maxPasswordRunes          = 128
	passwordHashPrefix        = "bcrypt-sha256-v1:"
)

var loginIDPattern = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)

// Config는 인가 서버의 고정된 배포 경계를 모은다.
type Config struct {
	Issuer       string
	Resource     string
	Clients      []string
	RedirectURIs []*url.URL
	BcryptCost   int
}

// Service는 데이터 접근 계층 위에서 인증·인가 계약을 수행한다.
type Service struct {
	store  authStore
	config Config
}

// authStore는 인가 서버가 데이터베이스 접근 계층에 요구하는 최소 계약이다.
type authStore interface {
	CreateAccount(context.Context, store.Account) error
	AccountByLoginID(context.Context, string) (store.Account, error)
	UpdatePasswordHashIfMatches(context.Context, model.ID, string, string) (bool, error)
	CreateAuthorizationCode(context.Context, store.AuthorizationCode) error
	ConsumeAuthorizationCode(context.Context, string, time.Time) (store.AuthorizationCode, error)
	SetAuthorizationCodeTokenID(context.Context, string, string) error
	RevokeToken(context.Context, string, time.Time) error
	IsTokenRevoked(context.Context, string, time.Time) (bool, error)
	ActiveSigningKey(context.Context) (store.SigningKey, error)
	SigningKeys(context.Context) ([]store.SigningKey, error)
	CreateSigningKey(context.Context, store.SigningKey) error
	RotateSigningKey(context.Context, store.SigningKey) error
}

// Token은 응답 헤더 또는 세션 쿠키로 보낼 서명된 자격 증명이다.
type Token struct {
	Raw       string
	ID        string
	ExpiresAt time.Time
}

// AuthorizeRequest는 로그인 뒤 인가 코드를 만들 때 이미 검증된 OAuth 입력이다.
type AuthorizeRequest struct{ ClientID, RedirectURI, CodeChallenge, CodeChallengeMethod, Resource string }

// New는 필요한 구성과 저장소 접근 계층을 확인해 인가 서비스를 만든다.
func New(source authStore, config Config) (*Service, error) {
	if source == nil || config.Issuer == "" || config.Resource == "" || config.BcryptCost < bcrypt.MinCost || config.BcryptCost > bcrypt.MaxCost {
		return nil, fmt.Errorf("인가 서비스 구성이 올바르지 않다")
	}
	if len(config.Clients) == 0 || len(config.RedirectURIs) == 0 {
		return nil, fmt.Errorf("등록 OAuth 클라이언트와 redirect_uri가 필요하다")
	}
	return &Service{store: source, config: config}, nil
}

// Register는 형식이 맞는 로그인 아이디와 bcrypt-SHA-256 해시를 가진 새 계정을 만든다.
func (s *Service) Register(ctx context.Context, loginID, password string) (model.ID, error) {
	if !loginIDPattern.MatchString(loginID) || !validPassword(password) {
		return model.ID{}, fmt.Errorf("로그인 아이디 또는 비밀번호가 올바르지 않다")
	}
	hash, err := hashPassword(password, s.config.BcryptCost)
	if err != nil {
		return model.ID{}, fmt.Errorf("비밀번호 해시 생성: %w", err)
	}
	id, err := model.NewID()
	if err != nil {
		return model.ID{}, fmt.Errorf("계정 식별자 생성: %w", err)
	}
	err = s.store.CreateAccount(ctx, store.Account{ID: id, LoginID: loginID, PasswordHash: hash, CreatedAt: time.Now().UTC()})
	if err != nil {
		return model.ID{}, err
	}
	return id, nil
}

// Authenticate는 저장된 bcrypt 해시와 제시한 비밀번호를 대조하고, 기존 직접 bcrypt
// 해시는 성공한 로그인에서 현재 형식으로 바꾼다.
func (s *Service) Authenticate(ctx context.Context, loginID, password string) (model.ID, error) {
	account, err := s.store.AccountByLoginID(ctx, loginID)
	if err != nil {
		return model.ID{}, fmt.Errorf("자격 증명 검증: %w", err)
	}
	legacy, err := comparePassword(account.PasswordHash, password)
	if err != nil {
		return model.ID{}, fmt.Errorf("자격 증명 검증: %w", err)
	}
	if legacy {
		hash, err := hashPassword(password, s.config.BcryptCost)
		if err != nil {
			return model.ID{}, fmt.Errorf("비밀번호 해시 생성: %w", err)
		}
		if _, err := s.store.UpdatePasswordHashIfMatches(ctx, account.ID, account.PasswordHash, hash); err != nil {
			return model.ID{}, fmt.Errorf("비밀번호 해시 갱신: %w", err)
		}
	}
	return account.ID, nil
}

func validPassword(password string) bool {
	length := utf8.RuneCountInString(password)
	return minPasswordRunes <= length && length <= maxPasswordRunes
}

func hashPassword(password string, cost int) (string, error) {
	material := passwordMaterial(password)
	hash, err := bcrypt.GenerateFromPassword(material[:], cost)
	if err != nil {
		return "", err
	}
	return passwordHashPrefix + string(hash), nil
}

func comparePassword(stored, password string) (bool, error) {
	if hash, prehashed := strings.CutPrefix(stored, passwordHashPrefix); prehashed {
		material := passwordMaterial(password)
		return false, bcrypt.CompareHashAndPassword([]byte(hash), material[:])
	}
	return true, bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
}

func passwordMaterial(password string) [sha256.Size]byte {
	return sha256.Sum256([]byte(password))
}

// ValidateAuthorizeRequest는 사전 등록 클라이언트, 완전 일치 redirect_uri, PKCE와 리소스를 확인한다.
func (s *Service) ValidateAuthorizeRequest(request AuthorizeRequest) error {
	if !slices.Contains(s.config.Clients, request.ClientID) {
		return fmt.Errorf("등록되지 않은 client_id")
	}
	redirect, err := url.Parse(request.RedirectURI)
	if err != nil {
		return fmt.Errorf("redirect_uri 해석: %w", err)
	}
	if !slices.ContainsFunc(s.config.RedirectURIs, func(allowed *url.URL) bool { return redirectMatches(allowed, redirect) }) {
		return fmt.Errorf("허용되지 않은 redirect_uri")
	}
	if request.CodeChallenge == "" || request.CodeChallengeMethod != "S256" {
		return fmt.Errorf("PKCE S256 code_challenge이 필요하다")
	}
	if request.Resource != s.config.Resource {
		return fmt.Errorf("resource가 일치하지 않는다")
	}
	return nil
}

// Authorize는 원문을 저장하지 않는 60초짜리 인가 코드를 발급한다.
func (s *Service) Authorize(ctx context.Context, accountID model.ID, request AuthorizeRequest) (string, error) {
	if err := s.ValidateAuthorizeRequest(request); err != nil {
		return "", err
	}
	code, err := secret()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	err = s.store.CreateAuthorizationCode(ctx, store.AuthorizationCode{Hash: digest(code), ClientID: request.ClientID, AccountID: accountID, RedirectURI: request.RedirectURI, CodeChallenge: request.CodeChallenge, Resource: request.Resource, IssuedAt: now, ExpiresAt: now.Add(authorizationCodeLifetime)})
	if err != nil {
		return "", err
	}
	return code, nil
}

// Exchange는 코드를 조건부로 한 번 소비하고 PKCE를 검증한 뒤 MCP 접근 토큰을 발급한다.
func (s *Service) Exchange(ctx context.Context, code, clientID, redirectURI, verifier string) (Token, error) {
	now := time.Now().UTC()
	stored, err := s.store.ConsumeAuthorizationCode(ctx, digest(code), now)
	if err != nil {
		var used store.CodeUsedError
		if errors.As(err, &used) && used.TokenID != "" {
			_ = s.store.RevokeToken(ctx, used.TokenID, used.IssuedAt.Add(accessTokenLifetime))
		}
		return Token{}, fmt.Errorf("invalid_grant: %w", err)
	}
	if stored.ClientID != clientID || stored.RedirectURI != redirectURI || digestPKCE(verifier) != stored.CodeChallenge {
		return Token{}, fmt.Errorf("invalid_grant")
	}
	token, err := s.issue(ctx, stored.AccountID, s.config.Resource, now, now)
	if err != nil {
		return Token{}, err
	}
	if err := s.store.SetAuthorizationCodeTokenID(ctx, stored.Hash, token.ID); err != nil {
		return Token{}, err
	}
	return token, nil
}

// WebSession은 웹 채널 전용 audience와 12시간 수명을 가진 서명 쿠키 값을 발급한다.
func (s *Service) WebSession(ctx context.Context, accountID model.ID, audience string) (Token, error) {
	return s.issue(ctx, accountID, audience, time.Now().UTC(), time.Now().UTC())
}

// Verify는 기대 audience, 서명, 필수 클레임, 만료와 폐기 목록을 한 경로에서 확인한다.
func (s *Service) Verify(ctx context.Context, raw, audience string) (model.ID, Token, error) {
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return model.ID{}, Token{}, fmt.Errorf("unauthenticated")
	}
	keys, err := s.store.SigningKeys(ctx)
	if err != nil {
		return model.ID{}, Token{}, fmt.Errorf("unauthenticated")
	}
	var claims tokenClaims
	verified := false
	for _, item := range keys {
		var public jose.JSONWebKey
		if json.Unmarshal([]byte(item.PublicKey), &public) != nil {
			continue
		}
		if parsed.Claims(public.Key, &claims) == nil {
			verified = true
			break
		}
	}
	if !verified || claims.ValidateWithLeeway(jwt.Expected{Issuer: s.config.Issuer, AnyAudience: jwt.Audience{audience}, Time: time.Now()}, 0) != nil || claims.Subject == "" || claims.ID == "" || claims.IssuedAt == nil {
		return model.ID{}, Token{}, fmt.Errorf("unauthenticated")
	}
	id, err := model.ParseID(claims.Subject)
	if err != nil {
		return model.ID{}, Token{}, fmt.Errorf("unauthenticated")
	}
	revoked, err := s.store.IsTokenRevoked(ctx, claims.ID, time.Now().UTC())
	if err != nil || revoked {
		return model.ID{}, Token{}, fmt.Errorf("unauthenticated")
	}
	expires := claims.Expiry.Time()
	return id, Token{Raw: raw, ID: claims.ID, ExpiresAt: expires}, nil
}

// Renew은 남은 수명이 10초 이하이면서 최초 인증 뒤 12시간 안일 때만 새 MCP 토큰을 발급한다.
func (s *Service) Renew(ctx context.Context, accountID model.ID, token Token) (Token, bool, error) {
	if time.Until(token.ExpiresAt) > 10*time.Second {
		return Token{}, false, nil
	}
	parsed, err := jwt.ParseSigned(token.Raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return Token{}, false, nil
	}
	var claims tokenClaims
	keys, err := s.store.SigningKeys(ctx)
	if err != nil {
		return Token{}, false, err
	}
	for _, item := range keys {
		var public jose.JSONWebKey
		if json.Unmarshal([]byte(item.PublicKey), &public) == nil && parsed.Claims(public.Key, &claims) == nil {
			break
		}
	}
	if claims.AuthenticatedAt == 0 || time.Since(time.Unix(claims.AuthenticatedAt, 0)) > webSessionLifetime {
		return Token{}, false, nil
	}
	renewed, err := s.issue(ctx, accountID, s.config.Resource, time.Now().UTC(), time.Unix(claims.AuthenticatedAt, 0).UTC())
	return renewed, err == nil, err
}

// Revoke는 로그아웃 또는 인가 코드 재사용 처리에서 jti를 만료 시각까지 막는다.
func (s *Service) Revoke(ctx context.Context, token Token) error {
	return s.store.RevokeToken(ctx, token.ID, token.ExpiresAt)
}

// JWKS는 검증자에게 비공개 키 없이 공개 키 목록을 제공한다.
func (s *Service) JWKS(ctx context.Context) (jose.JSONWebKeySet, error) {
	items, err := s.store.SigningKeys(ctx)
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}
	set := jose.JSONWebKeySet{Keys: make([]jose.JSONWebKey, 0, len(items))}
	for _, item := range items {
		var key jose.JSONWebKey
		if err := json.Unmarshal([]byte(item.PublicKey), &key); err != nil {
			return jose.JSONWebKeySet{}, fmt.Errorf("공개 키 해석: %w", err)
		}
		set.Keys = append(set.Keys, key)
	}
	return set, nil
}

// RotateSigningKey는 새 ES256 키를 활성화하고 기존 키를 검증 전용으로 은퇴시킨다.
func (s *Service) RotateSigningKey(ctx context.Context) error {
	key, err := newSigningKey()
	if err != nil {
		return err
	}
	return s.store.RotateSigningKey(ctx, key)
}

type tokenClaims struct {
	jwt.Claims
	AuthenticatedAt int64 `json:"auth_time"`
}

func (s *Service) issue(ctx context.Context, accountID model.ID, audience string, now, authenticatedAt time.Time) (Token, error) {
	key, err := s.activeKey(ctx)
	if err != nil {
		return Token{}, err
	}
	var private jose.JSONWebKey
	if err := json.Unmarshal([]byte(key.PrivateKey), &private); err != nil {
		return Token{}, fmt.Errorf("개인 키 해석: %w", err)
	}
	options := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", key.ID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, options)
	if err != nil {
		return Token{}, fmt.Errorf("JWT 서명기 생성: %w", err)
	}
	jti, err := secret()
	if err != nil {
		return Token{}, err
	}
	lifetime := accessTokenLifetime
	if audience != s.config.Resource {
		lifetime = webSessionLifetime
	}
	expires := now.Add(lifetime)
	raw, err := jwt.Signed(signer).Claims(tokenClaims{Claims: jwt.Claims{Issuer: s.config.Issuer, Subject: accountID.String(), Audience: jwt.Audience{audience}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(expires), ID: jti}, AuthenticatedAt: authenticatedAt.Unix()}).Serialize()
	if err != nil {
		return Token{}, fmt.Errorf("JWT 발급: %w", err)
	}
	return Token{Raw: raw, ID: jti, ExpiresAt: expires}, nil
}

func (s *Service) activeKey(ctx context.Context) (store.SigningKey, error) {
	key, err := s.store.ActiveSigningKey(ctx)
	if !errors.Is(err, store.ErrNotFound) {
		return key, err
	}
	key, err = newSigningKey()
	if err != nil {
		return store.SigningKey{}, err
	}
	if err := s.store.CreateSigningKey(ctx, key); err != nil {
		return store.SigningKey{}, err
	}
	return key, nil
}

func newSigningKey() (store.SigningKey, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return store.SigningKey{}, fmt.Errorf("서명 키 생성: %w", err)
	}
	id, err := secret()
	if err != nil {
		return store.SigningKey{}, err
	}
	publicData, err := json.Marshal(jose.JSONWebKey{Key: private.Public(), KeyID: id, Algorithm: string(jose.ES256), Use: "sig"})
	if err != nil {
		return store.SigningKey{}, err
	}
	privateData, err := json.Marshal(jose.JSONWebKey{Key: private, KeyID: id, Algorithm: string(jose.ES256), Use: "sig"})
	if err != nil {
		return store.SigningKey{}, err
	}
	return store.SigningKey{ID: id, Algorithm: string(jose.ES256), PublicKey: string(publicData), PrivateKey: string(privateData), State: "active", CreatedAt: time.Now().UTC()}, nil
}

func redirectMatches(allowed, actual *url.URL) bool {
	if allowed == nil || actual == nil || allowed.Scheme != actual.Scheme || allowed.Path != actual.Path || allowed.RawQuery != actual.RawQuery || allowed.Fragment != actual.Fragment || !strings.EqualFold(allowed.Hostname(), actual.Hostname()) {
		return false
	}
	if ip := net.ParseIP(allowed.Hostname()); ip != nil && ip.IsLoopback() {
		return true
	}
	return allowed.Port() == actual.Port()
}

func secret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("난수 생성: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func digestPKCE(verifier string) string { return digest(verifier) }
