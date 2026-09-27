package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// gatedPath는 TLS 판정 뒤에 있는 경로다. 업무 경로는 아직 등록되지 않았으므로 이 경로로
// 온 요청은 판정에 막히면 400, 판정을 지나면 등록되지 않아 404가 된다. 게이트 통과 여부를
// 이 두 상태로 구분한다.
const gatedPath = "/graphs"

// TestStatusPathsBypassTLSGate는 신뢰 대역 밖에서 전달 헤더 없이 온 평문 요청에도 상태
// 확인 경로가 응답하는지 확인한다. FR-AGENT_CONTEXT-140의 예외이며, 프로브는 프록시를
// 거치지 않고 오므로 이 예외가 없으면 생존 확인이 항상 실패한다.
func TestStatusPathsBypassTLSGate(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)

	health := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.RemoteAddr = "203.0.113.9:54321"
	app.handler().ServeHTTP(health, request)
	if health.Code != http.StatusOK {
		t.Fatalf("평문 healthz 상태 = %d, want 200", health.Code)
	}

	ready := httptest.NewRecorder()
	app.handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("평문 readyz 상태 = %d, want 200", ready.Code)
	}
}

// TestHealthAndReadySeparateFailures는 생존 확인과 데이터베이스 준비 확인의 실패를 구분한다.
func TestHealthAndReadySeparateFailures(t *testing.T) {
	database := &fakeReadiness{pingError: errors.New("데이터베이스 중단")}
	app := newApplication(database, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	handler := app.handler()

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("healthz 상태 = %d, want 200", health.Code)
	}

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz 상태 = %d, want 503", ready.Code)
	}
	if ready.Header().Get("X-Correlation-ID") == "" {
		t.Fatal("상관 식별자 헤더가 없다")
	}
}

// TestShutdownStopsReadinessAndLeavesPoolOpen은 종료가 준비 상태를 내리고 진행 요청을
// 기다리되 연결 풀은 닫지 않는지 확인한다. 풀은 색인 작업자도 쓰므로 닫는 순서를 조립한
// 곳이 정하며, 여기에서 닫으면 작업자가 도는 중에 사라진다.
func TestShutdownStopsReadinessAndLeavesPoolOpen(t *testing.T) {
	database := &fakeReadiness{}
	app := newApplication(database, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	server := &http.Server{Handler: app.handler()}
	if err := app.shutdown(t.Context(), server); err != nil {
		t.Fatalf("정상 종료: %v", err)
	}
	if database.closed {
		t.Fatal("종료가 연결 풀까지 닫았다")
	}
	ready := httptest.NewRecorder()
	app.handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("종료 뒤 readyz 상태 = %d, want 503", ready.Code)
	}
}

// TestTransportSecurityRejectsPlaintext는 프록시 종단에서 신뢰된 HTTPS 전달 헤더가 없는 요청을 거부하는지 확인한다.
func TestTransportSecurityRejectsPlaintext(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, gatedPath, nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("평문 요청 상태 = %d, want 400", response.Code)
	}
}

// TestTransportSecurityRejectsForgedForwardedProto는 신뢰 대역 밖에서 온 요청이 HTTPS 전달
// 헤더를 붙여도 거부되는지 확인한다. 헤더는 누구나 붙일 수 있으므로 이 검사가 없으면
// 앱 포트에 닿는 누구든 FR-AGENT_CONTEXT-140의 평문 거부를 지나간다.
func TestTransportSecurityRejectsForgedForwardedProto(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	request := secureRequest(http.MethodGet, gatedPath, nil)
	request.RemoteAddr = "203.0.113.9:54321"
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("신뢰 대역 밖 위조 헤더 상태 = %d, want 400", response.Code)
	}
}

// TestTransportSecurityRejectsAppendedForwardedProto는 신뢰된 프록시를 거친 요청이라도
// 전달 헤더가 여럿이면 거부하는지 확인한다. 프록시가 헤더를 덮어쓰지 않고 덧붙이면 원
// 요청자가 넣은 값이 앞에 남아 위조가 신뢰 대역 검사를 지나간다.
func TestTransportSecurityRejectsAppendedForwardedProto(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	request := httptest.NewRequest(http.MethodGet, gatedPath, nil)
	request.Header.Add("X-Forwarded-Proto", "https")
	request.Header.Add("X-Forwarded-Proto", "http")
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("덧붙인 전달 헤더 상태 = %d, want 400", response.Code)
	}
}

// TestTransportSecurityAcceptsTrustedProxy는 신뢰 대역 안 프록시가 전달한 HTTPS 요청만
// 통과하고 같은 프록시라도 전달 헤더가 없으면 평문으로 다루는지 확인한다.
func TestTransportSecurityAcceptsTrustedProxy(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)

	forwarded := httptest.NewRecorder()
	app.handler().ServeHTTP(forwarded, secureRequest(http.MethodGet, gatedPath, nil))
	if forwarded.Code != http.StatusNotFound {
		t.Fatalf("신뢰 프록시 요청 상태 = %d, want 404", forwarded.Code)
	}

	plain := httptest.NewRecorder()
	app.handler().ServeHTTP(plain, httptest.NewRequest(http.MethodGet, gatedPath, nil))
	if plain.Code != http.StatusBadRequest {
		t.Fatalf("전달 헤더 없는 신뢰 프록시 상태 = %d, want 400", plain.Code)
	}
}

// TestTransportSecurityMapsIPv4InIPv6는 IPv4-mapped IPv6로 들어온 프록시가 구성에 적은
// IPv4 대역과 맞는지 확인한다.
func TestTransportSecurityMapsIPv4InIPv6(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	request := secureRequest(http.MethodGet, gatedPath, nil)
	request.RemoteAddr = "[::ffff:192.0.2.1]:54321"
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("IPv4-mapped 신뢰 프록시 상태 = %d, want 404", response.Code)
	}
}

// TestTransportSecurityIgnoresForwardedProtoOnDirectTLS는 직접 TLS 종단에서 전달 헤더가
// 판정에 영향을 주지 않는지 확인한다.
func TestTransportSecurityIgnoresForwardedProtoOnDirectTLS(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true}, nil)
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, secureRequest(http.MethodGet, gatedPath, nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("직접 TLS의 전달 헤더 상태 = %d, want 400", response.Code)
	}
}

// TestTransportSecurityAcceptsDirectTLS는 직접 TLS 종단에서 실제 TLS 연결만 수락하는지 확인한다.
func TestTransportSecurityAcceptsDirectTLS(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true}, nil)
	request := httptest.NewRequest(http.MethodGet, gatedPath, nil)
	request.TLS = new(tls.ConnectionState{})
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("직접 TLS 요청 상태 = %d, want 404", response.Code)
	}
}

// TestRequestLogExcludesSensitiveValues는 구조화 로그가 경로를 남겨도 쿼리와 본문을 남기지 않는지 확인한다.
func TestRequestLogExcludesSensitiveValues(t *testing.T) {
	var output bytes.Buffer
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(&output, nil)), proxyTransport(), nil)
	request := httptest.NewRequest(http.MethodGet, "/healthz?access_token=secret-token", strings.NewReader("secret-body"))
	request.Header.Set("Authorization", "DPoP secret-access-token")
	request.Header.Set("DPoP", "secret-proof-with-jti")
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("healthz 상태 = %d, want 200", response.Code)
	}
	logged := output.String()
	for _, sensitive := range []string{"secret-token", "secret-body", "access_token", "secret-access-token", "secret-proof-with-jti"} {
		if strings.Contains(logged, sensitive) {
			t.Fatalf("구조화 로그에 민감한 값이 남았다: %q", sensitive)
		}
	}
}

// proxyTransport는 테스트용 신뢰된 역방향 프록시 종단 구성을 만든다. 대역은
// httptest.NewRequest가 기본으로 두는 RemoteAddr 192.0.2.1을 포함한다.
func proxyTransport() transportSecurity {
	return transportSecurity{trustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
}

// secureRequest는 신뢰된 프록시가 HTTPS 원 요청을 전달한 테스트 요청을 만든다.
func secureRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Header.Set("X-Forwarded-Proto", "https")
	return request
}

// fakeReadiness는 HTTP 상태 경로가 외부 데이터베이스 없이 검증할 수 있게 하는 대역이다.
type fakeReadiness struct {
	pingError error
	closed    bool
}

// Ping은 테스트가 지정한 준비 확인 결과를 돌려준다.
func (fake *fakeReadiness) Ping(context.Context) error {
	return fake.pingError
}

// Close는 종료가 연결 풀 정리를 요청했음을 표시한다.
func (fake *fakeReadiness) Close() {
	fake.closed = true
}

// TestAuthorizationServerPathsAreRegistered는 인가 서버 메타데이터가 알리는 세 경로가
// TLS 판정 뒤에 실제로 등록되어 있는지 확인한다. 등록되지 않으면 클라이언트가 접근
// 토큰을 받을 수 없어 `/mcp` 전체를 쓸 수 없다.
func TestAuthorizationServerPathsAreRegistered(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	app.authz = stubAuthorizationRoutes{}
	handler := app.handler()

	for _, testCase := range []struct{ method, path string }{
		{http.MethodGet, "/authorize"},
		{http.MethodPost, "/token"},
		{http.MethodGet, "/jwks.json"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(testCase.method, testCase.path, nil)
		request.Header.Set("X-Forwarded-Proto", "https")
		handler.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusNotFound {
			t.Fatalf("%s %s가 등록되지 않았다", testCase.method, testCase.path)
		}
	}
}

// stubAuthorizationRoutes는 경로가 등록됐는지만 보기 위해 본문 없이 응답한다.
type stubAuthorizationRoutes struct{}

func (stubAuthorizationRoutes) Authorize(writer http.ResponseWriter, _ *http.Request) {
	writer.WriteHeader(http.StatusSeeOther)
}

func (stubAuthorizationRoutes) Token(writer http.ResponseWriter, _ *http.Request) {
	writer.WriteHeader(http.StatusOK)
}

func (stubAuthorizationRoutes) JWKS(writer http.ResponseWriter, _ *http.Request) {
	writer.WriteHeader(http.StatusOK)
}

// TestHTTPServerSetsReadTimeouts는 수신 서버에 읽기·헤더·유휴 제한이 있는지 확인한다.
// 제한이 없으면 헤더를 느리게 보내는 연결을 대량으로 열어 고루틴과 파일 디스크립터를
// 고갈시킬 수 있고, 그 자리는 TLS 판정보다 앞이라 인증 없이 닿는다.
func TestHTTPServerSetsReadTimeouts(t *testing.T) {
	if readHeaderTimeout <= 0 || readTimeout <= 0 || idleTimeout <= 0 {
		t.Fatalf("타임아웃 = 헤더 %v, 읽기 %v, 유휴 %v; 모두 양수여야 한다", readHeaderTimeout, readTimeout, idleTimeout)
	}
	if readHeaderTimeout > readTimeout {
		t.Fatalf("헤더 제한 %v가 읽기 제한 %v보다 크다", readHeaderTimeout, readTimeout)
	}
	// 쓰기 제한은 두지 않는다. 흐름 검색이 외부 임베딩 제공자를 기다리는 동안 응답이
	// 끊기면 안 되므로, 값이 생기면 그 판단을 다시 해야 한다.
	server := newHTTPServer(":0", http.NewServeMux())
	if server.ReadHeaderTimeout != readHeaderTimeout || server.ReadTimeout != readTimeout || server.IdleTimeout != idleTimeout {
		t.Fatalf("서버 타임아웃 = %#v", server)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("쓰기 제한 = %v, want 0", server.WriteTimeout)
	}
}

// TestPanicBecomesInternalError는 처리기에서 빠져나온 panic이 연결을 끊지 않고 500으로
// 응답하는지 확인한다. 차단막이 없으면 클라이언트가 응답 대신 끊긴 연결을 받고 구조화
// 로그에도 요청이 남지 않는다.
func TestPanicBecomesInternalError(t *testing.T) {
	var logs bytes.Buffer
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(&logs, nil)), proxyTransport(), nil)
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("처리기 내부 오류")
	})
	recorder := httptest.NewRecorder()
	app.logRequests(app.recoverPanics(panicking)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/mcp", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("panic 응답 상태 = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(recorder.Body.String(), "internal_error") {
		t.Fatalf("panic 응답 본문 = %s", recorder.Body.String())
	}
	// 원인과 요청 흐름을 이을 상관 식별자가 로그에 남아야 한다.
	for _, want := range []string{"요청 처리 중 panic", "처리기 내부 오류", "correlation_id"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("로그에 %q가 없다: %s", want, logs.String())
		}
	}
}

// TestAbortHandlerPanicStaysUnhandled는 의도적인 연결 끊기 신호는 가로채지 않는지
// 확인한다. 가로채면 net/http이 그 신호로 하던 처리를 하지 못한다.
func TestAbortHandlerPanicStaysUnhandled(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	defer func() {
		if cause := recover(); cause != http.ErrAbortHandler {
			t.Fatalf("복구한 panic = %v, want http.ErrAbortHandler", cause)
		}
	}()
	aborting := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	app.recoverPanics(aborting).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp", nil))
	t.Fatal("ErrAbortHandler가 전달되지 않았다")
}
