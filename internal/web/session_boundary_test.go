package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

type sessionBoundaryAuthentication struct {
	fakeAuthentication
	verification, revocation   error
	verifications, revocations int
	cancel                     context.CancelFunc
	cancelAt                   string
}

func (auth *sessionBoundaryAuthentication) VerifyWebSession(ctx context.Context, raw, audience string) (model.ID, Session, error) {
	auth.verifications++
	if auth.cancelAt == "verify" {
		auth.cancel()
	}
	if auth.verification != nil {
		return model.ID{}, Session{}, auth.verification
	}
	return auth.fakeAuthentication.VerifyWebSession(ctx, raw, audience)
}

func (auth *sessionBoundaryAuthentication) Revoke(context.Context, Session) error {
	auth.revocations++
	if auth.cancelAt == "revoke" {
		auth.cancel()
	}
	return auth.revocation
}

func TestProtectedSessionFailuresStopBeforeDataAccess(t *testing.T) {
	for _, path := range []string{"/graphs", "/access", "/operator/restores", "/graphs/" + testID(t).String()} {
		for _, mode := range []string{"invalid", "unavailable", "canceled before", "canceled verify"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				auth := &sessionBoundaryAuthentication{fakeAuthentication: fakeAuthentication{accountID: testID(t)}, cancel: cancel}
				expected := 303
				switch mode {
				case "invalid":
					auth.verification = errors.New("synthetic-sensitive-marker")
				case "unavailable":
					auth.verification = fmt.Errorf("%w: synthetic-sensitive-marker", ErrAuthenticationUnavailable)
					expected = 500
				case "canceled before":
					cancel()
					expected = 200
				case "canceled verify":
					auth.cancelAt = "verify"
					expected = 200
				}
				// nil인 내장 인터페이스는 실제 데이터 호출이 있으면 실패한다.
				server, err := New(auth, &sessionUntouchedStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
				request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != expected || strings.Contains(response.Body.String(), "synthetic-sensitive-marker") || len(response.Result().Cookies()) != 0 || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("세션 실패의 응답 분류·비노출 경계가 다릅니다")
				}
				if expected == 303 && response.Header().Get("Location") != "/login" {
					t.Fatal("자격 실패가 재로그인으로 이동하지 않았습니다")
				}
				if expected != 303 && response.Header().Get("Location") != "" {
					t.Fatal("장애·취소를 로그인 실패로 바꿨습니다")
				}
				if strings.HasPrefix(mode, "canceled") && response.Body.Len() != 0 {
					t.Fatal("취소 뒤 응답 생성")
				}
				if mode == "canceled before" && auth.verifications != 0 {
					t.Fatal("취소된 요청에서 검증 호출")
				}
			})
		}
	}
}

type sessionUntouchedStore struct{ GraphStore }

func TestLogoutFailureAndCancellationKeepCookie(t *testing.T) {
	for _, mode := range []string{"verification unavailable", "revocation failure", "canceled before", "canceled verify", "canceled revoke", "invalid", "missing", "success", "cross origin", "GET"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			auth := &sessionBoundaryAuthentication{fakeAuthentication: fakeAuthentication{accountID: testID(t)}, cancel: cancel}
			method, expected, writes, clear := http.MethodPost, 303, 1, true
			switch mode {
			case "verification unavailable":
				auth.verification = ErrAuthenticationUnavailable
				expected, writes, clear = 500, 0, false
			case "revocation failure":
				auth.revocation = errors.New("synthetic-sensitive-marker")
				expected, clear = 500, false
			case "canceled before":
				cancel()
				expected, writes, clear = 200, 0, false
			case "canceled verify":
				auth.cancelAt = "verify"
				expected, writes, clear = 200, 0, false
			case "canceled revoke":
				auth.cancelAt = "revoke"
				expected, clear = 200, false
			case "invalid":
				auth.verification = http.ErrNoCookie
				writes = 0
			case "missing":
				writes = 0
			case "cross origin":
				expected, writes, clear = 403, 0, false
			case "GET":
				method, expected, writes, clear = http.MethodGet, 200, 0, false
			}
			server, err := New(auth, &sessionUntouchedStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(method, "/logout", nil).WithContext(ctx)
			if mode != "missing" {
				request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
			}
			if mode == "cross origin" {
				request.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			cookies := response.Result().Cookies()
			if response.Code != expected || auth.revocations != writes || (len(cookies) > 0) != clear || strings.Contains(response.Body.String(), "synthetic-sensitive-marker") {
				t.Fatal("로그아웃 실패·취소·쿠키 경계가 다릅니다")
			}
			if clear && (cookies[0].Value != "" || cookies[0].MaxAge != -1 || cookies[0].Path != "/" || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode) {
				t.Fatal("쿠키 삭제 속성 불일치")
			}
			if expected == 500 && response.Header().Get("Location") != "" {
				t.Fatal("장애 뒤 성공 리디렉션")
			}
			if strings.HasPrefix(mode, "canceled") && response.Body.Len() != 0 {
				t.Fatal("취소 뒤 성공 응답")
			}
			if method == http.MethodPost && response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("로그아웃 응답의 no-store 누락")
			}
			if (mode == "GET" || mode == "cross origin" || mode == "missing") && auth.verifications != 0 {
				t.Fatal("선행 거부·확인 화면에서 세션 검증")
			}
		})
	}
}
