package web

import (
	"context"
	"errors"
	"fmt"
	"io"
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

type observedCredentialBody struct {
	io.Reader
	reads      int
	beforeRead func()
}

func (body *observedCredentialBody) Read(buffer []byte) (int, error) {
	body.reads++
	if body.beforeRead != nil {
		body.beforeRead()
	}
	return body.Reader.Read(buffer)
}

func (*observedCredentialBody) Close() error { return nil }

func TestAuthenticationFormBoundary(t *testing.T) {
	for _, path := range []string{"/login", "/register"} {
		for _, test := range []struct {
			name               string
			body               string
			types              []string
			unknownLength      bool
			status             int
			read, authenticate bool
		}{
			{"below limit", "padding=" + strings.Repeat("a", authFormLimit-9), []string{"application/x-www-form-urlencoded"}, false, 303, true, true},
			{"at limit", "padding=" + strings.Repeat("a", authFormLimit-8), []string{"application/x-www-form-urlencoded"}, false, 303, true, true},
			{"known oversized", strings.Repeat("a", authFormLimit+1), []string{"application/x-www-form-urlencoded"}, false, 413, false, false},
			{"stream at limit", "padding=" + strings.Repeat("a", authFormLimit-8), []string{"application/x-www-form-urlencoded"}, true, 303, true, true},
			{"stream oversized", strings.Repeat("a", authFormLimit+1), []string{"application/x-www-form-urlencoded"}, true, 413, true, false},
			{"parameters", "login_id=tester", []string{"Application/X-Www-Form-Urlencoded; charset=UTF-8"}, false, 303, true, true},
			{"missing type", "login_id=tester", nil, false, 400, false, false},
			{"unsupported type", "{}", []string{"application/json"}, false, 400, false, false},
			{"multipart", "ignored", []string{"multipart/form-data; boundary=example"}, false, 400, false, false},
			{"duplicate type", "login_id=tester", []string{"application/x-www-form-urlencoded", "application/x-www-form-urlencoded"}, false, 400, false, false},
			{"invalid parameter", "login_id=tester", []string{"application/x-www-form-urlencoded; charset"}, false, 400, false, false},
			{"malformed form", "password=%zz", []string{"application/x-www-form-urlencoded"}, false, 400, true, false},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				auth := &countingCredentials{}
				server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(test.body))
				body := &observedCredentialBody{Reader: request.Body}
				request.Body = body
				request.Header["Content-Type"] = test.types
				if test.unknownLength {
					request.ContentLength = -1
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != test.status || response.Header().Get("Cache-Control") != "no-store" || (body.reads > 0) != test.read || (auth.calls.Load() > 0) != test.authenticate {
					t.Fatalf("양식 경계: status=%d reads=%d auth=%d", response.Code, body.reads, auth.calls.Load())
				}
				if server.attempts.global.count != 1 {
					t.Fatal("양식 오류가 공유 예산을 소비하지 않았습니다")
				}
			})
		}
	}
}

func TestAuthenticationCancellationAndEarlyRejection(t *testing.T) {
	for _, path := range []string{"/login", "/register"} {
		for _, mode := range []string{"address cancellation", "body cancellation", "cross origin", "source limit"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				auth := &countingCredentials{}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				config := Config{SecureCookie: func(*http.Request) bool { return true }}
				if mode == "address cancellation" {
					config.ClientAddress = func(*http.Request) netip.Addr { cancel(); return netip.MustParseAddr("192.0.2.1") }
				}
				server, err := New(auth, &fakeGraphStore{}, config)
				if err != nil {
					t.Fatal(err)
				}
				request := credentialRequest(path, "192.0.2.1:1234", "tester").WithContext(ctx)
				body := &observedCredentialBody{Reader: request.Body}
				request.Body = body
				switch mode {
				case "body cancellation":
					body.beforeRead = cancel
				case "cross origin":
					request.Header.Set("Sec-Fetch-Site", "cross-site")
				case "source limit":
					for range authSourceLimit {
						server.attempts.allow(netip.MustParseAddr("192.0.2.1"))
					}
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if auth.calls.Load() != 0 || len(response.Result().Cookies()) != 0 || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("선행 거부에서 인증 호출·쿠키 발급 또는 캐시 허용")
				}
				if mode == "body cancellation" {
					if body.reads == 0 || server.attempts.global.count != 1 || response.Body.Len() != 0 {
						t.Fatal("읽기 중 취소 경계가 다릅니다")
					}
				} else if body.reads != 0 {
					t.Fatal("선행 거부에서 본문을 읽었습니다")
				}
				if mode == "address cancellation" && (server.attempts.global.count != 0 || response.Body.Len() != 0) {
					t.Fatal("주소 해석 중 취소가 예산·응답을 생성했습니다")
				}
				if mode == "cross origin" && response.Code != 403 {
					t.Fatal("출처 거부 응답 변경")
				}
				if mode == "source limit" && response.Code != 429 {
					t.Fatal("시도 제한 응답 변경")
				}
			})
		}
	}
}

func TestAuthenticationCapacityRetryUsesRemainingWindows(t *testing.T) {
	auth := &countingCredentials{}
	server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	server.attempts.now = func() time.Time { return now }
	for i := range authSourceCapacity {
		server.attempts.sources[netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}), 32)] = attemptWindow{until: now.Add(10100 * time.Millisecond), count: 1}
	}
	for _, seconds := range []string{"11", "10"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, credentialRequest("/login", "192.0.2.1:1234", "tester"))
		if response.Code != 429 || response.Header().Get("Retry-After") != seconds || auth.calls.Load() != 0 || len(server.attempts.sources) != authSourceCapacity {
			t.Fatal("활성 상태 상한의 재시도·무인증·보존 경계가 다릅니다")
		}
		now = now.Add(time.Second)
	}
	server.attempts.global = attemptWindow{until: now.Add(20 * time.Second), count: authGlobalLimit}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, credentialRequest("/login", "192.0.2.1:1234", "tester"))
	if response.Header().Get("Retry-After") != "20" {
		t.Fatal("가득 찬 상태에서 전체 제한 창을 누락했습니다")
	}
	now = now.Add(20 * time.Second)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, credentialRequest("/register", "192.0.2.1:1234", "tester"))
	if response.Code != 303 || len(server.attempts.sources) != 1 {
		t.Fatal("만료 상태 정리 뒤 회복하지 못했습니다")
	}
}

func TestAuthenticationMalformedPeersShareUnknownBudget(t *testing.T) {
	auth := &countingCredentials{loginError: errors.New("wrong")}
	server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	peers := []string{"invalid:1234", "192.0.2.1", "192.0.2.2:invalid", "[fe80::1%en0]:1234", ""}
	for i := range authSourceLimit + 1 {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, credentialRequest("/login", peers[i%len(peers)], "tester"))
		expected := 401
		if i == authSourceLimit {
			expected = 429
		}
		if response.Code != expected {
			t.Fatal("주소 해석 실패가 공통 출처 예산을 우회했습니다")
		}
	}
	if auth.calls.Load() != authSourceLimit || len(server.attempts.sources) != 1 {
		t.Fatal("실패 주소별 상태가 증가했습니다")
	}
}

func TestAuthenticationHTTPConcurrentGlobalBound(t *testing.T) {
	auth := &countingCredentials{loginError: errors.New("wrong"), registerError: errors.New("duplicate")}
	server, err := New(auth, &fakeGraphStore{}, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	server.attempts.now = func() time.Time { return now }
	var limited, unexpected atomic.Int32
	var workers sync.WaitGroup
	for i := range authGlobalLimit * 2 {
		workers.Go(func() {
			path, expected := "/login", 401
			if i%2 == 1 {
				path, expected = "/register", 400
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, credentialRequest(path, fmt.Sprintf("192.0.2.%d:1234", i+1), "tester"))
			if response.Code == 429 && response.Header().Get("Retry-After") == "60" {
				limited.Add(1)
			} else if response.Code != expected {
				unexpected.Add(1)
			}
		})
	}
	workers.Wait()
	if limited.Load() != authGlobalLimit || unexpected.Load() != 0 || auth.calls.Load() != authGlobalLimit {
		t.Fatal("동시 HTTP 로그인·가입이 전체 예산을 초과했습니다")
	}
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
