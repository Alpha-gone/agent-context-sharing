package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
)

func TestOAuthURIConfigurationRejectsBeforeStartup(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"AUTHORIZATION_SERVER_URL", "http://issuer.test"},
		{"AUTHORIZATION_SERVER_URL", "https://issuer.test/"},
		{"AUTHORIZATION_SERVER_URL", "https://issuer.test?"},
		{"AUTHORIZATION_SERVER_URL", "https://issuer.test#"},
		{"AUTHORIZATION_SERVER_URL", "https://user@issuer.test"},
		{"AUTHORIZATION_SERVER_URL", "https://issuer.test:0"},
		{"AUTHORIZATION_SERVER_URL", "https://[::1%25lo0]"},
		{"MCP_RESOURCE_URL", "http://resource.test/mcp"},
		{"MCP_RESOURCE_URL", "https://resource.test/%6dcp"},
		{"MCP_RESOURCE_URL", "https://resource.test/mcp/"},
		{"MCP_RESOURCE_URL", "https://resource.test/mcp?"},
		{"MCP_RESOURCE_URL", "https://resource.test/mcp#"},
		{"MCP_RESOURCE_URL", "https://resource.test:65536/mcp"},
		{"MCP_RESOURCE_URL", "https://user@resource.test/mcp"},
		{"MCP_ALLOWED_ORIGINS", "http://resource.test"},
		{"MCP_ALLOWED_ORIGINS", "https://resource.test/"},
		{"MCP_ALLOWED_ORIGINS", "https://resource.test?x=1"},
		{"MCP_ALLOWED_ORIGINS", "https://resource.test#fragment"},
		{"MCP_ALLOWED_ORIGINS", "https://user@resource.test"},
		{"MCP_ALLOWED_ORIGINS", "https://*.test"},
		{"MCP_ALLOWED_ORIGINS", "https://resource.test,https://resource.test"},
		{"MCP_ALLOWED_ORIGINS", "https://resource.test,"},
	} {
		t.Run(test.key+"/"+test.value, func(t *testing.T) {
			values := serverConfiguration(t)
			values[test.key] = test.value
			started := false
			err := configureAndRun(func(key string) string { return values[key] }, func(config.Config) error { started = true; return nil })
			if err == nil || started {
				t.Fatal("잘못된 OAuth URI가 자원 시작 전에 차단되지 않았습니다")
			}
		})
	}
}

func TestOAuthDiscoveryHTTPIntegration(t *testing.T) {
	values := serverConfiguration(t)
	values["AUTHORIZATION_SERVER_URL"] = "https://issuer.test"
	values["MCP_RESOURCE_URL"] = "https://resource.test/mcp"
	values["MCP_ALLOWED_ORIGINS"] = "https://resource.test"
	values["OAUTH_CLIENT_REGISTRATIONS"] = `{"codex-cli-first-test":{"redirect_uris":["http://127.0.0.1:49152/callback"],"resources":["https://resource.test/mcp"],"loopback_port_policy":"exact"},"native":{"redirect_uris":["http://127.0.0.1:4000/callback"],"resources":["https://resource.test/mcp"],"loopback_port_policy":"same_ip"}}`
	cfg, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal("시험용 OAuth 구성 해석 실패")
	}
	backend := newPublicAuthenticationStore(t)
	service, err := authz.New(backend, authz.Config{Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: cfg.BcryptCost, OAuth: oauthConfiguration(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, client, callback string
		valid                  bool
	}{
		{"first exact", "codex-cli-first-test", "http://127.0.0.1:49152/callback", true},
		{"first changed port", "codex-cli-first-test", "http://127.0.0.1:50000/callback", false},
		{"general changed port", "native", "http://127.0.0.1:50000/callback", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := service.ValidateAuthorizationTarget(test.client, test.callback, cfg.MCPResourceURL.String())
			if (err == nil) != test.valid {
				t.Fatal("구성→인증 주입이 callback 포트 정책을 바꿨습니다")
			}
		})
	}
	handler, err := authz.NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	backend.Close() // 공개 발견 요청은 DB가 닫혀 있어도 처리할 수 있어야 한다.
	app := newApplication(backend, nil, transportSecurity{directTLS: true})
	app.oauthMetadata = handler
	server := httptest.NewTLSServer(app.handler())
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(client.CloseIdleConnections)
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions} {
			t.Run(method+" "+path, func(t *testing.T) {
				request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path+"?resource=https://attacker.test/mcp", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Host = "attacker.test"
				request.Header.Set("X-Forwarded-Host", "attacker.test")
				request.Header.Set("X-Forwarded-Proto", "http")
				request.Header.Set("Origin", "https://attacker.test")
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("메타데이터 TLS 요청 실패")
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal("메타데이터 응답 읽기 실패")
				}
				if response.Header.Get("Location") != "" || len(response.Cookies()) != 0 || response.Header.Get("Access-Control-Allow-Origin") != "" || response.Header.Get("Cache-Control") != "no-store" {
					t.Fatal("발견 경로가 리디렉션·세션·임의 CORS 허용을 만들었습니다")
				}
				if method == http.MethodPost || method == http.MethodOptions {
					if response.StatusCode != 405 || response.Header.Get("Allow") != "GET, HEAD" {
						t.Fatal("메타데이터 메서드 경계 불일치")
					}
					return
				}
				if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
					t.Fatal("공개 메타데이터 응답 형식 불일치")
				}
				if method == http.MethodHead {
					if len(body) != 0 {
						t.Fatal("HEAD에서 본문 전송")
					}
					return
				}
				if strings.Contains(string(body), "attacker.test") || strings.Contains(string(body), "codex-cli-first-test") || strings.Contains(string(body), "redirect_uris") || strings.Contains(string(body), "registration_endpoint") {
					t.Fatal("요청 값·등록 목록·자동 등록 endpoint 노출")
				}
				var document map[string]any
				if json.Unmarshal(body, &document) != nil {
					t.Fatal("발견 JSON 해석 실패")
				}
				if strings.Contains(path, "protected-resource") {
					if document["resource"] != "https://resource.test/mcp" {
						t.Fatal("고정 resource 변경")
					}
				} else if document["issuer"] != "https://issuer.test" || document["token_endpoint"] != "https://issuer.test/token" {
					t.Fatal("고정 issuer·endpoint 변경")
				}
			})
		}
		t.Run("TLS gate "+path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://attacker.test"+path, nil)
			request.Header.Set("X-Forwarded-Proto", "https")
			response := httptest.NewRecorder()
			app.handler().ServeHTTP(response, request)
			if response.Code != 400 || response.Header().Get("Location") != "" {
				t.Fatal("미검증 평문에서 메타데이터 공개")
			}
		})
	}
}
