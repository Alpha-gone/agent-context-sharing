package authorize

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/contract"
)

const resourceURL = "https://resource.test/mcp"
const issuerURL = "https://issuer.test"

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	t              *testing.T
	signer         *ecdsa.PrivateKey
	now            time.Time
	mu             sync.Mutex
	query          url.Values
	addresses      []string
	proofs         []string
	opens          atomic.Int64
	exchanges      atomic.Int64
	subject        string
	mutate         func(string, map[string]any)
	open           func(context.Context, string) error
	callbackChecks bool
	responseType   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, signer: key, now: time.Unix(1_800_000_000, 0), subject: "account-a", responseType: "DPoP"}
}

func testConfig(t *testing.T, timeout string) config.Config {
	t.Helper()
	env := map[string]string{"AGENT_CONTEXT_CLIENT_REMOTE_URL": resourceURL, "AGENT_CONTEXT_CLIENT_ID": "client", "AGENT_CONTEXT_CLIENT_AGENT_ID": uuid.NewV7().String(), "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": timeout, "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "30s"}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (f *fixture) token(jkt, subject string, exp int64) string {
	f.t.Helper()
	raw, err := signJWT(f.signer, map[string]any{"alg": "ES256", "kid": "key", "typ": "JWT"}, map[string]any{"iss": issuerURL, "sub": subject, "aud": []string{resourceURL}, "exp": exp, "cnf": map[string]string{"jkt": jkt}, "jti": uuid.NewV7().String()})
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func jsonResponse(request *http.Request, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: request}, nil
}

func (f *fixture) transport(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" {
		f.t.Error("HTTPS 검증이 우회됐습니다")
	}
	if request.Method == http.MethodGet && (request.Header.Get("Authorization") != "" || request.Header.Get("DPoP") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Idempotency-Key") != "") {
		f.t.Error("discovery에 비밀 헤더가 실렸습니다")
	}
	var body map[string]any
	switch request.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
		body = map[string]any{"resource": resourceURL, "authorization_servers": []string{issuerURL}, "dpop_bound_access_tokens_required": true, "dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{"agent-context"}}
	case "/.well-known/oauth-authorization-server":
		body = map[string]any{"issuer": issuerURL, "authorization_endpoint": issuerURL + "/authorize", "token_endpoint": issuerURL + "/token", "jwks_uri": issuerURL + "/jwks.json", "code_challenge_methods_supported": []string{"S256"}, "dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{"agent-context"}, "token_endpoint_auth_methods_supported": []string{"none"}, "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "authorization_response_iss_parameter_supported": true}
	case "/jwks.json":
		key := publicJWK(f.signer)
		key.Kid = "key"
		body = map[string]any{"keys": []jwk{key}}
	case "/token":
		f.exchanges.Add(1)
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "" {
			f.t.Error("공개 클라이언트의 토큰 요청이 다릅니다")
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(raw))
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		query := f.query.Clone()
		f.proofs = append(f.proofs, request.Header.Get("DPoP"))
		f.mu.Unlock()
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "secret-code" || form.Get("client_id") != "client" || form.Get("redirect_uri") != query.Get("redirect_uri") || form.Get("resource") != resourceURL || tokenHash(form.Get("code_verifier")) != query.Get("code_challenge") || len(form.Get("code_verifier")) < 43 {
			f.t.Error("PKCE·코드·redirect·resource 결합이 다릅니다")
		}
		parts := strings.Split(request.Header.Get("DPoP"), ".")
		if len(parts) != 3 {
			return nil, ErrAuthorization
		}
		headerRaw, _ := encoding.DecodeString(parts[0])
		var header struct {
			JWK jwk `json:"jwk"`
		}
		if json.Unmarshal(headerRaw, &header) != nil {
			return nil, ErrAuthorization
		}
		body = map[string]any{"access_token": f.token(thumbprint(header.JWK), f.subject, f.now.Unix()+3600), "token_type": f.responseType, "expires_in": 3600, "scope": "agent-context"}
	default:
		return nil, ErrAuthorization
	}
	if f.mutate != nil {
		f.mutate(request.URL.Path, body)
	}
	return jsonResponse(request, body)
}

func callbackURL(raw string) string {
	u, _ := url.Parse(raw)
	q := u.Query()
	callback, _ := url.Parse(q.Get("redirect_uri"))
	callback.RawQuery = url.Values{"state": {q.Get("state")}, "iss": {issuerURL}, "code": {"secret-code"}}.Encode()
	return callback.String()
}

func callbackRequest(target, host string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	if host != "" {
		req.Host = host
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (f *fixture) opener(ctx context.Context, target string) error {
	f.opens.Add(1)
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	q := u.Query()
	if q.Get("scope") != "agent-context" || q.Get("resource") != resourceURL || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		f.t.Error("인가 요청 필드가 다릅니다")
	}
	f.mu.Lock()
	f.query = q.Clone()
	f.mu.Unlock()
	if f.open != nil {
		return f.open(ctx, target)
	}
	callback := callbackURL(target)
	if f.callbackChecks {
		for _, field := range []string{"state", "iss", "code"} {
			bad, _ := url.Parse(callback)
			query := bad.Query()
			query.Set(field, "wrong")
			if field == "code" {
				query.Add("state", query.Get("state"))
			}
			bad.RawQuery = query.Encode()
			if status, err := callbackRequest(bad.String(), ""); err != nil || status != 400 {
				f.t.Errorf("잘못된 %s callback을 허용했습니다: %d %v", field, status, err)
			}
		}
		for _, bad := range []struct{ target, host string }{{strings.Replace(callback, "/callback?", "/other?", 1), ""}, {callback, "localhost:123"}} {
			if status, err := callbackRequest(bad.target, bad.host); err != nil || status != 400 {
				f.t.Errorf("잘못된 callback 경계: %d %v", status, err)
			}
		}
	}
	status, err := callbackRequest(callback, "")
	if err != nil {
		return err
	}
	if status != 200 {
		return ErrAuthorization
	}
	if f.callbackChecks {
		if status, err := callbackRequest(callback, ""); err != nil || status != 410 {
			f.t.Errorf("중복 callback 상태: %d %v", status, err)
		}
	}
	return nil
}

func (f *fixture) manager(timeout string) *Manager {
	f.t.Helper()
	listen := func(ctx context.Context, network, address string) (net.Listener, error) {
		listener, err := (&net.ListenConfig{}).Listen(ctx, network, address)
		if err == nil {
			f.mu.Lock()
			f.addresses = append(f.addresses, listener.Addr().String())
			f.mu.Unlock()
		}
		return listener, err
	}
	m, err := New(testConfig(f.t, timeout), Options{Transport: roundTripper(f.transport), OpenBrowser: f.opener, Listen: listen, Now: func() time.Time { return f.now }})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(m.Close)
	return m
}

func (f *fixture) assertClosed() {
	f.t.Helper()
	f.mu.Lock()
	addresses := append([]string(nil), f.addresses...)
	f.mu.Unlock()
	for _, address := range addresses {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			connection.Close()
			f.t.Errorf("callback 포트가 남았습니다: %s", address)
		}
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("조건 대기 제한 시간을 넘겼습니다")
		case <-ticker.C:
		}
	}
}

func TestBrowserFlowAndCallbackValidation(t *testing.T) {
	f := newFixture(t)
	f.callbackChecks = true
	m := f.manager("5s")
	credential, err := m.Credentials(t.Context(), Challenge{})
	if err != nil {
		t.Fatal(err)
	}
	if credential.raw == "" || f.opens.Load() != 1 || f.exchanges.Load() != 1 {
		t.Fatal("인가 결과가 다릅니다")
	}
	identity, err := m.Identity(t.Context())
	if err != nil || identity == "" || identity == credential.raw || strings.Contains(identity, f.subject) {
		t.Fatal("주체 식별자가 안전하지 않습니다")
	}
	f.assertClosed()
	for _, value := range []any{credential, m} {
		printed := fmt.Sprintf("%v %+v %#v", value, value, value)
		for _, secret := range []string{credential.raw, m.jkt, "secret-code", f.query.Get("state"), f.query.Get("code_challenge")} {
			if strings.Contains(printed, secret) {
				t.Fatal("기본 표현에 비밀이 노출됐습니다")
			}
		}
	}
	if _, err := json.Marshal(credential); err == nil {
		t.Fatal("자격 증명 직렬화를 허용했습니다")
	}
	for _, bad := range []string{"http://resource.test/mcp", "https://other.test/mcp", "https://resource.test/mcp?x=1"} {
		req, _ := http.NewRequest(http.MethodPost, bad, nil)
		if _, err := m.Authenticate(t.Context(), req); !errors.Is(err, contract.ErrProtocol) || req.Header.Get("Authorization") != "" {
			t.Fatal("다른 리소스에 토큰을 보냈습니다")
		}
	}
	request, _ := http.NewRequest(http.MethodPost, resourceURL, nil)
	if _, err := m.Authenticate(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	first := request.Header.Get("DPoP")
	if _, err := m.Authenticate(t.Context(), request); err != nil || first == request.Header.Get("DPoP") {
		t.Fatal("proof가 재사용됐습니다")
	}
	m.Close()
	if m.key != nil || m.current.raw != "" || m.jkt != "" {
		t.Fatal("종료한 프로세스의 비밀 참조가 남았습니다")
	}
	if _, err := m.Identity(t.Context()); !errors.Is(err, ErrAuthorization) {
		t.Fatal("종료 후 인증을 허용했습니다")
	}
}

func TestSharedAuthorizationAndWaiterCancellation(t *testing.T) {
	f := newFixture(t)
	opened := make(chan string, 1)
	f.open = func(_ context.Context, target string) error { opened <- target; return nil }
	m := f.manager("5s")
	ctx, cancel := context.WithCancel(t.Context())
	cancelled := make(chan error, 1)
	go func() { _, err := m.Credentials(ctx, Challenge{}); cancelled <- err }()
	target := <-opened
	results := make(chan error, 100)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { _, err := m.Credentials(t.Context(), Challenge{}); results <- err })
	}
	eventually(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.flight != nil && m.flight.waiters == 101 })
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if status, err := callbackRequest(callbackURL(target), ""); err != nil || status != 200 {
		t.Fatalf("%d %v", status, err)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.opens.Load() != 1 || f.exchanges.Load() != 1 {
		t.Fatal("공유 인가가 여러 번 실행됐습니다")
	}
	f.assertClosed()
}

func TestSharedAuthorizationDenial(t *testing.T) {
	f := newFixture(t)
	opened := make(chan string, 1)
	f.open = func(_ context.Context, target string) error { opened <- target; return nil }
	m := f.manager("5s")
	results := make(chan error, 100)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { _, err := m.Credentials(t.Context(), Challenge{}); results <- err })
	}
	target := <-opened
	eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.flight != nil && m.flight.waiters == 100
	})
	u, _ := url.Parse(callbackURL(target))
	q := u.Query()
	q.Del("code")
	q.Set("error", "access_denied")
	q.Set("error_description", "secret-denial")
	u.RawQuery = q.Encode()
	if status, err := callbackRequest(u.String(), ""); err != nil || status != http.StatusOK {
		t.Fatalf("거부 callback 처리 실패: %d %v", status, err)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, ErrAuthorization) || strings.Contains(err.Error(), "secret-denial") {
			t.Fatalf("거부가 모든 대기자에게 안전하게 전달되지 않았습니다: %v", err)
		}
	}
	if f.opens.Load() != 1 || f.exchanges.Load() != 0 || m.flight != nil || m.current.raw != "" {
		t.Fatal("거부 뒤 인가를 반복하거나 상태를 남겼습니다")
	}
	f.assertClosed()
}

func TestLastWaiterAndCloseCleanUp(t *testing.T) {
	for _, action := range []string{"cancel", "timeout", "opener", "denied", "close"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			opened := make(chan string, 1)
			f.open = func(ctx context.Context, target string) error {
				opened <- target
				if action == "opener" {
					return errors.New("비밀 URL: " + target)
				}
				if action == "denied" {
					u, _ := url.Parse(callbackURL(target))
					q := u.Query()
					q.Del("code")
					q.Set("error", "access_denied")
					q.Set("error_description", "secret-denial")
					u.RawQuery = q.Encode()
					_, err := callbackRequest(u.String(), "")
					return err
				}
				return nil
			}
			timeout := "5s"
			if action == "timeout" {
				timeout = "100ms"
			}
			m := f.manager(timeout)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := m.Credentials(ctx, Challenge{}); result <- err }()
			<-opened
			if action == "cancel" {
				cancel()
			}
			if action == "close" {
				m.Close()
			}
			err := <-result
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "http") {
				t.Fatalf("정리 오류 또는 비밀 노출: %v", err)
			}
			if action == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if action == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			f.assertClosed()
			if f.exchanges.Load() != 0 {
				t.Fatal("실패한 callback에서 코드를 교환했습니다")
			}
		})
	}
}

func TestRefreshAndLateUnauthorized(t *testing.T) {
	f := newFixture(t)
	m := f.manager("5s")
	old, err := m.Credentials(t.Context(), Challenge{})
	if err != nil {
		t.Fatal(err)
	}
	fresh := f.token(m.jkt, f.subject, f.now.Unix()+7200)
	headers := http.Header{"Mcp-Access-Token": {fresh}, "Mcp-Access-Token-Expires-At": {fmt.Sprint(f.now.Unix() + 7200)}}
	if err := m.Refresh(t.Context(), headers); err != nil {
		t.Fatal(err)
	}
	current, err := m.Reauthorize(t.Context(), old, Challenge{})
	if err != nil || current.raw != fresh || current.identity != old.identity || f.opens.Load() != 1 {
		t.Fatal("늦은401이 새 토큰을 지웠습니다")
	}
	for _, bad := range []http.Header{{"Mcp-Access-Token": {fresh}}, {"Mcp-Access-Token-Expires-At": {"1"}}, {"Mcp-Access-Token": {fresh, fresh}, "Mcp-Access-Token-Expires-At": {fmt.Sprint(f.now.Unix() + 7200)}}, {"Mcp-Access-Token": {fresh}, "Mcp-Access-Token-Expires-At": {"1"}}, {"Mcp-Access-Token": {f.token("other", f.subject, f.now.Unix()+7200)}, "Mcp-Access-Token-Expires-At": {fmt.Sprint(f.now.Unix() + 7200)}}} {
		if err := m.Refresh(t.Context(), bad); !errors.Is(err, contract.ErrProtocol) {
			t.Fatal(err)
		}
		if m.current.raw != fresh {
			t.Fatal("잘못된 갱신이 현재 토큰을 바꿨습니다")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Refresh(ctx, headers); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.subject = "account-b"
	if _, err := m.Reauthorize(t.Context(), current, Challenge{}); !errors.Is(err, contract.ErrIdentityChanged) {
		t.Fatalf("프로세스 내 계정 전환을 허용했습니다: %v", err)
	}
	if m.current.raw != "" || m.boundIdentity != old.identity {
		t.Fatal("다른 계정이 저장됐습니다")
	}
	f.subject = "account-a"
	next, err := m.Credentials(t.Context(), Challenge{})
	if err != nil || next.identity != old.identity {
		t.Fatal("같은 계정으로의 복구가 실패했습니다")
	}
}

func TestMetadataContract(t *testing.T) {
	for _, field := range []string{"resource", "authorization_servers", "dpop_bound_access_tokens_required", "dpop_signing_alg_values_supported", "issuer", "authorization_endpoint", "token_endpoint", "jwks_uri", "code_challenge_methods_supported", "scopes_supported", "token_endpoint_auth_methods_supported", "authorization_response_iss_parameter_supported"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			f.mutate = func(_ string, body map[string]any) {
				if _, exists := body[field]; exists {
					delete(body, field)
				}
			}
			m := f.manager("5s")
			if _, err := m.Credentials(t.Context(), Challenge{}); !errors.Is(err, contract.ErrProtocol) {
				t.Fatalf("%s 검증이 누락됐습니다: %v", field, err)
			}
			if f.opens.Load() != 0 {
				t.Fatal("잘못된 메타데이터 뒤 브라우저를 실행했습니다")
			}
		})
	}
	f := newFixture(t)
	f.responseType = "Bearer"
	m := f.manager("5s")
	if _, err := m.Credentials(t.Context(), Challenge{}); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("Bearer 하향을 허용했습니다")
	}
}

func TestChallengeClassification(t *testing.T) {
	for _, tc := range []struct {
		header                       string
		authenticated, retried, want bool
		err                          error
	}{{`DPoP algs="ES256", scope="agent-context", resource_metadata="https://resource.test/.well-known/oauth-protected-resource"`, false, false, true, nil}, {`Basic realm="x", DPoP error="invalid_token", algs="ES256"`, true, false, true, nil}, {`DPoP error="invalid_token"`, true, true, false, ErrAuthorization}, {`DPoP error="invalid_dpop_proof"`, true, false, false, contract.ErrProtocol}, {`DPoP error="use_dpop_nonce"`, true, false, false, contract.ErrProtocol}, {`Bearer error="invalid_token"`, true, false, false, contract.ErrProtocol}, {`DPoP error="invalid_token", error="invalid_token"`, true, false, false, contract.ErrProtocol}, {`DPoP resource_metadata="http://bad.test"`, false, false, false, contract.ErrProtocol}, {`DPoP scope="other"`, false, false, false, contract.ErrProtocol}, {`DPoP algs="RS256"`, false, false, false, contract.ErrProtocol}} {
		_, retry, err := ParseChallenge(401, http.Header{"Www-Authenticate": {tc.header}}, tc.authenticated, tc.retried)
		if retry != tc.want || !errors.Is(err, tc.err) {
			t.Fatalf("도전 분류: %+v, %v", tc, err)
		}
	}
	if _, retry, err := ParseChallenge(500, http.Header{"Www-Authenticate": {`DPoP error="invalid_token"`}}, true, false); retry || err != nil {
		t.Fatal("500을 재인가했습니다")
	}
}
