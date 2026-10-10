package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type countingCredentials struct {
	fakeAuthentication
	calls                     atomic.Int32
	loginError, registerError error
}

func (auth *countingCredentials) Authenticate(context.Context, string, string) (model.ID, error) {
	auth.calls.Add(1)
	return model.ID{}, auth.loginError
}
func (auth *countingCredentials) Register(context.Context, string, string) (model.ID, error) {
	auth.calls.Add(1)
	return model.ID{}, auth.registerError
}

func credentialRequest(path, peer, login string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"login_id": {login}, "password": {"correct password"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = peer
	return request
}

func TestAuthenticationAttemptsSharedAcrossRoutesAndRecover(t *testing.T) {
	auth := &countingCredentials{loginError: errors.New("wrong"), registerError: errors.New("duplicate")}
	server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	server.attempts.now = func() time.Time { return now }
	for attempt := range authSourceLimit {
		path := "/login"
		expected := http.StatusUnauthorized
		if attempt%2 == 1 {
			path, expected = "/register", http.StatusBadRequest
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, credentialRequest(path, fmt.Sprintf("192.0.2.1:%d", 1000+attempt), fmt.Sprintf("user%d", attempt)))
		if response.Code != expected {
			t.Fatalf("시도 %d = %d", attempt, response.Code)
		}
	}
	for _, path := range []string{"/login", "/register"} {
		response := httptest.NewRecorder()
		request := credentialRequest(path, "[::ffff:192.0.2.1]:3000", "otheruser")
		request.Header.Set("X-Forwarded-For", "198.51.100.5")
		request.Header.Set("CF-Connecting-IP", "198.51.100.6")
		server.ServeHTTP(response, request)
		if response.Code != 429 || response.Header().Get("Retry-After") != "60" || response.Header().Get("Cache-Control") != "no-store" || len(response.Result().Cookies()) != 0 || !strings.Contains(response.Body.String(), "잠시 후 다시 시도") {
			t.Fatal("제한 응답 계약이 다릅니다")
		}
	}
	if auth.calls.Load() != authSourceLimit {
		t.Fatal("거부 요청에서 인증을 호출했습니다")
	}
	// 같은 계정의 다른 출처는 잠기지 않는다. 한 출처의 성공도 예산을 초기화하지 않는다.
	auth.loginError = nil
	response := httptest.NewRecorder()
	server.ServeHTTP(response, credentialRequest("/login", "198.51.100.1:1234", "user0"))
	if response.Code != http.StatusSeeOther {
		t.Fatal("다른 출처의 정상 로그인이 잠겼습니다")
	}
	now = now.Add(authAttemptWindow - time.Millisecond)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, credentialRequest("/login", "192.0.2.1:1234", "user0"))
	if response.Code != 429 || response.Header().Get("Retry-After") != "1" {
		t.Fatal("재시도 올림이 다릅니다")
	}
	now = now.Add(time.Millisecond)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, credentialRequest("/login", "192.0.2.1:1234", "user0"))
	if response.Code != http.StatusSeeOther {
		t.Fatal("거부가 창을 연장하거나 정상 회복을 막았습니다")
	}
}

func TestAuthenticationAttemptsConcurrentGlobalBound(t *testing.T) {
	attempts := newAuthAttempts()
	now := time.Now()
	attempts.now = func() time.Time { return now }
	var allowed atomic.Int32
	var group sync.WaitGroup
	for i := range authGlobalLimit * 2 {
		group.Go(func() {
			if attempts.allow(netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", i+1))) == 0 {
				allowed.Add(1)
			}
		})
	}
	group.Wait()
	if allowed.Load() != authGlobalLimit || attempts.global.count != authGlobalLimit || len(attempts.sources) > authGlobalLimit {
		t.Fatal("동시 전체 상한 또는 메모리 상한이 다릅니다")
	}
	now = now.Add(time.Minute)
	if attempts.allow(netip.MustParseAddr("198.51.100.1")) != 0 || len(attempts.sources) != 1 {
		t.Fatal("전체 창 회복·만료 정리에 실패했습니다")
	}
}

func TestAuthenticationAttemptsIPv6AndCapacity(t *testing.T) {
	attempts := newAuthAttempts()
	now := time.Now()
	attempts.now = func() time.Time { return now }
	for i := range authSourceLimit {
		if attempts.allow(netip.MustParseAddr(fmt.Sprintf("2001:db8:1::%x", i+1))) != 0 {
			t.Fatal("상한 전 거부")
		}
	}
	if attempts.allow(netip.MustParseAddr("2001:db8:1::ffff")) == 0 {
		t.Fatal("IPv6 주소 변경으로 /64 제한을 우회했습니다")
	}
	if attempts.allow(netip.MustParseAddr("2001:db8:2::1")) != 0 {
		t.Fatal("다른 /64도 잠겼습니다")
	}
	clear(attempts.sources)
	for i := range authSourceCapacity {
		attempts.sources[netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}), 32)] = attemptWindow{until: now.Add(time.Minute), count: authSourceLimit}
	}
	if attempts.allow(netip.MustParseAddr("198.51.100.1")) == 0 || len(attempts.sources) != authSourceCapacity {
		t.Fatal("활성 출처가 밀려나거나 메모리가 무제한입니다")
	}
	now = now.Add(time.Minute)
	if attempts.allow(netip.MustParseAddr("198.51.100.1")) != 0 || len(attempts.sources) != 1 {
		t.Fatal("가득 찬 상태에서 회복하지 못했습니다")
	}
}

func TestAuthenticationAttemptsRejectBeforeBodyAndAuth(t *testing.T) {
	for _, path := range []string{"/login", "/register"} {
		for _, mode := range []string{"cross origin", "oversized", "malformed", "canceled", "busy"} {
			t.Run(path+mode, func(t *testing.T) {
				auth := &countingCredentials{}
				server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				request := credentialRequest(path, "192.0.2.1:1234", "tester")
				status, calls := http.StatusForbidden, int32(0)
				switch mode {
				case "cross origin":
					request.Header.Set("Sec-Fetch-Site", "cross-site")
				case "oversized":
					request = httptest.NewRequest(http.MethodPost, path, strings.NewReader("password="+strings.Repeat("a", authFormLimit)))
					request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					status = 413
				case "malformed":
					request = httptest.NewRequest(http.MethodPost, path, strings.NewReader("password=%zz"))
					request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					status = 400
				case "canceled":
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					request = request.WithContext(ctx)
					status = 200
				case "busy":
					auth.loginError, auth.registerError = ErrAuthenticationBusy, ErrAuthenticationBusy
					status, calls = 429, 1
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != status || auth.calls.Load() != calls || len(response.Result().Cookies()) != 0 {
					t.Fatalf("응답 = %d, 인증 호출 = %d", response.Code, auth.calls.Load())
				}
				if mode == "busy" && (response.Header().Get("Retry-After") != "1" || response.Header().Get("Cache-Control") != "no-store") {
					t.Fatal("작업 초과 응답이 다릅니다")
				}
				if (mode == "cross origin" || mode == "canceled") && len(server.attempts.sources) != 0 {
					t.Fatal("선행 거부가 예산을 소비했습니다")
				}
			})
		}
	}
}

func TestAuthenticationAttemptsSuccessDoesNotResetAndGETIsFree(t *testing.T) {
	auth := &countingCredentials{}
	server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	for range authSourceLimit {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, credentialRequest("/register", "192.0.2.1:1234", "tester"))
		if response.Code != 303 {
			t.Fatal("정상 가입 실패")
		}
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, credentialRequest("/login", "192.0.2.1:4321", "tester"))
	if response.Code != 429 {
		t.Fatal("정상 가입 성공으로 예산을 초기화했습니다")
	}
	for _, path := range []string{"/login", "/register", "/logout"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 200 {
			t.Fatal("GET에 인증 시도 제한을 적용했습니다")
		}
	}
}
