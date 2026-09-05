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

// TestHealthAndReadySeparateFailures는 생존 확인과 데이터베이스 준비 확인의 실패를 구분한다.
func TestHealthAndReadySeparateFailures(t *testing.T) {
	database := &fakeReadiness{pingError: errors.New("데이터베이스 중단")}
	app := newApplication(database, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
	handler := app.handler()

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, secureRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("healthz 상태 = %d, want 200", health.Code)
	}

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, secureRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz 상태 = %d, want 503", ready.Code)
	}
	if ready.Header().Get("X-Correlation-ID") == "" {
		t.Fatal("상관 식별자 헤더가 없다")
	}
}

// TestShutdownStopsReadinessAndClosesDatabase는 종료가 준비 상태를 먼저 내리고 풀을 닫는지 확인한다.
func TestShutdownStopsReadinessAndClosesDatabase(t *testing.T) {
	database := &fakeReadiness{}
	app := newApplication(database, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
	server := &http.Server{Handler: app.handler()}
	if err := app.shutdown(t.Context(), server); err != nil {
		t.Fatalf("정상 종료: %v", err)
	}
	if !database.closed {
		t.Fatal("데이터베이스 풀이 닫히지 않았다")
	}
	ready := httptest.NewRecorder()
	app.handler().ServeHTTP(ready, secureRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("종료 뒤 readyz 상태 = %d, want 503", ready.Code)
	}
}

// TestTransportSecurityRejectsPlaintext는 프록시 종단에서 신뢰된 HTTPS 전달 헤더가 없는 요청을 거부하는지 확인한다.
func TestTransportSecurityRejectsPlaintext(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("평문 healthz 상태 = %d, want 400", response.Code)
	}
}

// TestTransportSecurityRejectsForgedForwardedProto는 신뢰 대역 밖에서 온 요청이 HTTPS 전달
// 헤더를 붙여도 거부되는지 확인한다. 헤더는 누구나 붙일 수 있으므로 이 검사가 없으면
// 앱 포트에 닿는 누구든 FR-AGENT_CONTEXT-140의 평문 거부를 지나간다.
func TestTransportSecurityRejectsForgedForwardedProto(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
	request := secureRequest(http.MethodGet, "/healthz", nil)
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
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
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
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())

	forwarded := httptest.NewRecorder()
	app.handler().ServeHTTP(forwarded, secureRequest(http.MethodGet, "/healthz", nil))
	if forwarded.Code != http.StatusOK {
		t.Fatalf("신뢰 프록시 healthz 상태 = %d, want 200", forwarded.Code)
	}

	plain := httptest.NewRecorder()
	app.handler().ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if plain.Code != http.StatusBadRequest {
		t.Fatalf("전달 헤더 없는 신뢰 프록시 상태 = %d, want 400", plain.Code)
	}
}

// TestTransportSecurityMapsIPv4InIPv6는 IPv4-mapped IPv6로 들어온 프록시가 구성에 적은
// IPv4 대역과 맞는지 확인한다.
func TestTransportSecurityMapsIPv4InIPv6(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
	request := secureRequest(http.MethodGet, "/healthz", nil)
	request.RemoteAddr = "[::ffff:192.0.2.1]:54321"
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("IPv4-mapped 신뢰 프록시 상태 = %d, want 200", response.Code)
	}
}

// TestTransportSecurityIgnoresForwardedProtoOnDirectTLS는 직접 TLS 종단에서 전달 헤더가
// 판정에 영향을 주지 않는지 확인한다.
func TestTransportSecurityIgnoresForwardedProtoOnDirectTLS(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true})
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, secureRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("직접 TLS의 전달 헤더 상태 = %d, want 400", response.Code)
	}
}

// TestTransportSecurityAcceptsDirectTLS는 직접 TLS 종단에서 실제 TLS 연결만 수락하는지 확인한다.
func TestTransportSecurityAcceptsDirectTLS(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true})
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.TLS = new(tls.ConnectionState{})
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("직접 TLS healthz 상태 = %d, want 200", response.Code)
	}
}

// TestRequestLogExcludesSensitiveValues는 구조화 로그가 경로를 남겨도 쿼리와 본문을 남기지 않는지 확인한다.
func TestRequestLogExcludesSensitiveValues(t *testing.T) {
	var output bytes.Buffer
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(&output, nil)), proxyTransport())
	request := secureRequest(http.MethodGet, "/healthz?access_token=secret-token", strings.NewReader("secret-body"))
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("healthz 상태 = %d, want 200", response.Code)
	}
	logged := output.String()
	for _, sensitive := range []string{"secret-token", "secret-body", "access_token"} {
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
