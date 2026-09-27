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
		"response_type":         {"code"},
		"scope":                 {"agent-context"},
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
	verifier := testVerifier("handler-pkce")
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

	form := tokenForm(code, verifier)
	tokenRequest := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRequest.Header.Set("DPoP", dpopProof(t, "https://service.test/token", ""))
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
	if issued.TokenType != "DPoP" || issued.ExpiresIn != int(accessTokenLifetime.Seconds()) {
		t.Fatalf("토큰 응답 = %+v", issued)
	}
	if _, _, err := service.Verify(t.Context(), issued.AccessToken, "https://service.test/mcp"); err != nil {
		t.Fatalf("발급 토큰 검증: %v", err)
	}
}

func TestAuthorizeRedirectsToLoginWithoutSession(t *testing.T) {
	service := testService(t, newMemoryStore())
	recorder := httptest.NewRecorder()
	target := "/authorize?" + authorizeQuery(testVerifier("no-session")).Encode()
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
		"PKCE 없음":               {func(values url.Values) { values.Del("code_challenge") }, "invalid_request"},
		"plain 방식":              {func(values url.Values) { values.Set("code_challenge_method", "plain") }, "invalid_request"},
		"다른 resource":           {func(values url.Values) { values.Set("resource", "https://other.test/mcp") }, "invalid_target"},
		"지원하지 않는 response_type": {func(values url.Values) { values.Set("response_type", "token") }, "unsupported_response_type"},
		"response_type 없음":      {func(values url.Values) { values.Del("response_type") }, "unsupported_response_type"},
		"허용되지 않은 scope":         {func(values url.Values) { values.Set("scope", "admin") }, "invalid_scope"},
	}
	for name, testCase := range cases {
		query := authorizeQuery(testVerifier("redirect-error"))
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
	request.Header.Set("DPoP", dpopProof(t, service.config.Issuer+"/token", ""))
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

// testVerifier는 RFC 7636의 길이 요건을 채운 PKCE 검증기를 만든다. 이름을 남겨 두어
// 실패한 테스트가 어느 검증기를 쓴 것인지 보이게 한다.
func testVerifier(name string) string {
	verifier := name + "-verifier"
	for len(verifier) < 43 {
		verifier += "-x"
	}
	return verifier
}

// tokenForm은 모든 단계를 통과하는 토큰 요청의 폼 값을 만든다.
func tokenForm(code, verifier string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {"test-client"},
		"redirect_uri":  {"http://127.0.0.1/callback"},
		"code_verifier": {verifier},
		"resource":      {"https://service.test/mcp"},
	}
}

// issueCode는 인가 단계를 거쳐 교환 가능한 인가 코드를 만든다.
func issueCode(t *testing.T, service *Service, verifier string) string {
	t.Helper()
	accountID, err := service.Register(t.Context(), "tester", "correct horse battery staple")
	if err != nil {
		t.Fatalf("계정 등록: %v", err)
	}
	recorder := httptest.NewRecorder()
	testHandler(t, service, accountID, true).Authorize(recorder, httptest.NewRequest(http.MethodGet, "/authorize?"+authorizeQuery(verifier).Encode(), nil))
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("리다이렉트 주소 해석: %v", err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("인가 코드가 발급되지 않았다: %s", recorder.Body.String())
	}
	return code
}

// postToken은 주어진 폼으로 토큰 요청을 보낸다.
func postToken(t *testing.T, service *Service, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("DPoP", dpopProof(t, service.config.Issuer+"/token", ""))
	recorder := httptest.NewRecorder()
	testHandler(t, service, model.ID{}, true).Token(recorder, request)
	return recorder
}

// TestTokenRequiresMatchingResource는 토큰 요청의 리소스 결속을 확인한다.
//
// RFC 8707은 `resource`를 인가 요청과 토큰 요청 양쪽에 요구한다. 토큰 단계에서 확인하지
// 않으면 인가 때 고른 대상 리소스와 실제 발급 대상이 갈라져 결속이 성립하지 않는다.
func TestTokenRequiresMatchingResource(t *testing.T) {
	for name, mutate := range map[string]func(url.Values){
		"resource 누락": func(values url.Values) { values.Del("resource") },
		"빈 resource":  func(values url.Values) { values.Set("resource", "") },
		"다른 resource": func(values url.Values) { values.Set("resource", "https://other.test/mcp") },
	} {
		t.Run(name, func(t *testing.T) {
			service := testService(t, newMemoryStore())
			verifier := testVerifier("resource-binding")
			form := tokenForm(issueCode(t, service, verifier), verifier)
			mutate(form)
			recorder := postToken(t, service, form)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("응답 상태 = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "invalid_grant") {
				t.Fatalf("응답 본문 = %s", recorder.Body.String())
			}
		})
	}
}

// TestTokenValidatesVerifierSyntax는 PKCE 검증기의 길이와 문자 집합을 확인한다.
//
// 해시 대조만 하면 43자보다 짧거나 허용되지 않은 문자를 담은 검증기도 통과해, RFC 7636이
// 길이와 문자 집합으로 보장하려던 엔트로피가 사라진다.
func TestTokenValidatesVerifierSyntax(t *testing.T) {
	for name, testCase := range map[string]struct {
		verifier string
		accepted bool
	}{
		"42자":        {strings.Repeat("a", 42), false},
		"43자 경계":     {strings.Repeat("a", 43), true},
		"128자 경계":    {strings.Repeat("a", 128), true},
		"129자":       {strings.Repeat("a", 129), false},
		"허용되지 않은 문자": {strings.Repeat("a", 42) + "+", false},
		"빈 값":        {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			service := testService(t, newMemoryStore())
			recorder := postToken(t, service, tokenForm(issueCode(t, service, testCase.verifier), testCase.verifier))
			if testCase.accepted {
				if recorder.Code != http.StatusOK {
					t.Fatalf("경계 안의 검증기가 거부됐다: %d %s", recorder.Code, recorder.Body.String())
				}
				return
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("경계 밖의 검증기가 허용됐다: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestTokenResponseCarriesConfiguredScope는 토큰 응답의 scope가 리소스 URL이 아니라
// 확정된 scope 값인지 확인한다. 두 값은 다른 것이며 클라이언트가 재인가에 쓰는 것은 후자다.
func TestTokenResponseCarriesConfiguredScope(t *testing.T) {
	service := testService(t, newMemoryStore())
	verifier := testVerifier("scope-value")
	recorder := postToken(t, service, tokenForm(issueCode(t, service, verifier), verifier))
	if recorder.Code != http.StatusOK {
		t.Fatalf("토큰 응답 상태 = %d: %s", recorder.Code, recorder.Body.String())
	}
	var issued struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &issued); err != nil {
		t.Fatalf("토큰 응답 해석: %v", err)
	}
	if issued.Scope != "agent-context" {
		t.Fatalf("scope = %q, want agent-context", issued.Scope)
	}
}

// TestAuthorizeResponsesCarryIssuer는 성공과 오류 인가 응답 모두에 `iss`가 실리는지
// 확인한다. 클라이언트는 발신자를 확인한 뒤에야 응답을 신뢰할 수 있고, 오류 응답도
// 같은 검증을 거친다.
func TestAuthorizeResponsesCarryIssuer(t *testing.T) {
	const issuer = "https://service.test"
	t.Run("성공 응답", func(t *testing.T) {
		service := testService(t, newMemoryStore())
		accountID, err := service.Register(t.Context(), "tester", "correct horse battery staple")
		if err != nil {
			t.Fatalf("계정 등록: %v", err)
		}
		recorder := httptest.NewRecorder()
		query := authorizeQuery(testVerifier("issuer-success"))
		testHandler(t, service, accountID, true).Authorize(recorder, httptest.NewRequest(http.MethodGet, "/authorize?"+query.Encode(), nil))
		location, err := url.Parse(recorder.Header().Get("Location"))
		if err != nil {
			t.Fatalf("리다이렉트 주소 해석: %v", err)
		}
		if location.Query().Get("iss") != issuer {
			t.Fatalf("iss = %q, want %q", location.Query().Get("iss"), issuer)
		}
	})

	t.Run("오류 응답", func(t *testing.T) {
		service := testService(t, newMemoryStore())
		query := authorizeQuery(testVerifier("issuer-error"))
		query.Del("code_challenge")
		recorder := httptest.NewRecorder()
		testHandler(t, service, model.ID{}, true).Authorize(recorder, httptest.NewRequest(http.MethodGet, "/authorize?"+query.Encode(), nil))
		location, err := url.Parse(recorder.Header().Get("Location"))
		if err != nil {
			t.Fatalf("리다이렉트 주소 해석: %v", err)
		}
		if location.Query().Get("error") == "" {
			t.Fatal("오류 응답이 아니다")
		}
		if location.Query().Get("iss") != issuer {
			t.Fatalf("오류 응답 iss = %q, want %q", location.Query().Get("iss"), issuer)
		}
	})
}
