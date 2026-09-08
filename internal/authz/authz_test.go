package authz

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func TestAuthorizationCodeSingleUseRevokesIssuedToken(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "tester", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	verifier := "high-entropy-pkce-verifier"
	code, err := service.Authorize(t.Context(), accountID, AuthorizeRequest{ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digestPKCE(verifier), CodeChallengeMethod: "S256", Resource: "https://service.test/mcp"})
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

func TestAuthorizationCodeConcurrentExchangeAllowsOne(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := service.Register(t.Context(), "concurrent", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	verifier := "concurrent-pkce-verifier"
	code, err := service.Authorize(t.Context(), accountID, AuthorizeRequest{ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: digestPKCE(verifier), CodeChallengeMethod: "S256", Resource: "https://service.test/mcp"})
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

func TestValidateAuthorizeRequestAllowsLoopbackDynamicPort(t *testing.T) {
	service := testService(t, newMemoryStore())
	err := service.ValidateAuthorizeRequest(AuthorizeRequest{ClientID: "test-client", RedirectURI: "http://127.0.0.1:49152/callback", CodeChallenge: "challenge", CodeChallengeMethod: "S256", Resource: "https://service.test/mcp"})
	if err != nil {
		t.Fatalf("루프백 동적 포트가 거부됐다: %v", err)
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
	mu       sync.Mutex
	accounts map[string]store.Account
	codes    map[string]store.AuthorizationCode
	revoked  map[string]time.Time
	keys     []store.SigningKey
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
		return store.AuthorizationCode{}, store.CodeUsedError{TokenID: code.IssuedTokenID, IssuedAt: code.IssuedAt}
	}
	if !code.ExpiresAt.After(now) {
		return store.AuthorizationCode{}, store.ErrNotFound
	}
	code.ConsumedAt = &now
	s.codes[hash] = code
	return code, nil
}
func (s *memoryStore) SetAuthorizationCodeTokenID(_ context.Context, hash, tokenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := s.codes[hash]
	code.IssuedTokenID = tokenID
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
	if len(s.keys) == 0 {
		return store.SigningKey{}, store.ErrNotFound
	}
	return s.keys[0], nil
}
func (s *memoryStore) SigningKeys(_ context.Context) ([]store.SigningKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.SigningKey(nil), s.keys...), nil
}
func (s *memoryStore) CreateSigningKey(_ context.Context, key store.SigningKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
