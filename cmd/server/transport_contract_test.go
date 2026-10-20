package main

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"agent_context_sharing/internal/config"
)

func TestOPS03TLSGatePrecedesRouting(t *testing.T) {
	for _, path := range []string{"/login", "/register", "/graphs", "/jwks.json", "/mcp", "/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server", "/authorize", "/token", "/unknown", "/unknown//child", "/graphs/../login", "/./healthz", "//healthz", "/healthz/../readyz", "/healthz/"} {
		t.Run(path, func(t *testing.T) {
			app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("CF-Visitor", `{"scheme":"https"}`)
			request.Header.Set("Forwarded", "proto=https;for=192.0.2.1")
			request.Header.Set("CF-Connecting-IP", "192.0.2.1")
			app.handler().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" || len(response.Result().Cookies()) != 0 {
				t.Fatal("TLS 판정 전에 경로 정보·리디렉션·쿠키를 반환했습니다")
			}
		})
	}
}

func TestOPS03ForwardedProtocolGrammar(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []string
		valid  bool
	}{
		{"https", []string{"https"}, true},
		{"ascii case", []string{"hTtPs"}, true},
		{"ascii whitespace", []string{" \thttps\t "}, true},
		{"missing", nil, false},
		{"empty", []string{""}, false},
		{"http", []string{"http"}, false},
		{"list", []string{"https, http"}, false},
		{"identical list", []string{"https,https"}, false},
		{"duplicate", []string{"https", "https"}, false},
		{"appended", []string{"http", "https"}, false},
		{"unicode whitespace", []string{"\u00a0https\u00a0"}, false},
		{"unicode case fold", []string{"http\u017f"}, false},
		{"line break", []string{"https\n"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			security := proxyTransport()
			request := httptest.NewRequest(http.MethodGet, "/login", nil)
			request.TLS = &tls.ConnectionState{}
			request.Header["X-Forwarded-Proto"] = test.values
			request.Header.Set("CF-Connecting-IP", "198.51.100.1")
			if security.isTLSRequest(request) != test.valid {
				t.Fatal("단일 ASCII HTTPS 전달 헤더의 판정이 다릅니다")
			}
			want := netip.MustParseAddr("192.0.2.1")
			if test.valid {
				want = netip.MustParseAddr("198.51.100.1")
			}
			if security.clientAddress(request) != want {
				t.Fatal("전송 신뢰 판정과 방문자 주소 해석이 다릅니다")
			}
		})
	}
}

func TestOPS03RemotePeerGrammar(t *testing.T) {
	for _, test := range []struct{ name, peer, want string }{
		{"ipv4", "192.0.2.1:1234", "192.0.2.1"},
		{"ipv6", "[2001:db8::1]:1234", "2001:db8::1"},
		{"mapped", "[::ffff:192.0.2.1]:1234", "192.0.2.1"},
		{"zero port", "192.0.2.1:0", "192.0.2.1"},
		{"last port", "192.0.2.1:65535", "192.0.2.1"},
		{"missing port", "192.0.2.1", ""},
		{"empty port", "192.0.2.1:", ""},
		{"named port", "192.0.2.1:https", ""},
		{"large port", "192.0.2.1:65536", ""},
		{"negative port", "192.0.2.1:-1", ""},
		{"hostname", "proxy.test:1234", ""},
		{"zone", "[fe80::1%eth0]:1234", ""},
		{"bare ipv6", "2001:db8::1", ""},
		{"empty", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, valid := remoteAddr(test.peer)
			if test.want == "" {
				if valid || got.IsValid() {
					t.Fatal("잘못된 실제 상대 주소를 허용했습니다")
				}
				request := secureRequest(http.MethodGet, "/login", nil)
				request.RemoteAddr = test.peer
				request.Header.Set("CF-Connecting-IP", "192.0.2.1")
				if proxyTransport().isTLSRequest(request) || proxyTransport().clientAddress(request).IsValid() {
					t.Fatal("주소 해석 실패를 방문자 헤더로 신뢰했습니다")
				}
			} else if !valid || got != netip.MustParseAddr(test.want) {
				t.Fatal("실제 상대 주소의 정규화가 다릅니다")
			}
		})
	}
}

func TestOPS03ProbeExceptionsAreExact(t *testing.T) {
	for _, mode := range []string{"direct", "proxy"} {
		for _, path := range []string{"/healthz", "/readyz"} {
			for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions, http.MethodDelete} {
				t.Run(mode+" "+method+" "+path, func(t *testing.T) {
					security := proxyTransport()
					if mode == "direct" {
						security = transportSecurity{directTLS: true}
					}
					app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), security)
					request := httptest.NewRequest(method, path, nil)
					request.RemoteAddr = "203.0.113.1:1234"
					response := httptest.NewRecorder()
					app.handler().ServeHTTP(response, request)
					want := http.StatusBadRequest
					if method == http.MethodGet || method == http.MethodHead {
						want = http.StatusOK
					}
					if response.Code != want {
						t.Fatal("상태 확인의 메서드별 TLS 예외가 다릅니다")
					}
				})
			}
		}
	}
}

func TestOPS03VisitorAddressGrammar(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []string
		want   string
	}{
		{"ipv4", []string{"198.51.100.1"}, "198.51.100.1"},
		{"ipv6", []string{"2001:db8::1"}, "2001:db8::1"},
		{"mapped", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"ascii whitespace", []string{" \t198.51.100.1\t "}, "198.51.100.1"},
		{"missing", nil, "192.0.2.1"},
		{"empty", []string{""}, "192.0.2.1"},
		{"list", []string{"198.51.100.1, 198.51.100.2"}, "192.0.2.1"},
		{"duplicate", []string{"198.51.100.1", "198.51.100.1"}, "192.0.2.1"},
		{"zone", []string{"fe80::1%eth0"}, "192.0.2.1"},
		{"unicode whitespace", []string{"\u00a0198.51.100.1\u00a0"}, "192.0.2.1"},
		{"hostname", []string{"visitor.test"}, "192.0.2.1"},
		{"port", []string{"198.51.100.1:1234"}, "192.0.2.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := secureRequest(http.MethodPost, "/login", nil)
			request.Header["Cf-Connecting-Ip"] = test.values
			request.Header.Set("X-Forwarded-For", "203.0.113.1")
			request.Header.Set("X-Real-IP", "203.0.113.2")
			request.Header.Set("Forwarded", "for=203.0.113.3;proto=https")
			if got := proxyTransport().clientAddress(request); got != netip.MustParseAddr(test.want) {
				t.Fatal("잘못된 방문자 주소를 실제 상대 대신 사용했습니다")
			}
		})
	}
}

func TestOPS03TerminationConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, mode, proxies, cert, key string
		valid                          bool
	}{
		{"direct", "direct", "", "/synthetic/server.crt", "/synthetic/server.key", true},
		{"proxy", "proxy", "192.0.2.1/32", "", "", true},
		{"unknown mode", "automatic", "192.0.2.1/32", "", "", false},
		{"direct trusts proxy", "direct", "192.0.2.1/32", "/synthetic/server.crt", "/synthetic/server.key", false},
		{"direct missing cert", "direct", "", "", "/synthetic/server.key", false},
		{"direct missing key", "direct", "", "/synthetic/server.crt", "", false},
		{"direct missing pair", "direct", "", "", "", false},
		{"proxy no trust", "proxy", "", "", "", false},
		{"proxy has cert", "proxy", "192.0.2.1/32", "/synthetic/server.crt", "", false},
		{"proxy has key", "proxy", "192.0.2.1/32", "", "/synthetic/server.key", false},
		{"proxy has pair", "proxy", "192.0.2.1/32", "/synthetic/server.crt", "/synthetic/server.key", false},
		{"proxy malformed trust", "proxy", "proxy.test", "", "", false},
		{"proxy zoned trust", "proxy", "fe80::1%eth0", "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := serverConfiguration(t)
			values["TLS_TERMINATION"], values["TRUSTED_PROXY_CIDRS"] = test.mode, test.proxies
			values["TLS_CERT_FILE"], values["TLS_KEY_FILE"] = test.cert, test.key
			starts := 0
			err := configureAndRun(func(name string) string { return values[name] }, func(cfg config.Config) error {
				starts++
				if string(cfg.TLSMode) != test.mode || cfg.TLSCertFile != test.cert || cfg.TLSKeyFile != test.key {
					t.Fatal("검증된 TLS 구성이 조립 경계에서 바뀌었습니다")
				}
				return nil
			})
			if test.valid {
				if err != nil || starts != 1 {
					t.Fatal("정상 종단 구성이 조립되지 않았습니다")
				}
			} else if err == nil || starts != 0 {
				t.Fatal("잘못된 종단 구성이 자원 시작 전에 거부되지 않았습니다")
			}
		})
	}
}
