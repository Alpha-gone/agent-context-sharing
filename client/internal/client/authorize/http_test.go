package authorize

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

func TestWellKnownPreservesIssuerPath(t *testing.T) {
	for _, path := range []string{"", "/", "/tenant/", "/tenant%2Fid/", "/%74enant"} {
		raw, err := wellKnown("https://issuer.test"+path, "oauth-authorization-server")
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(raw)
		if err != nil || u.EscapedPath() != "/.well-known/oauth-authorization-server"+strings.TrimSuffix(path, "/") {
			t.Fatal("well-known 위치에서 issuer의 경로 인코딩을 바꿨습니다")
		}
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("secret-response-body") }
func (failingBody) Close() error             { return nil }

func TestAuthorizationHTTPFailureClassification(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want error
	}{
		{"discovery-network", contract.ErrTransport},
		{"discovery-body", contract.ErrTransport},
		{"discovery-status", contract.ErrProtocol},
		{"token-network", contract.ErrTransport},
		{"token-status", ErrAuthorization},
		{"token-redirect", contract.ErrProtocol},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFixture(t)
			m := f.manager("5s")
			failures := 0
			m.transport = roundTripper(func(req *http.Request) (*http.Response, error) {
				atToken := strings.HasPrefix(scenario.name, "token-")
				if atToken != (req.URL.Path == "/token") {
					return f.transport(req)
				}
				failures++
				if strings.HasSuffix(scenario.name, "network") {
					return nil, errors.New("secret-network-detail")
				}
				response, err := jsonResponse(req, map[string]any{"error_description": "secret-status-body"})
				if err != nil {
					return nil, err
				}
				switch {
				case strings.HasSuffix(scenario.name, "body"):
					response.Body = failingBody{}
				case strings.HasSuffix(scenario.name, "redirect"):
					response.StatusCode = http.StatusFound
					response.Header.Set("Location", issuerURL+"/token")
				default:
					response.StatusCode = http.StatusServiceUnavailable
				}
				return response, nil
			})
			if _, err := m.Credentials(t.Context(), Challenge{}); !errors.Is(err, scenario.want) || strings.Contains(err.Error(), "secret-") {
				t.Fatalf("HTTP 오류 분류나 비밀 비노출이 다릅니다: %v", err)
			}
			if failures != 1 || m.current.raw != "" {
				t.Fatal("인가 실패를 반복했거나 토큰을 저장했습니다")
			}
			f.assertClosed()
		})
	}
}

func TestDiscoveryRedirectPolicy(t *testing.T) {
	for _, scenario := range []string{"allowed", "downgrade", "userinfo", "fourth", "token"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			m := f.manager("5s")
			calls := 0
			m.transport = roundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("Authorization") != "" || req.Header.Get("DPoP") != "" || req.Header.Get("Cookie") != "" {
					t.Fatal("redirect에 비밀을 전달했습니다")
				}
				if scenario == "allowed" && calls == 2 {
					return jsonResponse(req, map[string]any{"ok": true})
				}
				target := "https://other.test/metadata"
				if scenario == "downgrade" {
					target = "http://other.test/metadata"
				}
				if scenario == "userinfo" {
					target = "https://user:secret@other.test/metadata"
				}
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})
			var out map[string]any
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://resource.test/metadata", nil)
			err := readJSON(t.Context(), m.httpClient(scenario != "token"), req, &out)
			if scenario == "allowed" {
				if err != nil || calls != 2 {
					t.Fatal(err)
				}
			} else if !errors.Is(err, contract.ErrProtocol) {
				t.Fatal("잘못된 redirect를 허용했습니다")
			}
			if scenario == "fourth" && calls != 4 {
				t.Fatalf("redirect 상한: %d", calls)
			}
			if scenario == "token" && calls != 1 {
				t.Fatal("토큰 교환 redirect를 따라갔습니다")
			}
		})
	}
}

func TestJSONAndResponseBounds(t *testing.T) {
	f := newFixture(t)
	m := f.manager("5s")
	for _, body := range []string{`null`, `[]`, `{"issuer":"a","issuer":"b"}`, `{"broken":`} {
		m.transport = roundTripper(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
		var out map[string]any
		if err := m.getJSON(t.Context(), "https://resource.test/metadata", &out); !errors.Is(err, contract.ErrProtocol) {
			t.Fatal("잘못된 JSON 응답을 허용했습니다")
		}
	}
	m.transport = roundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: responseLimit + 1, Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
	})
	var out map[string]any
	if err := m.getJSON(t.Context(), "https://resource.test/metadata", &out); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("크기 상한을 허용했습니다")
	}
}

func TestExpiredTokenStartsFreshAuthorization(t *testing.T) {
	f := newFixture(t)
	m := f.manager("5s")
	first, err := m.Credentials(t.Context(), Challenge{})
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	next, err := m.Credentials(t.Context(), Challenge{})
	if err != nil {
		t.Fatal(err)
	}
	if first.raw == next.raw || first.identity != next.identity || f.opens.Load() != 2 {
		t.Fatal("만료된 토큰이 재사용됐거나 주체가 바뀌었습니다")
	}
}

func TestIPv6FallbackAndWaiterLimit(t *testing.T) {
	f := newFixture(t)
	m := f.manager("5s")
	var networks []string
	m.listen = func(ctx context.Context, network, address string) (net.Listener, error) {
		networks = append(networks, network)
		if network == "tcp4" {
			return nil, errors.New("IPv4 사용 불가")
		}
		return (&net.ListenConfig{}).Listen(ctx, network, address)
	}
	if _, err := m.Credentials(t.Context(), Challenge{}); err != nil {
		t.Fatalf("IPv6 시험 환경 또는 인가 실패: %v", err)
	}
	if len(networks) != 2 || networks[1] != "tcp6" {
		t.Fatal("IPv6를 시도하지 않았습니다")
	}
	f2 := newFixture(t)
	opened := make(chan string, 1)
	f2.open = func(_ context.Context, target string) error { opened <- target; return nil }
	manager := f2.manager("5s")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 128)
	var wg sync.WaitGroup
	for range 128 {
		wg.Go(func() { _, err := manager.Credentials(ctx, Challenge{}); results <- err })
	}
	<-opened
	eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.flight != nil && manager.flight.waiters == 128
	})
	if _, err := manager.Credentials(t.Context(), Challenge{}); !errors.Is(err, ErrBusy) {
		t.Fatal("129번째 인증 대기자를 수락했습니다")
	}
	cancel()
	wg.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	f2.assertClosed()
}
