package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestCredentialHTTPErrorBoundary(t *testing.T) {
	for _, path := range []string{"/login", "/register"} {
		for _, mode := range []string{"invalid", "unavailable", "success"} {
			t.Run(path+" "+mode, func(t *testing.T) {
				auth := &credentialBoundaryAuthentication{account: testID(t)}
				status := http.StatusSeeOther
				if mode == "invalid" {
					auth.failure = errors.New("synthetic-sensitive-internal-marker")
					status = http.StatusBadRequest
					if path == "/login" {
						status = http.StatusUnauthorized
					}
				} else if mode == "unavailable" {
					auth.failure = fmt.Errorf("%w: synthetic-sensitive-internal-marker", ErrAuthenticationUnavailable)
					status = http.StatusInternalServerError
				}
				server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, credentialRequest(path, "192.0.2.1:1234", "tester"))
				body := response.Body.String()
				if response.Code != status || strings.Contains(body, "synthetic-sensitive-internal-marker") || strings.Contains(body, "correct password") {
					t.Fatal("자격 오류의 HTTP 분류 또는 원문 비노출이 다릅니다")
				}
				if mode != "success" && (auth.sessions != 0 || len(response.Result().Cookies()) != 0 || response.Header().Get("Location") != "") {
					t.Fatal("실패한 자격 대조 뒤 세션·쿠키·성공 리디렉션이 있습니다")
				}
				if mode == "unavailable" && !strings.Contains(body, "잠시 후 다시 시도해 주세요.") {
					t.Fatal("인증 장애의 고정 한국어 안내가 없습니다")
				}
				if mode == "success" && path == "/register" && (auth.sessions != 0 || response.Header().Get("Location") != "/login?registered=1") {
					t.Fatal("가입 성공이 세션을 발급하거나 다른 곳으로 이동했습니다")
				}
				if mode == "success" && path == "/login" && (auth.sessions != 1 || len(response.Result().Cookies()) != 1) {
					t.Fatal("정상 로그인 처리가 달라졌습니다")
				}
			})
		}
	}
}

func TestCredentialHTTPCancellationStopsFollowup(t *testing.T) {
	for _, mode := range []string{"login credential", "register credential", "login session"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			auth := &credentialBoundaryAuthentication{account: testID(t), cancel: cancel, cancelAt: "credential"}
			path, sessions := "/login", 0
			if mode == "register credential" {
				path = "/register"
			} else if mode == "login session" {
				auth.cancelAt, sessions = "session", 1
			}
			server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, credentialRequest(path, "192.0.2.1:1234", "tester").WithContext(ctx))
			if auth.sessions != sessions || response.Body.Len() != 0 || response.Header().Get("Location") != "" || len(response.Result().Cookies()) != 0 {
				t.Fatal("취소 뒤 후속 세션·본문·성공 리디렉션·쿠키를 처리했습니다")
			}
		})
	}
}

type credentialBoundaryAuthentication struct {
	fakeAuthentication
	account  model.ID
	failure  error
	cancel   context.CancelFunc
	cancelAt string
	sessions int
}

func (auth *credentialBoundaryAuthentication) Authenticate(context.Context, string, string) (model.ID, error) {
	if auth.cancelAt == "credential" {
		auth.cancel()
	}
	return auth.account, auth.failure
}

func (auth *credentialBoundaryAuthentication) Register(ctx context.Context, login, password string) (model.ID, error) {
	return auth.Authenticate(ctx, login, password)
}

func (auth *credentialBoundaryAuthentication) WebSession(context.Context, model.ID, string) (Session, error) {
	auth.sessions++
	if auth.cancelAt == "session" {
		auth.cancel()
	}
	return Session{Raw: "synthetic-session", ID: "synthetic-id", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
