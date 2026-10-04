package authz

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// TestDPoPRequestBindsTokenAndRejectsReplay는 같은 키의 새 proof만 해당 토큰과 요청에
// 결합될 수 있고, 한번 예약된 proof는 재생되지 않는지 확인한다.
func TestDPoPRequestBindsTokenAndRejectsReplay(t *testing.T) {
	service := testService(t, newMemoryStore())
	accountID, err := service.Register(t.Context(), "dpoptester", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	verifier := testVerifier("dpop-binding")
	code, err := service.Authorize(t.Context(), accountID, time.Now().UTC(), AuthorizeRequest{
		ResponseType: "code", ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback",
		CodeChallenge: digest(verifier), CodeChallengeMethod: "S256", Resource: service.config.Resource,
	})
	if err != nil {
		t.Fatalf("인가 코드 발급: %v", err)
	}
	key := newTestDPoPKey(t)
	token, err := service.Exchange(t.Context(), code, "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource, key.proof(t, service.config.Issuer+"/token", "", time.Now().UTC()))
	if err != nil {
		t.Fatalf("DPoP 토큰 교환: %v", err)
	}
	request := DPoPRequest{AccessToken: token.Raw, Proof: key.proof(t, service.config.Resource, token.Raw, time.Now().UTC()), Method: "POST", Target: service.config.Resource}
	if authenticated, _, err := service.VerifyAndRenew(t.Context(), request); err != nil || authenticated != accountID {
		t.Fatalf("결합한 DPoP 요청 = %s, %v; want %s, nil", authenticated, err, accountID)
	}
	if _, _, err := service.VerifyAndRenew(t.Context(), request); !errors.Is(err, ErrInvalidDPoPProof) {
		t.Fatalf("같은 proof 재생 = %v, want ErrInvalidDPoPProof", err)
	}
	other := newTestDPoPKey(t)
	if _, _, err := service.VerifyAndRenew(t.Context(), DPoPRequest{AccessToken: token.Raw, Proof: other.proof(t, service.config.Resource, token.Raw, time.Now().UTC()), Method: "POST", Target: service.config.Resource}); !errors.Is(err, ErrInvalidDPoPProof) {
		t.Fatalf("다른 키 proof = %v, want ErrInvalidDPoPProof", err)
	}
	if _, _, err := service.VerifyAndRenew(t.Context(), DPoPRequest{AccessToken: token.Raw, Proof: key.proof(t, "https://other.test/mcp", token.Raw, time.Now().UTC()), Method: "POST", Target: service.config.Resource}); !errors.Is(err, ErrInvalidDPoPProof) {
		t.Fatalf("다른 대상 proof = %v, want ErrInvalidDPoPProof", err)
	}
	if _, _, err := service.VerifyAndRenew(t.Context(), DPoPRequest{AccessToken: token.Raw, Proof: key.proof(t, service.config.Resource, token.Raw, time.Now().UTC()), Method: "GET", Target: service.config.Resource}); !errors.Is(err, ErrInvalidDPoPProof) {
		t.Fatalf("다른 메서드 proof = %v, want ErrInvalidDPoPProof", err)
	}
	if _, _, err := service.VerifyAndRenew(t.Context(), DPoPRequest{AccessToken: token.Raw, Proof: key.proof(t, service.config.Resource, "other-token", time.Now().UTC()), Method: "POST", Target: service.config.Resource}); !errors.Is(err, ErrInvalidDPoPProof) {
		t.Fatalf("다른 토큰 proof = %v, want ErrInvalidDPoPProof", err)
	}
	if _, _, err := service.VerifyAndRenew(t.Context(), DPoPRequest{AccessToken: token.Raw, Proof: key.proof(t, service.config.Resource, token.Raw, time.Now().UTC().Add(-dpopProofLifetime-time.Second)), Method: "POST", Target: service.config.Resource}); !errors.Is(err, ErrInvalidDPoPProof) {
		t.Fatalf("허용 창 밖 proof = %v, want ErrInvalidDPoPProof", err)
	}
}

// TestDPoPRenewalKeepsConfirmationThumbprint는 갱신이 다른 키로 재결합되지 않고 기존
// `cnf.jkt`를 그대로 승계하는지 확인한다.
func TestDPoPRenewalKeepsConfirmationThumbprint(t *testing.T) {
	service := testService(t, newMemoryStore())
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	key := newTestDPoPKey(t)
	now := time.Now().UTC()
	// issue의 기본 수명은 1시간이라 만료 직전 토큰을 위해 직접 서명한다.
	token := signedDPoPToken(t, service, accountID, now.Add(5*time.Second), now, key.thumbprint)
	_, renewed, err := service.VerifyAndRenew(t.Context(), DPoPRequest{AccessToken: token.Raw, Proof: key.proof(t, service.config.Resource, token.Raw, now), Method: "POST", Target: service.config.Resource})
	if err != nil || renewed.Raw == "" {
		t.Fatalf("DPoP 토큰 갱신 = %+v, %v", renewed, err)
	}
	claims, err := service.verifiedClaims(t.Context(), renewed.Raw, service.config.Resource)
	if err != nil {
		t.Fatalf("갱신 토큰 검증: %v", err)
	}
	if claims.Confirmation.JWKThumbprint != key.thumbprint {
		t.Fatalf("갱신 cnf.jkt = %q, want %q", claims.Confirmation.JWKThumbprint, key.thumbprint)
	}
}

// TestDPoPProofReservationFailureIsUnavailable는 유효 proof의 단일 사용 예약이 저장소
// 장애로 끝났을 때 인증 실패로 위장하지 않는지 확인한다.
func TestDPoPProofReservationFailureIsUnavailable(t *testing.T) {
	service := testService(t, &failingStore{memoryStore: newMemoryStore(), dpopProofFails: true})
	key := newTestDPoPKey(t)
	err := func() error {
		_, err := service.verifyDPoPProof(t.Context(), DPoPRequest{
			Proof:  key.proof(t, service.config.Issuer+"/token", "", time.Now().UTC()),
			Method: "POST",
			Target: service.config.Issuer + "/token",
		}, "", false)
		return err
	}()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("proof 예약 저장소 장애 = %v, want ErrUnavailable", err)
	}
}

// TestDPoPProofRejectsMalformedProof는 SDD 「토큰 검증」의 proof JOSE와 요청 결합 항목이
// 하나라도 어긋난 proof를 예약 전에 거부하고, 경계 안의 값은 받는지 확인한다.
func TestDPoPProofRejectsMalformedProof(t *testing.T) {
	service := testService(t, newMemoryStore())
	target := service.config.Issuer + "/token"
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("P-256 키 생성: %v", err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("P-384 키 생성: %v", err)
	}
	public := jose.JSONWebKey{Key: p256.Public(), Algorithm: string(jose.ES256)}
	now := time.Now().UTC()
	claims := func(htu string, issuedAt time.Time) dpopClaims {
		proofID, err := secret()
		if err != nil {
			t.Fatalf("DPoP jti 생성: %v", err)
		}
		return dpopClaims{Claims: jwt.Claims{IssuedAt: jwt.NewNumericDate(issuedAt), ID: proofID}, Method: "POST", Target: htu}
	}
	withoutID := claims(target, now)
	withoutID.ID = ""
	for _, test := range []struct {
		name  string
		proof string
		ok    bool
	}{
		{"허용 창 안의 미래 iat", signDPoPProof(t, jose.ES256, p256, "dpop+jwt", public, claims(target, now.Add(dpopProofLifetime-time.Second))), true},
		{"typ가 dpop+jwt가 아님", signDPoPProof(t, jose.ES256, p256, "JWT", public, claims(target, now)), false},
		{"개인 키를 담은 JWK", signDPoPProof(t, jose.ES256, p256, "dpop+jwt", jose.JSONWebKey{Key: p256, Algorithm: string(jose.ES256)}, claims(target, now)), false},
		{"ES256이 아닌 alg", signDPoPProof(t, jose.ES384, p384, "dpop+jwt", jose.JSONWebKey{Key: p384.Public(), Algorithm: string(jose.ES384)}, claims(target, now)), false},
		{"jti 없음", signDPoPProof(t, jose.ES256, p256, "dpop+jwt", public, withoutID), false},
		{"허용 창 밖의 미래 iat", signDPoPProof(t, jose.ES256, p256, "dpop+jwt", public, claims(target, now.Add(dpopProofLifetime+time.Second))), false},
		{"허용 창 밖의 과거 iat", signDPoPProof(t, jose.ES256, p256, "dpop+jwt", public, claims(target, now.Add(-dpopProofLifetime-time.Second))), false},
		{"query가 있는 htu", signDPoPProof(t, jose.ES256, p256, "dpop+jwt", public, claims(target+"?x=1", now)), false},
		{"fragment가 있는 htu", signDPoPProof(t, jose.ES256, p256, "dpop+jwt", public, claims(target+"#x", now)), false},
	} {
		_, err := service.verifyDPoPProof(t.Context(), DPoPRequest{Proof: test.proof, Method: "POST", Target: target}, "", false)
		if test.ok && err != nil {
			t.Fatalf("%s: %v, want nil", test.name, err)
		}
		if !test.ok && !errors.Is(err, ErrInvalidDPoPProof) {
			t.Fatalf("%s: %v, want ErrInvalidDPoPProof", test.name, err)
		}
	}
}

// TestNormalizedDPoPTarget은 htu가 RFC 3986의 구문·scheme 기반 정규화로만 같아지고,
// 뜻이 다른 표기는 같아지지 않는지 확인한다.
func TestNormalizedDPoPTarget(t *testing.T) {
	for _, test := range []struct {
		raw, expected string
		same          bool
	}{
		{"HTTPS://Service.Test/mcp", "https://service.test/mcp", true},
		{"https://service.test:443/mcp", "https://service.test/mcp", true},
		{"http://service.test:80/mcp", "http://service.test/mcp", true},
		{"https://service.test", "https://service.test/", true},
		{"https://service.test/%6Dcp", "https://service.test/mcp", true},
		{"https://service.test/a%2fb", "https://service.test/a%2Fb", true},
		{"https://[::1]:443/mcp", "https://[::1]/mcp", true},
		{"https://service.test:8443/mcp", "https://service.test/mcp", false},
		{"https://service.test/a%2Fb", "https://service.test/a/b", false},
		{"https://service.test/MCP", "https://service.test/mcp", false},
		{"https://service.test/mcp?", "https://service.test/mcp", false},
		{"https://user@service.test/mcp", "https://service.test/mcp", false},
	} {
		if same := sameDPoPTarget(test.raw, test.expected); same != test.same {
			t.Fatalf("sameDPoPTarget(%q, %q) = %t, want %t", test.raw, test.expected, same, test.same)
		}
	}
}

func signDPoPProof(t *testing.T, algorithm jose.SignatureAlgorithm, key *ecdsa.PrivateKey, typ string, jwk jose.JSONWebKey, claims dpopClaims) string {
	t.Helper()
	options := (&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader("jwk", jwk)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: algorithm, Key: key}, options)
	if err != nil {
		t.Fatalf("DPoP 서명기 생성: %v", err)
	}
	proof, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("DPoP proof 발급: %v", err)
	}
	return proof
}

type testDPoPKey struct {
	private    *ecdsa.PrivateKey
	thumbprint string
}

func newTestDPoPKey(t *testing.T) testDPoPKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("DPoP 키 생성: %v", err)
	}
	jwk := jose.JSONWebKey{Key: private.Public(), Algorithm: string(jose.ES256)}
	thumbprint, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatalf("DPoP thumbprint: %v", err)
	}
	return testDPoPKey{private: private, thumbprint: base64.RawURLEncoding.EncodeToString(thumbprint)}
}

func (key testDPoPKey) proof(t *testing.T, target, accessToken string, issuedAt time.Time) string {
	t.Helper()
	options := (&jose.SignerOptions{}).WithType("dpop+jwt").WithHeader("jwk", jose.JSONWebKey{Key: key.private.Public(), Algorithm: string(jose.ES256)})
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key.private}, options)
	if err != nil {
		t.Fatalf("DPoP 서명기 생성: %v", err)
	}
	proofID, err := secret()
	if err != nil {
		t.Fatalf("DPoP jti 생성: %v", err)
	}
	proof, err := jwt.Signed(signer).Claims(dpopClaims{Claims: jwt.Claims{IssuedAt: jwt.NewNumericDate(issuedAt), ID: proofID}, Method: "POST", Target: target, AccessTokenHash: dpopAccessTokenHash(accessToken)}).Serialize()
	if err != nil {
		t.Fatalf("DPoP proof 발급: %v", err)
	}
	return proof
}

func signedDPoPToken(t *testing.T, service *Service, accountID model.ID, expiresAt, authenticatedAt time.Time, jkt string) Token {
	t.Helper()
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
	raw, err := jwt.Signed(signer).Claims(tokenClaims{Claims: jwt.Claims{Issuer: service.config.Issuer, Subject: accountID.String(), Audience: jwt.Audience{service.config.Resource}, IssuedAt: jwt.NewNumericDate(expiresAt.Add(-time.Hour)), Expiry: jwt.NewNumericDate(expiresAt), ID: "dpop-renewal"}, AuthenticatedAt: authenticatedAt.Unix(), Confirmation: tokenConfirmation{JWKThumbprint: jkt}}).Serialize()
	if err != nil {
		t.Fatalf("DPoP 토큰 발급: %v", err)
	}
	return Token{Raw: raw, ID: "dpop-renewal", ExpiresAt: expiresAt}
}
