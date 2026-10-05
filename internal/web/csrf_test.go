package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"agent_context_sharing/internal/model"
)

type observedAuthentication struct {
	fakeAuthentication
	calls int
}

func (authentication *observedAuthentication) VerifyWebSession(ctx context.Context, raw, audience string) (model.ID, Session, error) {
	authentication.calls++
	return authentication.fakeAuthentication.VerifyWebSession(ctx, raw, audience)
}

type unreadableCSRFBody struct{ t *testing.T }

func (body unreadableCSRFBody) Read([]byte) (int, error) {
	body.t.Fatal("출처 검사 전에 요청 본문을 읽었다")
	return 0, nil
}
func (unreadableCSRFBody) Close() error { return nil }

func TestWebRejectsCrossOriginMutationsBeforeAuthentication(t *testing.T) {
	accountID, graphID := testID(t), testID(t)
	for _, target := range []string{
		"/login", "/register", "/logout", "/access", "/graphs",
		"/graphs/" + graphID.String() + "/access",
		"/graphs/" + graphID.String() + "/deletion", "/operator/restores",
	} {
		for _, metadata := range []struct{ name, value string }{
			{"Origin", "https://attacker.test"},
			{"Origin", "https://sibling.example.com"},
			{"Sec-Fetch-Site", "cross-site"},
			{"Sec-Fetch-Site", "same-site"},
		} {
			t.Run(target+"/"+metadata.name+"/"+metadata.value, func(t *testing.T) {
				authentication := &observedAuthentication{fakeAuthentication: fakeAuthentication{accountID: accountID}}
				graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, grade: model.GraphGradeOwner}
				server, err := New(authentication, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				request := sessionRequest(http.MethodPost, "https://service.example.com"+target, nil)
				request.Header.Set(metadata.name, metadata.value)
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				request.Body = unreadableCSRFBody{t: t}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden || len(response.Header().Values("Set-Cookie")) != 0 || authentication.calls != 0 || graphs.granted {
					t.Fatalf("출처 거부 = %d, 쿠키 %v, 세션 검증 %d, 등급 부여 %t", response.Code, response.Header().Values("Set-Cookie"), authentication.calls, graphs.granted)
				}
			})
		}
	}
}

func TestWebAllowsSameOriginAndNonBrowserLogin(t *testing.T) {
	for _, metadata := range []struct{ name, value string }{
		{"Origin", "https://service.test"}, {"Sec-Fetch-Site", "same-origin"}, {"", ""},
	} {
		t.Run(metadata.name, func(t *testing.T) {
			server := newTestServer(t, model.ID{}, model.ID{})
			request := sessionRequest(http.MethodPost, "https://service.test/login", url.Values{"login_id": {"tester"}, "password": {"password"}})
			if metadata.name != "" {
				request.Header.Set(metadata.name, metadata.value)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusSeeOther || len(response.Header().Values("Set-Cookie")) != 1 {
				t.Fatalf("로그인 = %d, 쿠키 %v", response.Code, response.Header().Values("Set-Cookie"))
			}
		})
	}
}

func TestCrossSiteLoginGETDoesNotCreateSession(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "https://service.test/login", nil)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	newTestServer(t, model.ID{}, model.ID{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(response.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("로그인 화면 = %d, 쿠키 %v", response.Code, response.Header().Values("Set-Cookie"))
	}
}
