package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthenticationSixPrefixesCannotBlockOtherSources(t *testing.T) {
	account := testID(t)
	auth := &countingCredentials{fakeAuthentication: fakeAuthentication{accountID: account}, loginError: errors.New("wrong")}
	probe := new(authorizationProbe)
	server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }, Authorization: probe})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	server.attempts.now = func() time.Time { return now }
	// /56 안의 여섯 /64가 각 10회씩 이전 전체 한도 60회를 소비한다.
	for prefix := range 6 {
		for range 10 {
			response := httptest.NewRecorder()
			server.ServeHTTP(response, credentialRequest("/login", fmt.Sprintf("[2001:db8:193:%x::1]:1234", prefix), "tester"))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("attack fixture status=%d", response.Code)
			}
		}
	}
	auth.loginError = nil
	for _, path := range []string{"/login", "/register"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, credentialRequest(path, "198.51.100.1:1234", "tester"))
		if response.Code != http.StatusSeeOther || response.Header().Get("Retry-After") != "" {
			t.Errorf("unrelated %s blocked: %d", path, response.Code)
		}
	}
	for _, decision := range []string{"allow", "deny"} {
		request := httptest.NewRequest(http.MethodPost, "https://issuer.test/authorize?"+authorizationValues().Encode(), strings.NewReader("decision="+decision))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
		request.RemoteAddr = "198.51.100.2:1234"
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusSeeOther || response.Header().Get("Retry-After") != "" {
			t.Errorf("unrelated consent %s blocked: %d", decision, response.Code)
		}
		location := response.Header().Get("Location")
		if decision == "allow" && !strings.Contains(location, "code=synthetic-code") || decision == "deny" && !strings.Contains(location, "error=access_denied") {
			t.Errorf("consent redirect=%q", location)
		}
	}
	if probe.issued != 1 || probe.account != account {
		t.Errorf("consent issuance=%d account=%s", probe.issued, probe.account)
	}
	// 유효 세션도 같은 출처의 시도 제한을 우회하지 못한다.
	request := httptest.NewRequest(http.MethodPost, "https://issuer.test/authorize?"+authorizationValues().Encode(), strings.NewReader("decision=allow"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
	request.RemoteAddr = "[2001:db8:193:0::ffff]:1234"
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "60" || probe.issued != 1 {
		t.Fatal("same-prefix budget/session boundary changed")
	}
}
