package authz

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

func TestAuthenticateUsesPasswordMismatchForMissingLoginID(t *testing.T) {
	service := testService(t, newMemoryStore())
	if _, err := service.Authenticate(t.Context(), "missing", "wrong password"); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatalf("없는 로그인 아이디 인증 = %v, want bcrypt 불일치", err)
	}
}

func TestTokenAudienceSeparation(t *testing.T) {
	service := testService(t, newMemoryStore())
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.issue(t.Context(), id, "web", time.Now().UTC())
	if err != nil {
		t.Fatalf("웹 세션 발급: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); err != nil {
		t.Fatalf("웹 세션 검증: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "other-channel"); err == nil {
		t.Fatal("웹 세션의 다른 audience 사용이 허용됐다")
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
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: service.config.Issuer, Subject: accountID.String(), Audience: jwt.Audience{"web"}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)), ID: "unknown-key-token"}).Serialize()
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
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: service.config.Issuer, Subject: accountID.String(), Audience: jwt.Audience{"web"}, IssuedAt: jwt.NewNumericDate(now), ID: "without-exp"}).Serialize()
	if err != nil {
		t.Fatalf("만료 없는 JWT 발급: %v", err)
	}
	if _, _, err := service.Verify(t.Context(), raw, "web"); err == nil {
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
		t.Fatal("기존 해시가 현재 형식으로 바뀌지 않았다")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(passwordMaterial(password))); err != nil {
		t.Fatalf("재해시한 비밀번호 대조: %v", err)
	}
}

// TestAuthenticateUpgradesRawDigestHash는 v1 원시 다이제스트 해시가 여전히 인증되고
// 성공한 로그인에서 현재 형식으로 바뀌는지 확인한다.
func TestAuthenticateUpgradesRawDigestHash(t *testing.T) {
	const password = "raw-digest-password"
	digest := sha256.Sum256([]byte(password))
	raw, err := bcrypt.GenerateFromPassword(digest[:], bcrypt.MinCost)
	if err != nil {
		t.Fatalf("v1 해시 생성: %v", err)
	}
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("계정 식별자: %v", err)
	}
	backend := newMemoryStore()
	if err := backend.CreateAccount(t.Context(), store.Account{
		ID: id, LoginID: "rawdigest", PasswordHash: legacyRawDigestPrefix + string(raw), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("계정 생성: %v", err)
	}
	service := testService(t, backend)
	if authenticated, err := service.Authenticate(t.Context(), "rawdigest", password); err != nil || authenticated != id {
		t.Fatalf("v1 해시 인증 = %s, %v; want %s, nil", authenticated, err, id)
	}
	account, err := backend.AccountByLoginID(t.Context(), "rawdigest")
	if err != nil {
		t.Fatalf("재해시한 계정 조회: %v", err)
	}
	if !strings.HasPrefix(account.PasswordHash, passwordHashPrefix) {
		t.Fatal("v1 해시가 현재 형식으로 바뀌지 않았다")
	}
}

func testService(t *testing.T, backend authStore) *Service {
	t.Helper()
	service, err := New(backend, Config{Issuer: "https://service.test", BcryptCost: 4})
	if err != nil {
		t.Fatalf("인증 서비스 생성: %v", err)
	}
	return service
}

type memoryStore struct {
	mu                sync.Mutex
	accounts          map[string]store.Account
	revoked           map[string]time.Time
	keys              []store.SigningKey
	activeKeyConflict *store.SigningKey
}

func newMemoryStore() *memoryStore {
	return &memoryStore{accounts: map[string]store.Account{}, revoked: map[string]time.Time{}}
}
func (s *memoryStore) CreateAccount(_ context.Context, account store.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[account.LoginID]; ok {
		return store.ErrLoginIDExists
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
func (s *memoryStore) RevokedTokenIDs(_ context.Context, now time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.revoked))
	for id, expiresAt := range s.revoked {
		if expiresAt.After(now) {
			ids = append(ids, id)
		}
	}
	return ids, nil
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
