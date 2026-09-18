package authz

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

// testHandler는 세션 계정 판정을 고정한 인가 서버 처리기를 만든다.
func testHandler(t *testing.T, service *Service, accountID model.ID, authenticated bool) *Handler {
	t.Helper()
	handler, err := NewHandler(service, func(*http.Request) (model.ID, bool) {
		return accountID, authenticated
	}, "/login")
	if err != nil {
		t.Fatalf("인가 서버 처리기 생성: %v", err)
	}
	return handler
}

// authorizeQuery는 모든 단계를 통과하는 인가 요청의 질의 인자를 만든다.
func authorizeQuery(verifier string) url.Values {
	return url.Values{
		"client_id":             {"test-client"},
		"redirect_uri":          {"http://127.0.0.1/callback"},
		"code_challenge":        {digest(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {"https://service.test/mcp"},
		"state":                 {"client-state"},
	}
}

func TestAuthorizeIssuesCodeExchangeableForToken(t *testing.T) {
	service := testService(t, newMemoryStore())
	accountID, err := service.Register(t.Context(), "tester", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	verifier := "handler-pkce-verifier"
	recorder := httptest.NewRecorder()
	testHandler(t, service, accountID, true).Authorize(recorder, httptest.NewRequest(http.MethodGet, "/authorize?"+authorizeQuery(verifier).Encode(), nil))
	if recorder.Code != http.StatusFound {
		t.Fatalf("인가 응답 상태 = %d, want %d", recorder.Code, http.StatusFound)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("리다이렉트 주소 해석: %v", err)
	}
	if location.Host != "127.0.0.1" || location.Path != "/callback" {
		t.Fatalf("리다이렉트 대상 = %s, want http://127.0.0.1/callback", location)
	}
	if location.Query().Get("state") != "client-state" {
		t.Fatalf("state = %q, want client-state", location.Query().Get("state"))
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatal("인가 코드가 리다이렉트에 실리지 않았다")
	}

	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {"test-client"}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}}
	tokenRequest := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRecorder := httptest.NewRecorder()
	testHandler(t, service, accountID, true).Token(tokenRecorder, tokenRequest)
	if tokenRecorder.Code != http.StatusOK {
		t.Fatalf("토큰 응답 상태 = %d, want %d: %s", tokenRecorder.Code, http.StatusOK, tokenRecorder.Body.String())
	}
	var issued struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(tokenRecorder.Body.Bytes(), &issued); err != nil {
		t.Fatalf("토큰 응답 해석: %v", err)
	}
	if issued.TokenType != "Bearer" || issued.ExpiresIn != int(accessTokenLifetime.Seconds()) {
		t.Fatalf("토큰 응답 = %+v", issued)
	}
	if _, _, err := service.Verify(t.Context(), issued.AccessToken, "https://service.test/mcp"); err != nil {
		t.Fatalf("발급 토큰 검증: %v", err)
	}
}

func TestAuthorizeRedirectsToLoginWithoutSession(t *testing.T) {
	service := testService(t, newMemoryStore())
	recorder := httptest.NewRecorder()
	target := "/authorize?" + authorizeQuery("no-session-verifier").Encode()
	testHandler(t, service, model.ID{}, false).Authorize(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("미로그인 인가 응답 상태 = %d, want %d", recorder.Code, http.StatusSeeOther)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("리다이렉트 주소 해석: %v", err)
	}
	if location.Path != "/login" {
		t.Fatalf("리다이렉트 경로 = %q, want /login", location.Path)
	}
	// 로그인 뒤 같은 인가 요청으로 돌아와야 흐름이 이어진다.
	if location.Query().Get("next") != target {
		t.Fatalf("next = %q, want %q", location.Query().Get("next"), target)
	}
}

func TestAuthorizeDoesNotRedirectUnverifiedTarget(t *testing.T) {
	service := testService(t, newMemoryStore())
	// 1단계와 2단계의 실패는 리다이렉트하지 않는다. 돌려보낼 곳을 아직 믿을 수 없다.
	for name, query := range map[string]url.Values{
		"등록되지 않은 client_id":    {"client_id": {"other-client"}, "redirect_uri": {"http://127.0.0.1/callback"}},
		"허용되지 않은 redirect_uri": {"client_id": {"test-client"}, "redirect_uri": {"https://evil.test/steal"}},
	} {
		recorder := httptest.NewRecorder()
		testHandler(t, service, model.ID{}, true).Authorize(recorder, httptest.NewRequest(http.MethodGet, "/authorize?"+query.Encode(), nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s 응답 상태 = %d, want %d", name, recorder.Code, http.StatusBadRequest)
		}
		if location := recorder.Header().Get("Location"); location != "" {
			t.Fatalf("%s에서 리다이렉트했다: %s", name, location)
		}
	}
}

func TestAuthorizeRedirectsPKCEAndResourceErrors(t *testing.T) {
	service := testService(t, newMemoryStore())
	cases := map[string]struct {
		mutate func(url.Values)
		code   string
	}{
		"PKCE 없음":     {func(values url.Values) { values.Del("code_challenge") }, "invalid_request"},
		"plain 방식":    {func(values url.Values) { values.Set("code_challenge_method", "plain") }, "invalid_request"},
		"다른 resource": {func(values url.Values) { values.Set("resource", "https://other.test/mcp") }, "invalid_target"},
	}
	for name, testCase := range cases {
		query := authorizeQuery("redirect-error-verifier")
		testCase.mutate(query)
		recorder := httptest.NewRecorder()
		testHandler(t, service, model.ID{}, true).Authorize(recorder, httptest.NewRequest(http.MethodGet, "/authorize?"+query.Encode(), nil))
		if recorder.Code != http.StatusFound {
			t.Fatalf("%s 응답 상태 = %d, want %d", name, recorder.Code, http.StatusFound)
		}
		location, err := url.Parse(recorder.Header().Get("Location"))
		if err != nil {
			t.Fatalf("%s 리다이렉트 주소 해석: %v", name, err)
		}
		if location.Query().Get("error") != testCase.code {
			t.Fatalf("%s error = %q, want %q", name, location.Query().Get("error"), testCase.code)
		}
		if location.Query().Get("code") != "" {
			t.Fatalf("%s에서 인가 코드가 발급됐다", name)
		}
	}
}

func TestTokenRejectsUnsupportedGrantType(t *testing.T) {
	service := testService(t, newMemoryStore())
	form := url.Values{"grant_type": {"client_credentials"}}
	request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	testHandler(t, service, model.ID{}, true).Token(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("응답 상태 = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if !strings.Contains(recorder.Body.String(), "unsupported_grant_type") {
		t.Fatalf("응답 본문 = %s", recorder.Body.String())
	}
}

func TestJWKSPublishesPublicKeysOnly(t *testing.T) {
	service := testService(t, newMemoryStore())
	// 첫 발급이 활성 서명 키를 만든다.
	if _, err := service.WebSession(t.Context(), model.ID{1}, "web"); err != nil {
		t.Fatalf("세션 발급: %v", err)
	}
	recorder := httptest.NewRecorder()
	testHandler(t, service, model.ID{}, true).JWKS(recorder, httptest.NewRequest(http.MethodGet, "/jwks.json", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("응답 상태 = %d, want %d", recorder.Code, http.StatusOK)
	}
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &set); err != nil {
		t.Fatalf("JWKS 해석: %v", err)
	}
	if len(set.Keys) == 0 {
		t.Fatal("공개 키가 비어 있다")
	}
	for _, key := range set.Keys {
		// `d`는 EC 개인 키 성분이다. 공개 목록에 나가면 안 된다.
		if _, found := key["d"]; found {
			t.Fatal("JWKS에 개인 키 성분이 실렸다")
		}
	}
}
