package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthorizationCodeSingleUseRevokesIssuedToken(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "tester", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	verifier := "high-entropy-pkce-verifier"
	code, err := service.Authorize(t.Context(), accountID, AuthorizeRequest{ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digest(verifier), CodeChallengeMethod: "S256", Resource: "https://service.test/mcp"})
	if err != nil {
		t.Fatalf("인가 코드 발급: %v", err)
	}
	token, err := service.Exchange(t.Context(), code, "test-client", "http://127.0.0.1/callback", verifier)
	if err != nil {
		t.Fatalf("첫 코드 교환: %v", err)
	}
	if _, err := service.Exchange(t.Context(), code, "test-client", "http://127.0.0.1/callback", verifier); err == nil {
		t.Fatal("재사용 코드가 허용됐다")
	}
	revoked, err := backend.IsTokenRevoked(t.Context(), token.ID, time.Now())
	if err != nil || !revoked {
		t.Fatalf("재사용 코드 토큰 폐기 = %t, %v", revoked, err)
	}
}

func TestExchangeRejectsAuthorizationCodeForOtherResource(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	verifier := "resource-change-verifier"
	code := "resource-change-code"
	now := time.Now().UTC()
	if err := backend.CreateAuthorizationCode(t.Context(), store.AuthorizationCode{Hash: digest(code), ClientID: "test-client", AccountID: accountID, RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digest(verifier), Resource: "https://other.test/mcp", IssuedAt: now, ExpiresAt: now.Add(authorizationCodeLifetime)}); err != nil {
		t.Fatalf("인가 코드 저장: %v", err)
	}
	if _, err := service.Exchange(t.Context(), code, "test-client", "http://127.0.0.1/callback", verifier); err == nil {
		t.Fatal("다른 resource의 인가 코드가 교환됐다")
	}
}

func TestExchangeRevokesTokenWhenAuthorizationCodeRecordFails(t *testing.T) {
	backend := newMemoryStore()
	backend.setAuthorizationCodeTokenErr = errors.New("기록 실패")
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "recordfail", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	verifier := "record-failure-verifier"
	code, err := service.Authorize(t.Context(), accountID, AuthorizeRequest{ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digest(verifier), CodeChallengeMethod: "S256", Resource: service.config.Resource})
	if err != nil {
		t.Fatalf("인가 코드 발급: %v", err)
	}
	if _, err := service.Exchange(t.Context(), code, "test-client", "http://127.0.0.1/callback", verifier); err == nil {
		t.Fatal("토큰 기록 실패가 성공으로 처리됐다")
	}
	if len(backend.revoked) != 1 {
		t.Fatalf("기록 실패 뒤 폐기 토큰 수 = %d, want 1", len(backend.revoked))
	}
}

func TestAuthenticateUsesPasswordMismatchForMissingLoginID(t *testing.T) {
	service := testService(t, newMemoryStore())
	if _, err := service.Authenticate(t.Context(), "missing", "wrong password"); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatalf("없는 로그인 아이디 인증 = %v, want bcrypt 불일치", err)
	}
}

func TestAuthorizationCodeConcurrentExchangeAllowsOne(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "concurrent", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	verifier := "concurrent-pkce-verifier"
	code, err := service.Authorize(t.Context(), accountID, AuthorizeRequest{ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digest(verifier), CodeChallengeMethod: "S256", Resource: "https://service.test/mcp"})
	if err != nil {
		t.Fatalf("인가 코드 발급: %v", err)
	}
	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Go(func() {
			_, err := service.Exchange(t.Context(), code, "test-client", "http://127.0.0.1/callback", verifier)
			results <- err
		})
	}
	waitGroup.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("동시 인가 코드 교환 성공 수 = %d, want 1", successes)
	}
}

func TestTokenAudienceSeparation(t *testing.T) {
	service := testService(t, newMemoryStore())
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.issue(t.Context(), id, "web", time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("웹 세션 발급: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); err != nil {
		t.Fatalf("웹 세션 검증: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "https://service.test/mcp"); err == nil {
		t.Fatal("웹 세션의 MCP 교차 사용이 허용됐다")
	}
}

func TestVerifyRejectsTokenWithUnknownKeyID(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	key, err := service.activeKey(t.Context())
	if err != nil {
		t.Fatalf("서명 키 준비: %v", err)
	}
	var private jose.JSONWebKey
	if err := json.Unmarshal([]byte(key.PrivateKey), &private); err != nil {
		t.Fatalf("개인 키 해석: %v", err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "unknown-key"))
	if err != nil {
		t.Fatalf("JWT 서명기 생성: %v", err)
	}
	now := time.Now().UTC()
	raw, err := jwt.Signed(signer).Claims(tokenClaims{Claims: jwt.Claims{Issuer: service.config.Issuer, Subject: accountID.String(), Audience: jwt.Audience{"web"}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)), ID: "unknown-key-token"}, AuthenticatedAt: now.Unix()}).Serialize()
	if err != nil {
		t.Fatalf("알 수 없는 kid 토큰 발급: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), raw, "web"); err == nil {
		t.Fatal("알 수 없는 kid 토큰이 다른 공개 키로 검증됐다")
	}
}

func TestVerifyRequiresExpirationClaim(t *testing.T) {
	service := testService(t, newMemoryStore())
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	key, err := service.activeKey(t.Context())
	if err != nil {
		t.Fatalf("서명 키 준비: %v", err)
	}
	var private jose.JSONWebKey
	if err := json.Unmarshal([]byte(key.PrivateKey), &private); err != nil {
		t.Fatalf("개인 키 해석: %v", err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", key.ID))
	if err != nil {
		t.Fatalf("JWT 서명기 생성: %v", err)
	}
	now := time.Now().UTC()
	raw, err := jwt.Signed(signer).Claims(tokenClaims{Claims: jwt.Claims{Issuer: service.config.Issuer, Subject: accountID.String(), Audience: jwt.Audience{service.config.Resource}, IssuedAt: jwt.NewNumericDate(now), ID: "without-exp"}, AuthenticatedAt: now.Unix()}).Serialize()
	if err != nil {
		t.Fatalf("만료 없는 JWT 발급: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), raw, service.config.Resource); err == nil {
		t.Fatal("exp 없는 토큰이 검증됐다")
	}
}

func TestActiveKeyUsesExistingKeyAfterConcurrentCreation(t *testing.T) {
	backend := newMemoryStore()
	existing, err := newSigningKey()
	if err != nil {
		t.Fatalf("기존 서명 키 생성: %v", err)
	}
	backend.activeKeyConflict = &existing
	service := testService(t, backend)
	key, err := service.activeKey(t.Context())
	if err != nil {
		t.Fatalf("경쟁 뒤 활성 서명 키 조회: %v", err)
	}
	if key.ID != existing.ID {
		t.Fatalf("경쟁 뒤 활성 서명 키 = %q, want %q", key.ID, existing.ID)
	}
}

func TestRenewRequiresVerifiedUnexpiredToken(t *testing.T) {
	service := testService(t, newMemoryStore())
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	otherAccountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expiredToken := signedRenewalToken(t, service, accountID, now.Add(-time.Second), now.Add(-time.Hour), "")
	expiredToken.ExpiresAt = now.Add(5 * time.Second)
	tests := []struct {
		name        string
		accountID   model.ID
		token       Token
		wantRenewed bool
	}{
		{name: "valid near expiry", accountID: accountID, token: signedRenewalToken(t, service, accountID, now.Add(5*time.Second), now.Add(-time.Hour), ""), wantRenewed: true},
		{name: "expired signed claim", accountID: accountID, token: expiredToken},
		{name: "unknown kid", accountID: accountID, token: signedRenewalToken(t, service, accountID, now.Add(5*time.Second), now.Add(-time.Hour), "unknown-key")},
		{name: "other account", accountID: otherAccountID, token: signedRenewalToken(t, service, accountID, now.Add(5*time.Second), now.Add(-time.Hour), "")},
		{name: "future authentication", accountID: accountID, token: signedRenewalToken(t, service, accountID, now.Add(5*time.Second), now.Add(time.Hour), "")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, renewed, err := service.Renew(t.Context(), test.accountID, test.token)
			if err != nil {
				t.Fatalf("토큰 갱신: %v", err)
			}
			if renewed != test.wantRenewed {
				t.Fatalf("갱신 여부 = %t, want %t", renewed, test.wantRenewed)
			}
		})
	}
}

func signedRenewalToken(t *testing.T, service *Service, accountID model.ID, expiresAt, authenticatedAt time.Time, keyID string) Token {
	t.Helper()
	key, err := service.activeKey(t.Context())
	if err != nil {
		t.Fatalf("서명 키 준비: %v", err)
	}
	var private jose.JSONWebKey
	if err := json.Unmarshal([]byte(key.PrivateKey), &private); err != nil {
		t.Fatalf("개인 키 해석: %v", err)
	}
	if keyID == "" {
		keyID = key.ID
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID))
	if err != nil {
		t.Fatalf("JWT 서명기 생성: %v", err)
	}
	issuedAt := time.Now().UTC()
	if !expiresAt.After(issuedAt) {
		issuedAt = expiresAt.Add(-time.Hour)
	}
	raw, err := jwt.Signed(signer).Claims(tokenClaims{Claims: jwt.Claims{Issuer: service.config.Issuer, Subject: accountID.String(), Audience: jwt.Audience{service.config.Resource}, IssuedAt: jwt.NewNumericDate(issuedAt), Expiry: jwt.NewNumericDate(expiresAt), ID: "renewal-token"}, AuthenticatedAt: authenticatedAt.Unix()}).Serialize()
	if err != nil {
		t.Fatalf("갱신 검사 토큰 발급: %v", err)
	}
	return Token{Raw: raw, ID: "renewal-token", ExpiresAt: expiresAt}
}

func TestValidateAuthorizeRequestAllowsLoopbackDynamicPort(t *testing.T) {
	service := testService(t, newMemoryStore())
	err := service.ValidateAuthorizeRequest(AuthorizeRequest{ClientID: "test-client", RedirectURI: "http://127.0.0.1:49152/callback", CodeChallenge: "challenge", CodeChallengeMethod: "S256", Resource: "https://service.test/mcp"})
	if err != nil {
		t.Fatalf("루프백 동적 포트가 거부됐다: %v", err)
	}
}

func TestRegisterValidatesPasswordLengthAndAuthenticatesLongUnicodePassword(t *testing.T) {
	tests := []struct {
		name     string
		loginID  string
		password string
		valid    bool
	}{
		{name: "seven characters", loginID: "shortpwd", password: strings.Repeat("a", 7)},
		{name: "minimum length", loginID: "minpwd", password: strings.Repeat("a", 8), valid: true},
		{name: "maximum Unicode length", loginID: "unicodepwd", password: strings.Repeat("한", 128), valid: true},
		{name: "over maximum length", loginID: "longpwd", password: strings.Repeat("a", 129)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := testService(t, newMemoryStore())
			id, err := service.Register(t.Context(), test.loginID, test.password)
			if !test.valid {
				if err == nil {
					t.Fatal("허용되지 않는 비밀번호가 등록됐다")
				}
				return
			}
			if err != nil {
				t.Fatalf("계정 등록: %v", err)
			}
			authenticatedID, err := service.Authenticate(t.Context(), test.loginID, test.password)
			if err != nil || authenticatedID != id {
				t.Fatalf("등록한 비밀번호 인증 = %s, %v; want %s, nil", authenticatedID, err, id)
			}
		})
	}
}

func TestAuthenticateRehashesLegacyDirectBcryptHash(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	password := "legacy password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("기존 bcrypt 해시 생성: %v", err)
	}
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("계정 식별자 생성: %v", err)
	}
	if err := backend.CreateAccount(t.Context(), store.Account{ID: id, LoginID: "legacy", PasswordHash: string(hash), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("기존 계정 저장: %v", err)
	}
	authenticatedID, err := service.Authenticate(t.Context(), "legacy", password)
	if err != nil || authenticatedID != id {
		t.Fatalf("기존 bcrypt 해시 인증 = %s, %v; want %s, nil", authenticatedID, err, id)
	}
	account, err := backend.AccountByLoginID(t.Context(), "legacy")
	if err != nil {
		t.Fatalf("재해시한 계정 조회: %v", err)
	}
	currentHash, rehashed := strings.CutPrefix(account.PasswordHash, passwordHashPrefix)
	if !rehashed {
		t.Fatalf("기존 해시가 현재 형식으로 바뀌지 않았다: %q", account.PasswordHash)
	}
	material := passwordMaterial(password)
	if err := bcrypt.CompareHashAndPassword([]byte(currentHash), material[:]); err != nil {
		t.Fatalf("재해시한 비밀번호 대조: %v", err)
	}
}

func testService(t *testing.T, backend *memoryStore) *Service {
	t.Helper()
	redirect, err := url.Parse("http://127.0.0.1/callback")
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(backend, Config{Issuer: "https://service.test", Resource: "https://service.test/mcp", Clients: []string{"test-client"}, RedirectURIs: []*url.URL{redirect}, BcryptCost: 4})
	if err != nil {
		t.Fatalf("인가 서비스 생성: %v", err)
	}
	return service
}

type memoryStore struct {
	mu                           sync.Mutex
	accounts                     map[string]store.Account
	codes                        map[string]store.AuthorizationCode
	revoked                      map[string]time.Time
	keys                         []store.SigningKey
	setAuthorizationCodeTokenErr error
	activeKeyConflict            *store.SigningKey
}

func newMemoryStore() *memoryStore {
	return &memoryStore{accounts: map[string]store.Account{}, codes: map[string]store.AuthorizationCode{}, revoked: map[string]time.Time{}}
}
func (s *memoryStore) CreateAccount(_ context.Context, account store.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[account.LoginID]; ok {
		return errors.New("duplicate")
	}
	s.accounts[account.LoginID] = account
	return nil
}
func (s *memoryStore) AccountByLoginID(_ context.Context, loginID string) (store.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[loginID]
	if !ok {
		return store.Account{}, store.ErrNotFound
	}
	return account, nil
}
func (s *memoryStore) UpdatePasswordHashIfMatches(_ context.Context, accountID model.ID, oldHash, newHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for loginID, account := range s.accounts {
		if account.ID != accountID || account.PasswordHash != oldHash {
			continue
		}
		account.PasswordHash = newHash
		s.accounts[loginID] = account
		return true, nil
	}
	return false, nil
}
func (s *memoryStore) CreateAuthorizationCode(_ context.Context, code store.AuthorizationCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code.Hash] = code
	return nil
}
func (s *memoryStore) ConsumeAuthorizationCode(_ context.Context, hash string, now time.Time) (store.AuthorizationCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	code, ok := s.codes[hash]
	if !ok {
		return store.AuthorizationCode{}, store.ErrNotFound
	}
	if code.ConsumedAt != nil {
		used := store.CodeUsedError{TokenID: code.IssuedTokenID, IssuedAt: code.IssuedAt}
		if code.IssuedTokenExpiresAt != nil {
			used.TokenExpiresAt = code.IssuedTokenExpiresAt.UTC()
		}
		return store.AuthorizationCode{}, used
	}
	if !code.ExpiresAt.After(now) {
		return store.AuthorizationCode{}, store.ErrNotFound
	}
	code.ConsumedAt = &now
	s.codes[hash] = code
	return code, nil
}
func (s *memoryStore) SetAuthorizationCodeToken(_ context.Context, hash, tokenID string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setAuthorizationCodeTokenErr != nil {
		return s.setAuthorizationCodeTokenErr
	}
	code := s.codes[hash]
	code.IssuedTokenID = tokenID
	code.IssuedTokenExpiresAt = &expiresAt
	s.codes[hash] = code
	return nil
}
func (s *memoryStore) RevokeToken(_ context.Context, tokenID string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked[tokenID] = expiresAt
	return nil
}
func (s *memoryStore) IsTokenRevoked(_ context.Context, tokenID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt, ok := s.revoked[tokenID]
	return ok && expiresAt.After(now), nil
}
func (s *memoryStore) ActiveSigningKey(_ context.Context) (store.SigningKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.keys {
		if key.State == "active" {
			return key, nil
		}
	}
	return store.SigningKey{}, store.ErrNotFound
}
func (s *memoryStore) SigningKey(_ context.Context, keyID string) (store.SigningKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.keys {
		if key.ID == keyID {
			return key, nil
		}
	}
	return store.SigningKey{}, store.ErrNotFound
}
func (s *memoryStore) SigningKeys(_ context.Context) ([]store.SigningKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.SigningKey(nil), s.keys...), nil
}
func (s *memoryStore) CreateSigningKey(_ context.Context, key store.SigningKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeKeyConflict != nil && key.State == "active" {
		s.keys = append([]store.SigningKey{*s.activeKeyConflict}, s.keys...)
		s.activeKeyConflict = nil
		return store.ErrActiveSigningKeyExists
	}
	s.keys = append([]store.SigningKey{key}, s.keys...)
	return nil
}
func (s *memoryStore) RotateSigningKey(_ context.Context, key store.SigningKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.keys {
		s.keys[index].State = "retired"
	}
	s.keys = append([]store.SigningKey{key}, s.keys...)
	return nil
}
