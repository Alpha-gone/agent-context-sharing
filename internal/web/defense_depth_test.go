package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestDynamicResponsesDenyFraming(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	for _, path := range []string{"/login", "/register", "/graphs", "/access", "/operator/restores", "/authorize", "/connections", "/unknown"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
			request := httptest.NewRequest(method, path, nil)
			request.Header.Set("Sec-Fetch-Site", "cross-site")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Header().Get("X-Frame-Options") != "DENY" || response.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" {
				t.Fatalf("%s %s missing frame protection", method, path)
			}
		}
	}
}

func TestSessionCookieTransportNames(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	for _, secure := range []bool{true, false} {
		server.config.SecureCookie = func(*http.Request) bool { return secure }
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		expected := "agent_context_session"
		if secure {
			expected = "__Host-agent_context_session"
		}
		for _, clear := range []bool{false, true} {
			response := httptest.NewRecorder()
			if clear {
				server.clearSessionCookie(response, request)
			} else {
				server.setSessionCookie(response, request, Session{Raw: "session", ExpiresAt: time.Now().Add(time.Hour)})
			}
			cookies := response.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatal("cookie count", len(cookies))
			}
			cookie := cookies[0]
			if cookie.Name != expected || cookie.Secure != secure || cookie.Domain != "" || cookie.Path != "/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("invalid cookie attributes: %+v", cookie)
			}
		}
	}
	server.config.SecureCookie = func(*http.Request) bool { return true }
	request := httptest.NewRequest(http.MethodGet, "/graphs", nil)
	request.AddCookie(&http.Cookie{Name: "agent_context_session", Value: "legacy"})
	if _, _, err := server.verifiedSession(request); err == nil {
		t.Fatal("TLS accepted legacy cookie")
	}
}
