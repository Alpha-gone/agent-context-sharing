// Package authz는 계정 자격 증명, 웹 세션 JWT와 폐기 목록을 관리한다.
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
	"regexp"
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
	webSessionLifetime    = 12 * time.Hour
	minPasswordRunes      = 8
	maxPasswordRunes      = 128
	passwordHashPrefix    = "bcrypt-sha256-b64-v2:"
	legacyRawDigestPrefix = "bcrypt-sha256-v1:"
)

var loginIDPattern = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)

var (
	// ErrInvalidCredential은 제시한 자격 증명 자체가 유효하지 않다는 결과다.
	ErrInvalidCredential = errors.New("invalid_credential")
	// ErrUnavailable은 자격 증명의 문제가 아니라 발급·폐기·검증을 끝내지 못했다는 결과다.
	//
	// 두 오류를 나누는 이유는 호출자가 할 일이 다르기 때문이다. 앞은 재인증이 답이지만
	// 서명 키나 폐기 목록을 읽지 못한 것은 재인증해도 같은 자리에서 다시 막힌다.
	// 「토큰 검증」이 이 구분을 계약으로 확정했다.
	ErrUnavailable = errors.New("unavailable")
)

// Config는 인가 서버의 고정된 배포 경계를 모은다.
type Config struct {
	Issuer     string
	BcryptCost int
}

// Service는 데이터 접근 계층 위에서 인증·인가 계약을 수행한다.
type Service struct {
	diagnostics       Diagnostics
	store             authStore
	config            Config
	dummyPasswordHash string
	passwords         *passwordWork
	// cache는 「토큰 검증」이 요청마다 가져오지 않기로 한 서명 키와 폐기 목록을 담는다.
	cache *verificationCache
}

// authStore는 인가 서버가 데이터베이스 접근 계층에 요구하는 최소 계약이다.
type authStore interface {
	CreateAccount(context.Context, store.Account) error
	AccountByLoginID(context.Context, string) (store.Account, error)
	UpdatePasswordHashIfMatches(context.Context, model.ID, string, string) (bool, error)
	RevokeToken(context.Context, string, time.Time) error
	RevokedTokenIDs(context.Context, time.Time) ([]string, error)
	ActiveSigningKey(context.Context) (store.SigningKey, error)
	SigningKey(context.Context, string) (store.SigningKey, error)
	SigningKeys(context.Context) ([]store.SigningKey, error)
	CreateSigningKey(context.Context, store.SigningKey) error
	RotateSigningKey(context.Context, store.SigningKey) error
}

// Token은 웹 세션 쿠키로 보낼 서명된 자격 증명이다.
type Token struct {
	Raw       string
	ID        string
	ExpiresAt time.Time
}

// New는 필요한 구성과 저장소 접근 계층을 확인해 인가 서비스를 만든다.
func New(source authStore, config Config) (*Service, error) {
	if source == nil || config.Issuer == "" || config.BcryptCost < bcrypt.MinCost || config.BcryptCost > bcrypt.MaxCost {
		return nil, fmt.Errorf("인가 서비스 구성이 올바르지 않다")
	}
	dummyPasswordHash, err := hashPassword("", config.BcryptCost)
	if err != nil {
		return nil, fmt.Errorf("더미 비밀번호 해시 생성: %w", err)
	}
	return &Service{store: source, config: config, dummyPasswordHash: dummyPasswordHash, passwords: newPasswordWork(), cache: newVerificationCache()}, nil
}

// Register는 형식이 맞는 로그인 아이디와 bcrypt-SHA-256 해시를 가진 새 계정을 만든다.
func (s *Service) Register(ctx context.Context, loginID, password string) (model.ID, error) {
	if !loginIDPattern.MatchString(loginID) || !validPassword(password) {
		return model.ID{}, fmt.Errorf("로그인 아이디 또는 비밀번호가 올바르지 않다")
	}
	if err := s.passwords.acquire(ctx); err != nil {
		return model.ID{}, err
	}
	defer s.passwords.release()
	hash, err := s.passwords.generate(password, s.config.BcryptCost)
	if err != nil {
		return model.ID{}, fmt.Errorf("비밀번호 해시 생성: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return model.ID{}, err
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
	if err := s.passwords.acquire(ctx); err != nil {
		return model.ID{}, err
	}
	defer s.passwords.release()
	account, err := s.store.AccountByLoginID(ctx, loginID)
	if ctx.Err() != nil {
		return model.ID{}, ctx.Err()
	}
	if errors.Is(err, store.ErrNotFound) {
		_, err := s.passwords.compare(s.dummyPasswordHash, password)
		if ctx.Err() != nil {
			return model.ID{}, ctx.Err()
		}
		if err == nil {
			err = bcrypt.ErrMismatchedHashAndPassword
		}
		return model.ID{}, fmt.Errorf("자격 증명 검증: %w", err)
	}
	if err != nil {
		return model.ID{}, fmt.Errorf("자격 증명 검증: %w", err)
	}
	legacy, err := s.passwords.compare(account.PasswordHash, password)
	if ctx.Err() != nil {
		return model.ID{}, ctx.Err()
	}
	if err != nil {
		return model.ID{}, fmt.Errorf("자격 증명 검증: %w", err)
	}
	if legacy {
		hash, err := s.passwords.generate(password, s.config.BcryptCost)
		if err != nil {
			return model.ID{}, fmt.Errorf("비밀번호 해시 생성: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return model.ID{}, err
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
	hash, err := bcrypt.GenerateFromPassword([]byte(passwordMaterial(password)), cost)
	if err != nil {
		return "", err
	}
	return passwordHashPrefix + string(hash), nil
}

// comparePassword는 저장된 해시 형식을 보고 대조하며, 현재 형식이 아니면 재해시 대상으로
// 알린다. 반환한 bool이 true이면 호출자가 성공한 로그인에서 현재 형식으로 바꾼다.
func comparePassword(stored, password string) (bool, error) {
	if hash, current := strings.CutPrefix(stored, passwordHashPrefix); current {
		return false, bcrypt.CompareHashAndPassword([]byte(hash), []byte(passwordMaterial(password)))
	}
	if hash, raw := strings.CutPrefix(stored, legacyRawDigestPrefix); raw {
		digest := sha256.Sum256([]byte(password))
		return true, bcrypt.CompareHashAndPassword([]byte(hash), digest[:])
	}
	return true, bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
}

// passwordMaterial은 bcrypt의 72바이트 상한을 피하려고 먼저 해시한 값을 base64로 만든다.
//
// 원시 다이제스트를 그대로 넣지 않는 이유는 NUL 바이트 때문이다. Go의 bcrypt는 NUL을
// 특별히 다루지 않지만 bcrypt 규격은 NUL 종료 문자열을 전제하므로, 다이제스트에 NUL이
// 들어간 비밀번호는 다른 구현에서 그 지점까지만 검증된다. base64는 NUL을 만들지 않는다.
func passwordMaterial(password string) string {
	digest := sha256.Sum256([]byte(password))
	return base64.RawStdEncoding.EncodeToString(digest[:])
}

// WebSession은 웹 채널 전용 audience와 12시간 수명을 가진 서명 쿠키 값을 발급한다.
func (s *Service) WebSession(ctx context.Context, accountID model.ID, audience string) (Token, error) {
	now := time.Now().UTC()
	return s.issue(ctx, accountID, audience, now)
}

// Verify는 기대 audience, 서명, 필수 클레임, 만료와 폐기 목록을 한 경로에서 확인한다.
func (s *Service) Verify(ctx context.Context, raw, audience string) (accountID model.ID, token Token, verifyErr error) {
	defer func() {
		if s.diagnostics == nil {
			return
		}
		if verifyErr != nil {
			level, reason := "WARN", "invalid_credential"
			if errors.Is(verifyErr, ErrUnavailable) {
				level, reason = "ERROR", "authentication_unavailable"
			}
			s.diagnostics.RecordDiagnostic(ctx, level, "web_session_validation", "failed", reason)
			return
		}
		s.diagnostics.RecordDiagnostic(ctx, "INFO", "web_session_validation", "completed", "completed")
	}()

	claims, err := s.verifiedClaims(ctx, raw, audience)
	if err != nil {
		return model.ID{}, Token{}, err
	}
	id, err := model.ParseID(claims.Subject)
	if err != nil {
		return model.ID{}, Token{}, ErrInvalidCredential
	}
	return id, Token{Raw: raw, ID: claims.ID, ExpiresAt: claims.Expiry.Time()}, nil
}

// verifiedClaims는 서명, 필수 클레임, 값과 폐기 여부를 한 경로에서 확인한다.
//
// 자격 증명 실패는 어느 항목이 틀렸는지 구분하지 않고 모두 ErrInvalidCredential로 묶는다.
// 만료와 서명 불일치를 나눠 알리면 공격자에게 무엇을 고쳐야 하는지 알려주는 셈이 된다.
// 반면 서명 키나 폐기 목록을 읽지 못한 것은 자격 증명과 무관하므로 ErrUnavailable로
// 따로 올린다. 어느 조회가 실패했는지는 감싼 원인에만 남고 응답에는 담기지 않는다.
func (s *Service) verifiedClaims(ctx context.Context, raw, audience string) (jwt.Claims, error) {
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return jwt.Claims{}, ErrInvalidCredential
	}
	if len(parsed.Headers) != 1 || parsed.Headers[0].KeyID == "" {
		return jwt.Claims{}, ErrInvalidCredential
	}
	public, err := s.cache.publicKey(ctx, s.store, parsed.Headers[0].KeyID)
	if err != nil {
		// 모르는 `kid`는 자격 증명의 문제이고, 그 밖의 조회 실패는 저장소의 문제다.
		if errors.Is(err, store.ErrNotFound) {
			return jwt.Claims{}, ErrInvalidCredential
		}
		return jwt.Claims{}, fmt.Errorf("%w: 서명 키 조회: %w", ErrUnavailable, err)
	}
	var claims jwt.Claims
	if parsed.Claims(public.Key, &claims) != nil || claims.ValidateWithLeeway(jwt.Expected{Issuer: s.config.Issuer, AnyAudience: jwt.Audience{audience}, Time: time.Now()}, 0) != nil || claims.Subject == "" || claims.ID == "" || claims.IssuedAt == nil || claims.Expiry == nil {
		return jwt.Claims{}, ErrInvalidCredential
	}
	revoked, err := s.cache.isRevoked(ctx, s.store, claims.ID, time.Now().UTC())
	if err != nil {
		return jwt.Claims{}, fmt.Errorf("%w: 폐기 목록 조회: %w", ErrUnavailable, err)
	}
	if revoked {
		return jwt.Claims{}, ErrInvalidCredential
	}
	return claims, nil
}

// Revoke는 로그아웃에서 jti를 만료 시각까지 막는다.
func (s *Service) Revoke(ctx context.Context, token Token) error {
	if err := s.store.RevokeToken(ctx, token.ID, token.ExpiresAt); err != nil {
		return err
	}
	// 이 인스턴스가 방금 폐기한 토큰은 다음 적재를 기다리지 않고 곧바로 막는다.
	s.cache.invalidateRevocations(token.ID)
	return nil
}

// JWKS는 검증자에게 비공개 키 없이 공개 키 목록을 제공한다.
func (s *Service) JWKS(ctx context.Context) (jose.JSONWebKeySet, error) {
	// 최초 로그인 전의 발견도 발급과 같은 원자적 키 초기화 경로를 사용한다.
	if _, err := s.activeKey(ctx); err != nil {
		return jose.JSONWebKeySet{}, err
	}
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
	if err := s.store.RotateSigningKey(ctx, key); err != nil {
		return err
	}
	// 이 인스턴스의 새 키는 부재 판정의 유효기간을 기다리지 않고 다시 읽는다.
	s.cache.invalidateKeys()
	return nil
}

func (s *Service) issue(ctx context.Context, accountID model.ID, audience string, now time.Time) (Token, error) {
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
	expires := now.Add(webSessionLifetime)
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: s.config.Issuer, Subject: accountID.String(), Audience: jwt.Audience{audience}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(expires), ID: jti}).Serialize()
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
		if !errors.Is(err, store.ErrActiveSigningKeyExists) {
			return store.SigningKey{}, err
		}
		return s.store.ActiveSigningKey(ctx)
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

func secret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("난수 생성: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
