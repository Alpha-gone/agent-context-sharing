package web

import (
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func registrationAttribute(body, pattern string) string {
	match := regexp.MustCompile(pattern).FindStringSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	return html.UnescapeString(match[1])
}

func TestRegistrationPreservesReturnTarget(t *testing.T) {
	authorize := "/authorize?" + authorizationValues().Encode()
	for _, sample := range []struct{ name, next, want string }{
		{"authorize", authorize, authorize}, {"local", "/graphs?name=한글+%26test", "/graphs?name=한글+%26test"},
		{"empty", "", ""}, {"external", "https://evil.test/steal", ""}, {"network", "//evil.test/steal", ""},
		{"script", "javascript:alert(1)", ""}, {"malformed", "/%zz", ""},
		{"attribute", `/graphs?value=\"><img src=x onerror=bad()>`, `/graphs?value=\"><img src=x onerror=bad()>`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			server := newTestServer(t, testID(t), testID(t))
			get := func(path string) *httptest.ResponseRecorder {
				t.Helper()
				response := httptest.NewRecorder()
				server.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
				if response.Code != 200 {
					t.Fatalf("GET 상태=%d", response.Code)
				}
				return response
			}
			page := get("/login?" + url.Values{"next": {sample.next}}.Encode())
			registerLink := registrationAttribute(page.Body.String(), `<a href="([^"]*)">계정 등록</a>`)
			target, err := url.Parse(registerLink)
			if err != nil || target.Path != "/register" || target.Query().Get("next") != sample.want {
				t.Fatalf("등록 링크=%s", registerLink)
			}
			page = get(registerLink)
			hidden := registrationAttribute(page.Body.String(), `<input[^>]*name="next"[^>]*value="([^"]*)"`)
			back := registrationAttribute(page.Body.String(), `<a href="([^"]*)">로그인으로 돌아가기</a>`)
			backURL, err := url.Parse(back)
			if err != nil || backURL.Path != "/login" || backURL.Query().Get("next") != sample.want || hidden != sample.want {
				t.Fatalf("등록의 복귀 상태 hidden=%q back=%q", hidden, back)
			}
			if strings.Contains(page.Body.String(), "<img src=x") {
				t.Fatal("복귀 주소가 HTML 속성을 벗어남")
			}
			post := func(path string, form url.Values) *httptest.ResponseRecorder {
				t.Helper()
				request := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				return response
			}
			response := post("/register", url.Values{"login_id": {"new_account"}, "password": {"correct password"}, "next": {hidden}})
			login, err := url.Parse(response.Header().Get("Location"))
			if err != nil || response.Code != 303 || login.Path != "/login" || login.Query().Get("registered") != "1" || login.Query().Get("next") != sample.want {
				t.Fatalf("가입 뒤 이동=%s status=%d", login, response.Code)
			}
			page = get(login.RequestURI())
			next := registrationAttribute(page.Body.String(), `<input[^>]*name="next"[^>]*value="([^"]*)"`)
			if next != sample.want {
				t.Fatal("로그인 양식 복귀 주소 누락")
			}
			response = post("/login", url.Values{"login_id": {"new_account"}, "password": {"correct password"}, "next": {next}})
			want := sample.want
			if want == "" {
				want = "/graphs"
			}
			gotURL, err := url.Parse(response.Header().Get("Location"))
			wantURL, _ := url.Parse(want)
			if response.Code != 303 || err != nil || gotURL.Scheme != "" || gotURL.Host != "" || gotURL.Path != wantURL.Path || gotURL.Query().Encode() != wantURL.Query().Encode() {
				t.Fatalf("로그인 뒤 복귀=%q", response.Header().Get("Location"))
			}
		})
	}
}

func TestRegistrationFailureKeepsSafeReturnTarget(t *testing.T) {
	for _, failure := range []error{errors.New("duplicate login"), ErrAuthenticationUnavailable} {
		for _, next := range []string{"/authorize?client_id=first&state=한글+%26", "https://evil.test/steal"} {
			t.Run(failure.Error()+"/"+next, func(t *testing.T) {
				auth := &countingCredentials{registerError: failure}
				server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				form := url.Values{"login_id": {"new_account"}, "password": {"secret password"}, "next": {next}}
				request := httptest.NewRequest("POST", "/register", strings.NewReader(form.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				wantStatus := 400
				if errors.Is(failure, ErrAuthenticationUnavailable) {
					wantStatus = 500
				}
				want := next
				if strings.HasPrefix(next, "https:") {
					want = ""
				}
				if response.Code != wantStatus || registrationAttribute(response.Body.String(), `<input[^>]*name="next"[^>]*value="([^"]*)"`) != want {
					t.Fatalf("실패 양식 복귀 상태=%d", response.Code)
				}
				if strings.Contains(response.Body.String(), "secret password") {
					t.Fatal("가입 실패 양식에 비밀번호 재노출")
				}
			})
		}
	}
}

func TestRegistrationRejectsPostedExternalReturnTarget(t *testing.T) {
	for _, next := range []string{"https://evil.test/steal", "//evil.test/steal", "javascript:alert(1)", "/%zz", ""} {
		t.Run(next, func(t *testing.T) {
			server := newTestServer(t, testID(t), testID(t))
			form := url.Values{"login_id": {"new_account"}, "password": {"correct password"}, "next": {next}}
			request := httptest.NewRequest("POST", "/register?next="+url.QueryEscape("/authorize?client_id=first"), strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			target, err := url.Parse(response.Header().Get("Location"))
			if err != nil || response.Code != 303 || target.Path != "/login" || target.Query().Get("registered") != "1" || target.Query().Has("next") {
				t.Fatalf("등록 POST의 외부/미지정 복귀 주소=%s", target)
			}
		})
	}
}
