package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func signedSessionClaims(t *testing.T, service *Service, claims jwt.Claims) string {
	t.Helper()
	key, err := service.activeKey(t.Context())
	if err != nil {
		t.Fatal("시험 서명 키 준비 실패")
	}
	private, err := privateSigningKey(key)
	if err != nil {
		t.Fatal("시험 서명 키 해석 실패")
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", key.ID))
	if err != nil {
		t.Fatal("시험 서명기 준비 실패")
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal("시험 JWT 생성 실패")
	}
	return raw
}

func TestWebSessionRequiredClaimsAndBoundaries(t *testing.T) {
	service := testService(t, newMemoryStore())
	now := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return now }
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	valid := func() jwt.Claims {
		return jwt.Claims{Issuer: service.config.Issuer, Subject: id.String(), Audience: jwt.Audience{"web"}, IssuedAt: jwt.NewNumericDate(now.Add(-time.Hour)), Expiry: jwt.NewNumericDate(now.Add(time.Hour)), ID: "session-claims"}
	}
	for _, test := range []struct {
		name   string
		change func(*jwt.Claims)
	}{
		{"missing issuer", func(c *jwt.Claims) { c.Issuer = "" }},
		{"wrong issuer", func(c *jwt.Claims) { c.Issuer = "https://other.test" }},
		{"missing subject", func(c *jwt.Claims) { c.Subject = "" }},
		{"invalid account", func(c *jwt.Claims) { c.Subject = "invalid" }},
		{"missing audience", func(c *jwt.Claims) { c.Audience = nil }},
		{"other channel", func(c *jwt.Claims) { c.Audience = jwt.Audience{"https://service.test/mcp"} }},
		{"mixed audience", func(c *jwt.Claims) { c.Audience = jwt.Audience{"web", "https://service.test/mcp"} }},
		{"missing issued at", func(c *jwt.Claims) { c.IssuedAt = nil }},
		{"future issued at", func(c *jwt.Claims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Second)) }},
		{"missing expiry", func(c *jwt.Claims) { c.Expiry = nil }},
		{"at expiry", func(c *jwt.Claims) { c.Expiry = jwt.NewNumericDate(now) }},
		{"after expiry", func(c *jwt.Claims) { c.Expiry = jwt.NewNumericDate(now.Add(-time.Second)) }},
		{"empty lifetime", func(c *jwt.Claims) { c.Expiry = c.IssuedAt }},
		{"excess lifetime", func(c *jwt.Claims) {
			c.Expiry = jwt.NewNumericDate(c.IssuedAt.Time().Add(webSessionLifetime + time.Second))
		}},
		{"missing token id", func(c *jwt.Claims) { c.ID = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := valid()
			test.change(&claims)
			if _, _, err := service.Verify(t.Context(), signedSessionClaims(t, service, claims), "web"); !errors.Is(err, ErrInvalidCredential) {
				t.Fatal("잘못된 세션을 허용했습니다")
			}
		})
	}
	raw := signedSessionClaims(t, service, valid())
	if _, _, err := service.Verify(t.Context(), raw, ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("빈 기대 audience를 허용했습니다")
	}
	if _, _, err := service.Verify(t.Context(), raw, "https://service.test/mcp"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("웹 세션을 다른 대상으로 허용했습니다")
	}
	if got, _, err := service.Verify(t.Context(), raw, "web"); err != nil || got != id {
		t.Fatal("정상 세션 거부")
	}
}

func TestWebSessionExactLifetimeAndNoRenewal(t *testing.T) {
	service := testService(t, newMemoryStore())
	now := time.Now().UTC().Truncate(time.Second).Add(500 * time.Millisecond)
	service.now = func() time.Time { return now }
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(t.Context(), id, "web")
	if err != nil {
		t.Fatal("세션 발급 실패")
	}
	expires := now.Truncate(time.Second).Add(webSessionLifetime)
	if token.ExpiresAt != expires {
		t.Fatal("반환된 만료 시각이 JWT 정밀도와 다릅니다")
	}
	for _, test := range []struct {
		name  string
		at    time.Time
		valid bool
	}{
		{"issued", now, true},
		{"before expiry", expires.Add(-time.Nanosecond), true},
		{"at expiry", expires, false},
		{"after expiry", expires.Add(time.Nanosecond), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now = test.at
			account, verified, err := service.Verify(t.Context(), token.Raw, "web")
			if test.valid {
				if err != nil || account != id || verified.Raw != token.Raw || verified.ID != token.ID || !verified.ExpiresAt.Equal(token.ExpiresAt) {
					t.Fatal("검증이 세션을 바꾸거나 수명을 연장했습니다")
				}
			} else if !errors.Is(err, ErrInvalidCredential) {
				t.Fatal("만료 경계에서 세션 수명이 연장됐습니다")
			}
		})
	}
}

type sessionFaultStore struct {
	authStore
	fail   string
	cancel context.CancelFunc
	writes int
}

func TestWebSessionCanceledOperationsSkipStore(t *testing.T) {
	service := testService(t, &sessionFaultStore{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if token, err := service.WebSession(ctx, id, "web"); !errors.Is(err, context.Canceled) || token.Raw != "" {
		t.Fatal("취소된 발급이 저장소를 호출하거나 값을 반환했습니다")
	}
	if id, token, err := service.Verify(ctx, "ignored", "web"); !errors.Is(err, context.Canceled) || !id.IsZero() || token.Raw != "" {
		t.Fatal("취소된 검증이 저장소를 호출하거나 값을 반환했습니다")
	}
	if err := service.Revoke(ctx, Token{ID: "ignored", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, context.Canceled) {
		t.Fatal("취소된 폐기가 저장소를 호출했습니다")
	}
}

func (s *sessionFaultStore) ActiveSigningKey(ctx context.Context) (store.SigningKey, error) {
	if s.fail == "issuance" {
		return store.SigningKey{}, errors.New("synthetic-sensitive-marker")
	}
	key, err := s.authStore.ActiveSigningKey(ctx)
	if s.cancel != nil {
		s.cancel()
	}
	return key, err
}

func (s *sessionFaultStore) RevokeToken(ctx context.Context, id string, expires time.Time) error {
	s.writes++
	if s.fail == "revocation" {
		return errors.New("synthetic-sensitive-marker")
	}
	err := s.authStore.RevokeToken(ctx, id, expires)
	if s.cancel != nil {
		s.cancel()
	}
	return err
}

func TestWebSessionIssuanceAndRevocationFailures(t *testing.T) {
	backend := &sessionFaultStore{authStore: newMemoryStore()}
	service := testService(t, backend)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	for _, audience := range []string{"", "https://service.test/mcp"} {
		if _, err := service.WebSession(t.Context(), id, audience); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("다른 채널 세션 발급 허용")
		}
	}
	if _, err := service.WebSession(t.Context(), model.ID{}, "web"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("잘못된 계정으로 발급했습니다")
	}
	for _, mode := range []string{"issuance", "revocation"} {
		t.Run(mode, func(t *testing.T) {
			backend.fail = mode
			var failure error
			if mode == "issuance" {
				_, failure = service.WebSession(t.Context(), id, "web")
			} else {
				failure = service.Revoke(t.Context(), Token{ID: "session-failure", ExpiresAt: time.Now().Add(time.Hour)})
			}
			if !errors.Is(failure, ErrUnavailable) || strings.Contains(failure.Error(), "synthetic-sensitive-marker") {
				t.Fatal("세션 장애의 분류·원문 비노출 실패")
			}
		})
	}
	backend.fail = ""
	token, err := service.WebSession(t.Context(), id, "web")
	if err != nil {
		t.Fatal("정상 발급 실패")
	}
	ctx, cancel := context.WithCancel(t.Context())
	backend.cancel = cancel
	if err := service.Revoke(ctx, token); !errors.Is(err, context.Canceled) {
		t.Fatal("폐기 저장 직후 취소 전파 실패")
	}
	backend.cancel = nil
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("이미 저장된 폐기가 취소로 되돌아갔습니다")
	}
	ctx, cancel = context.WithCancel(t.Context())
	backend.cancel = cancel
	if result, err := service.WebSession(ctx, id, "web"); !errors.Is(err, context.Canceled) || result.Raw != "" {
		t.Fatal("키 조회 중 취소된 발급이 성공했습니다")
	}
}
