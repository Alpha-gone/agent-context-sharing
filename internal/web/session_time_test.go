package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestSessionAccountPreservesAuthenticationTime(t *testing.T) {
	accountID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	authenticatedAt := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	server, err := New(fakeAuthentication{accountID: accountID, authenticatedAt: authenticatedAt}, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/authorize", nil)
	if _, got, ok := server.SessionAccount(request); ok || !got.IsZero() {
		t.Fatal("쿠키 없이 인증 시각이 전달됐다")
	}
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
	id, got, ok := server.SessionAccount(request)
	if !ok || id != accountID || !got.Equal(authenticatedAt) {
		t.Fatalf("세션 계정과 인증 시각 = %s, %s, %t", id, got, ok)
	}
}
