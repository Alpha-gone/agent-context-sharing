package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

type authorizationProbe struct {
	issued     int
	account    model.ID
	validation string
	failure    error
	cancel     context.CancelFunc
}

func (*authorizationProbe) ValidateAuthorizationTarget(client, callback, resource string) error {
	if client != "first" || callback != "http://127.0.0.1:49152/callback?fixed=1" || resource != "https://resource.test/mcp" {
		return errors.New("invalid target")
	}
	return nil
}
func (p *authorizationProbe) ValidateAuthorizationRequest(url.Values) string { return p.validation }
func (p *authorizationProbe) IssueAuthorizationCode(_ context.Context, account model.ID, _ url.Values) (string, error) {
	p.issued++
	p.account = account
	if p.cancel != nil {
		p.cancel()
	}
	return "synthetic-code", p.failure
}
func (*authorizationProbe) AuthorizationIssuer() string { return "https://issuer.test" }
func authorizationValues() url.Values {
	return url.Values{"client_id": {"first"}, "redirect_uri": {"http://127.0.0.1:49152/callback?fixed=1"}, "resource": {"https://resource.test/mcp"}, "response_type": {"code"}, "scope": {"agent-context"}, "code_challenge_method": {"S256"}, "code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "state": {"synthetic & 한글 state"}}
}

func TestOAuthAuthorizationHTTPBoundary(t *testing.T) {
	for _, test := range []struct {
		name, method, body, origin, validation string
		cookie                                 bool
		change                                 func(url.Values)
		status                                 int
		result                                 string
		issued                                 int
	}{
		{"consent view", "GET", "", "", "", true, nil, 200, "", 0},
		{"login required", "GET", "", "", "", false, nil, 303, "login", 0},
		{"post requires session", "POST", "decision=allow", "", "", false, nil, 401, "", 0},
		{"allow", "POST", "decision=allow", "", "", true, nil, 303, "code", 1},
		{"deny", "POST", "decision=deny", "", "", true, nil, 303, "access_denied", 0},
		{"missing consent", "POST", "", "", "", true, nil, 303, "invalid_request", 0},
		{"duplicate consent", "POST", "decision=allow&decision=deny", "", "", true, nil, 303, "invalid_request", 0},
		{"no implicit allow", "GET", "decision=allow", "", "", true, nil, 200, "", 0},
		{"cross origin", "POST", "decision=allow", "https://attacker.test", "", true, nil, 403, "", 0},
		{"cross site sibling", "POST", "decision=allow", "https://sibling.test", "", true, nil, 403, "", 0},
		{"unknown client", "GET", "", "", "", true, func(v url.Values) { v.Set("client_id", "unknown") }, 400, "", 0},
		{"unregistered callback", "GET", "", "", "", true, func(v url.Values) { v.Set("redirect_uri", "https://attacker.test") }, 400, "", 0},
		{"wrong resource", "GET", "", "", "", true, func(v url.Values) { v.Set("resource", "https://attacker.test/mcp") }, 400, "", 0},
		{"duplicate query", "GET", "", "", "", true, func(v url.Values) { v.Add("state", "other") }, 400, "", 0},
		{"scope error", "GET", "", "", "invalid_scope", true, nil, 303, "invalid_scope", 0},
		{"pkce error", "GET", "", "", "invalid_request", true, nil, 303, "invalid_request", 0},
		{"response error", "GET", "", "", "unsupported_response_type", true, nil, 303, "unsupported_response_type", 0},
		{"oversized query", "GET", "", "", "", true, func(v url.Values) { v.Set("state", strings.Repeat("x", authFormLimit)) }, 413, "", 0},
		{"oversized body", "POST", "decision=" + strings.Repeat("x", authFormLimit), "", "", true, nil, 413, "", 0},
		{"unsupported method", "PUT", "", "", "", true, nil, 405, "", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := testID(t)
			probe := &authorizationProbe{validation: test.validation}
			server, err := New(fakeAuthentication{accountID: account}, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }, Authorization: probe})
			if err != nil {
				t.Fatal(err)
			}
			values := authorizationValues()
			if test.change != nil {
				test.change(values)
			}
			request := httptest.NewRequest(test.method, "https://issuer.test/authorize?"+values.Encode(), strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if test.cookie {
				request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status || probe.issued != test.issued {
				t.Fatalf("인가 HTTP 상태 또는 발급 횟수 불일치: status=%d issued=%d", response.Code, probe.issued)
			}
			if probe.issued != 0 && probe.account != account {
				t.Fatal("양식 계정으로 인가 결합")
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Referrer-Policy") != "no-referrer" || response.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatal("인가 캐시·referrer·프레임 보호 누락")
			}
			if !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action") {
				t.Fatal("프레임 보호 누락 또는 등록 callback 이동을 제한하는 양식 정책")
			}
			location := response.Header().Get("Location")
			if test.result == "" {
				if location != "" {
					t.Fatal("예상하지 않은 callback 이동")
				}
				if test.status == 200 && !strings.Contains(response.Body.String(), "<legend>이 클라이언트의 연결을 허용하시겠습니까?</legend>") {
					t.Fatal("명시적 동의 양식 누락")
				}
				return
			}
			target, err := url.Parse(location)
			if err != nil {
				t.Fatal("리디렉션 해석 실패")
			}
			if test.result == "login" {
				if target.Path != "/login" || target.Query().Get("next") != request.URL.RequestURI() {
					t.Fatal("로그인 뒤 인가 요청 복원 실패")
				}
				return
			}
			query := target.Query()
			if target.Host != "127.0.0.1:49152" || query.Get("fixed") != "1" || query.Get("iss") != "https://issuer.test" || query.Get("state") != values.Get("state") {
				t.Fatal("callback·고정 query·state·iss 결합 누락")
			}
			if test.result == "code" {
				if query.Get("code") != "synthetic-code" || query.Get("error") != "" {
					t.Fatal("코드 응답 불일치")
				}
			} else if query.Get("error") != test.result || query.Get("code") != "" {
				t.Fatal("인가 오류 응답 불일치")
			}
		})
	}
}

func TestOAuthAuthorizationFailureAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name                string
		unavailable, cancel bool
	}{
		{"session store unavailable", true, false}, {"issue failed", false, false}, {"cancel after issue", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := testID(t)
			probe := &authorizationProbe{failure: errors.New("synthetic private detail")}
			auth := &sessionBoundaryAuthentication{fakeAuthentication: fakeAuthentication{accountID: account}}
			if test.unavailable {
				auth.verification = ErrAuthenticationUnavailable
			}
			server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }, Authorization: probe})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				probe.cancel = cancel
			}
			request := httptest.NewRequest("POST", "https://issuer.test/authorize?"+authorizationValues().Encode(), strings.NewReader("decision=allow")).WithContext(ctx)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if !test.cancel && response.Code != 500 {
				t.Fatal("내부 장애를 재인증으로 사상")
			}
			if response.Header().Get("Location") != "" || strings.Contains(response.Body.String(), "synthetic") || len(response.Result().Cookies()) != 0 {
				t.Fatal("장애·취소 뒤 자격·내부 원문·성공 이동")
			}
		})
	}
}

func TestOAuthAuthorizationSharesLoginBudget(t *testing.T) {
	probe := &authorizationProbe{}
	server, err := New(fakeAuthentication{accountID: testID(t)}, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }, Authorization: probe})
	if err != nil {
		t.Fatal(err)
	}
	for range authSourceLimit {
		request := httptest.NewRequest("POST", "https://issuer.test/login", strings.NewReader("login_id=test&password=password"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 303 {
			t.Fatal("시험 로그인 예산 준비 실패")
		}
	}
	request := httptest.NewRequest("POST", "https://issuer.test/authorize?"+authorizationValues().Encode(), unreadableCSRFBody{t})
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 429 || probe.issued != 0 || response.Header().Get("Retry-After") == "" {
		t.Fatal("동의 요청이 공유 인증 예산을 우회")
	}
}
