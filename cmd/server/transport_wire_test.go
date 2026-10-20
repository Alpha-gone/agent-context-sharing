package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/web"
)

func TestOPS03WireTransportBoundary(t *testing.T) {
	for _, mode := range []string{"direct", "proxy", "untrusted proxy"} {
		t.Run(mode, func(t *testing.T) {
			security := transportSecurity{trustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
			if mode == "direct" {
				security = transportSecurity{directTLS: true}
			} else if mode == "untrusted proxy" {
				security.trustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
			}
			app, auth, keys := newTransportContractApplication(t, security)
			server := httptest.NewUnstartedServer(app.handler())
			server.Config = newHTTPServer("", app.handler())
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			if mode == "direct" {
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			client := server.Client()
			client.Timeout = 5 * time.Second
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			t.Cleanup(client.CloseIdleConnections)
			for _, test := range []struct {
				name, method, path        string
				proto                     []string
				proxyStatus, directStatus int
			}{
				{"health", "GET", "/healthz", nil, 200, 200},
				{"ready", "GET", "/readyz", nil, 200, 200},
				{"head probe", "HEAD", "/healthz", nil, 200, 200},
				{"post probe", "POST", "/healthz", nil, 400, 404},
				{"public login", "GET", "/login", nil, 400, 200},
				{"plain login", "POST", "/login", nil, 400, 303},
				{"trusted login", "POST", "/login", []string{"hTtPs"}, 303, 303},
				{"trusted registration", "POST", "/register", []string{"https"}, 303, 303},
				{"duplicate proto", "POST", "/login", []string{"https", "https"}, 400, 303},
				{"appended proto", "POST", "/login", []string{"http", "https"}, 400, 303},
				{"list proto", "POST", "/login", []string{"https,http"}, 400, 303},
				{"unicode proto", "POST", "/login", []string{"http\u017f"}, 400, 303},
				{"unicode whitespace", "POST", "/login", []string{"\u00a0https\u00a0"}, 400, 303},
				{"public keys", "GET", "/jwks.json", []string{"https"}, 200, 200},
				{"plain public keys", "GET", "/jwks.json", nil, 400, 200},
				{"protected web", "GET", "/graphs", []string{"https"}, 303, 303},
				{"mcp boundary", "POST", "/mcp", []string{"https"}, 404, 404},
				{"metadata boundary", "GET", "/.well-known/oauth-authorization-server", []string{"https"}, 404, 404},
				{"unknown boundary", "GET", "/unknown", nil, 400, 404},
				{"path canonicalization", "GET", "/unknown//child", nil, 400, 307},
			} {
				t.Run(test.name, func(t *testing.T) {
					beforeAuth, beforeKeys := auth.calls.Load(), keys.calls.Load()
					request, err := http.NewRequestWithContext(t.Context(), test.method, server.URL+test.path, strings.NewReader("login_id=tester&password=synthetic-password"))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					request.Header["X-Forwarded-Proto"] = test.proto
					request.Header.Set("CF-Visitor", `{"scheme":"https"}`)
					request.Header.Set("CF-Connecting-IP", "198.51.100.1")
					request.Header.Set("X-Forwarded-For", "192.0.2.1")
					request.Header.Set("Forwarded", "for=192.0.2.1;proto=https")
					response, err := client.Do(request)
					if err != nil {
						t.Fatal("루프백 전송 시험 요청에 실패했습니다")
					}
					defer response.Body.Close()
					_, _ = io.Copy(io.Discard, response.Body)
					want := test.proxyStatus
					if mode == "direct" {
						want = test.directStatus
					} else if mode == "untrusted proxy" && test.path != "/healthz" && test.path != "/readyz" {
						want = http.StatusBadRequest
					}
					if response.StatusCode != want {
						t.Fatalf("전송 경계 상태 = %d, 기대 %d", response.StatusCode, want)
					}
					login := test.method == "POST" && test.path == "/login" && want == 303
					registered := test.method == "POST" && test.path == "/register" && want == 303
					if login || registered {
						if auth.calls.Load() != beforeAuth+1 {
							t.Fatal("승인된 전송 뒤 자격 처리에 연결되지 않았습니다")
						}
						peer := "127.0.0.1"
						if mode == "proxy" {
							peer = "198.51.100.1"
						}
						if auth.peer.Load() != peer {
							t.Fatal("실제 연결과 방문자 주소의 신뢰 경계가 다릅니다")
						}
					} else if auth.calls.Load() != beforeAuth {
						t.Fatal("거부·비인증 경로에서 자격 처리를 실행했습니다")
					}
					if test.path == "/jwks.json" && want == 200 {
						if keys.calls.Load() != beforeKeys+1 {
							t.Fatal("허용된 공개 키 경로에 연결되지 않았습니다")
						}
					} else if keys.calls.Load() != beforeKeys {
						t.Fatal("전송 판정 전에 공개 키 경로를 실행했습니다")
					}
					cookies := response.Cookies()
					if login {
						if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != "/" {
							t.Fatal("전송 판정과 세션 쿠키 속성이 다릅니다")
						}
					} else if len(cookies) != 0 {
						t.Fatal("로그인 성공 외의 경로에서 세션 쿠키를 발급했습니다")
					}
				})
			}
			if mode == "direct" {
				response, err := client.Get(strings.Replace(server.URL, "https://", "http://", 1) + "/healthz")
				if err != nil {
					t.Fatal("직접 TLS 수신기의 평문 거부 응답을 확인하지 못했습니다")
				}
				defer response.Body.Close()
				_, _ = io.Copy(io.Discard, response.Body)
				if response.StatusCode != http.StatusBadRequest {
					t.Fatal("직접 TLS 수신기에 평문 프로브가 접속했습니다")
				}
			}
		})
	}
}

func TestOPS03RejectedTransportDoesNotReadForm(t *testing.T) {
	app, auth, _ := newTransportContractApplication(t, proxyTransport())
	body := &transportObservedBody{}
	request := httptest.NewRequest(http.MethodPost, "/login", body)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || body.reads != 0 || auth.calls.Load() != 0 {
		t.Fatal("TLS 거부 전에 양식 또는 자격을 처리했습니다")
	}
}

type transportObservedBody struct{ reads int }

func (body *transportObservedBody) Read([]byte) (int, error) { body.reads++; return 0, io.EOF }

type transportAuthentication struct {
	web.Authentication
	id    model.ID
	calls atomic.Int32
	peer  atomic.Value
}

func (auth *transportAuthentication) Authenticate(context.Context, string, string) (model.ID, error) {
	auth.calls.Add(1)
	return auth.id, nil
}

func (auth *transportAuthentication) Register(ctx context.Context, login, password string) (model.ID, error) {
	return auth.Authenticate(ctx, login, password)
}

func (*transportAuthentication) WebSession(context.Context, model.ID, string) (web.Session, error) {
	return web.Session{Raw: "synthetic-session", ID: "synthetic-id", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type transportPublicKeys struct{ calls atomic.Int32 }

func (keys *transportPublicKeys) JWKS(writer http.ResponseWriter, _ *http.Request) {
	keys.calls.Add(1)
	writer.WriteHeader(http.StatusOK)
}

func newTransportContractApplication(t *testing.T, security transportSecurity) (*application, *transportAuthentication, *transportPublicKeys) {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	auth := &transportAuthentication{id: id}
	auth.peer.Store("")
	server, err := web.New(auth, &struct{ web.GraphStore }{}, web.Config{SecureCookie: security.isTLSRequest, ClientAddress: func(request *http.Request) netip.Addr {
		peer := security.clientAddress(request)
		auth.peer.Store(peer.String())
		return peer
	}})
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), security)
	keys := &transportPublicKeys{}
	app.web, app.publicKeys = server, keys
	return app, auth, keys
}
