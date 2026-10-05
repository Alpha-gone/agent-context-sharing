package authz

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type tokenReadCounter struct {
	io.ReadCloser
	reads int
}

func (body *tokenReadCounter) Read(buffer []byte) (int, error) {
	body.reads++
	return body.ReadCloser.Read(buffer)
}

func TestRedirectMatchesOnlyExactURIOrLoopbackPort(t *testing.T) {
	for _, test := range []struct {
		allowed, actual string
		want            bool
	}{
		{"https://client.test/callback", "https://client.test/callback", true},
		{"https://client.test/callback", "https://attacker@client.test/callback", false},
		{"https://client.test/callback", "https://client.test/call%62ack", false},
		{"https://client.test/callback", "https://CLIENT.test/callback", false},
		{"https://client.test/callback", "https://client.test/callback?", false},
		{"https://client.test/callback", "https://client.test:443/callback", false},
		{"https://client.test/callback?q=%61", "https://client.test/callback?q=a", false},
		{"http://127.0.0.1/callback", "http://127.0.0.1:49152/callback", true},
		{"http://[::1]/callback", "http://[::1]:49152/callback", true},
		{"http://127.0.0.1/callback", "http://attacker@127.0.0.1:49152/callback", false},
		{"http://127.0.0.1/callback", "http://127.0.0.1:49152/call%62ack", false},
		{"http://127.0.0.1/callback", "http://127.0.0.1:49152/callback?", false},
		{"http://127.0.0.1/callback", "http://127.0.0.2:49152/callback", false},
		{"http://localhost/callback", "http://localhost:49152/callback", false},
	} {
		t.Run(test.actual, func(t *testing.T) {
			allowed, err := url.Parse(test.allowed)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := url.Parse(test.actual)
			if err != nil {
				t.Fatal(err)
			}
			if got := redirectMatches(allowed, actual); got != test.want {
				t.Fatalf("리다이렉트 일치 = %t, 기대 %t", got, test.want)
			}
		})
	}
}

func TestAuthorizeRejectsRedirectNormalizationBeforeIssuingCode(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	for _, target := range []string{
		"http://attacker@127.0.0.1/callback", "http://127.0.0.1/call%62ack",
		"http://127.0.0.1/callback?", "HTTP://127.0.0.1/callback",
	} {
		t.Run(target, func(t *testing.T) {
			query := authorizeQuery(testVerifier("redirect-boundary"))
			query.Set("redirect_uri", target)
			response := httptest.NewRecorder()
			testHandler(t, service, model.ID{1}, true).Authorize(response, httptest.NewRequest(http.MethodGet, "/authorize?"+query.Encode(), nil))
			if response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" || len(backend.codes) != 0 {
				t.Fatalf("리다이렉트 거부 = %d, Location %q, 코드 %d", response.Code, response.Header().Get("Location"), len(backend.codes))
			}
		})
	}
}

func TestTokenBodyLimitBeforeCodeAndProofConsumption(t *testing.T) {
	for _, size := range []int{maxTokenBodyBytes - 1, maxTokenBodyBytes, maxTokenBodyBytes + 1} {
		for _, length := range []int64{-1, 1, int64(size)} {
			t.Run(fmt.Sprintf("크기=%d/길이=%d", size, length), func(t *testing.T) {
				backend := newMemoryStore()
				service := testService(t, backend)
				prefix := "grant_type=authorization_code&padding="
				request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(prefix+strings.Repeat("x", size-len(prefix))))
				request.ContentLength = length
				body := &tokenReadCounter{ReadCloser: request.Body}
				request.Body = body
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if size > maxTokenBodyBytes {
					request.Header.Set("DPoP", dpopProof(t, service.config.Issuer+"/token", ""))
				}
				response := httptest.NewRecorder()
				testHandler(t, service, model.ID{}, false).Token(response, request)
				wantStatus, wantError := http.StatusBadRequest, "invalid_dpop_proof"
				if size > maxTokenBodyBytes {
					wantStatus, wantError = http.StatusRequestEntityTooLarge, "invalid_request"
				}
				var payload struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if response.Code != wantStatus || payload.Error != wantError || len(backend.proofs) != 0 || len(backend.codes) != 0 {
					t.Fatalf("본문 상한 응답 = %d %q, 기대 %d %q", response.Code, payload.Error, wantStatus, wantError)
				}
				if length > maxTokenBodyBytes && body.reads != 0 {
					t.Fatalf("선언 길이 초과 본문을 %d번 읽었다", body.reads)
				}
			})
		}
	}
}

func TestVerifyAndRenewRejectsOtherAudienceWithoutDPoPBypass(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	for _, audience := range []string{"web", "https://other.test/mcp"} {
		token, err := service.WebSession(t.Context(), accountID, audience)
		if err != nil {
			t.Fatal(err)
		}
		id, renewed, err := service.VerifyAndRenew(t.Context(), DPoPRequest{AccessToken: token.Raw, Method: http.MethodPost, Target: audience})
		if !errors.Is(err, ErrInvalidCredential) || id != (model.ID{}) || renewed.Raw != "" || len(backend.proofs) != 0 {
			t.Fatalf("다른 리소스 검증 = %s, 갱신 %q, 오류 %v", id, renewed.Raw, err)
		}
	}
	key := newTestDPoPKey(t)
	now := time.Now().UTC()
	token := signedDPoPToken(t, service, accountID, now.Add(time.Hour), now, key.thumbprint)
	if _, _, err := service.VerifyAndRenew(t.Context(), DPoPRequest{AccessToken: token.Raw, Method: http.MethodPost, Target: service.config.Resource}); !errors.Is(err, ErrInvalidDPoPProof) {
		t.Fatalf("proof 없는 리소스 요청 = %v", err)
	}
}

func TestConsumedCodeSurvivesCleanupAndRevokesIssuedTokenIntegration(t *testing.T) {
	database := newAuthorizationCodeIntegrationStore(t)
	service := testService(t, database)
	accountID, err := service.Register(t.Context(), "cleanup_"+newIntegrationLoginSuffix(t), "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	verifier := testVerifier("cleanup-reuse")
	code, err := service.Authorize(t.Context(), accountID, time.Now().UTC(), AuthorizeRequest{
		ResponseType: "code", ClientID: "test-client", RedirectURI: "http://127.0.0.1/callback",
		CodeChallenge: digest(verifier), CodeChallengeMethod: "S256", Resource: service.config.Resource,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := exchangeWithDPoP(t, service, code, "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, service.config.Resource); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CleanupExpiredAuthorizationCodes(t.Context(), time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := exchangeWithDPoP(t, service, code, "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource); err == nil {
		t.Fatal("정리 뒤 소비한 코드 재사용이 허용됐다")
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, service.config.Resource); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("재사용 뒤 발급 토큰 검증 = %v", err)
	}
}
