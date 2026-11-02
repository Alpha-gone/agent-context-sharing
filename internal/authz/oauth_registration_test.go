package authz

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/store"
)

func TestOAuthDynamicRegistrationAdvertised(t *testing.T) {
	service, err := New(newMemoryStore(), oauthTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.AuthorizationServerMetadata(response, httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil))
	var metadata map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["registration_endpoint"] != "https://issuer.test/oauth/register" {
		t.Fatalf("DCR endpoint = %v", metadata["registration_endpoint"])
	}
}

func newRegistrationTestService(t *testing.T) *Service {
	t.Helper()
	cfg := oauthTestConfig()
	cfg.OAuth.Clients = testClients{}
	service, err := New(newMemoryStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func registrationRequest(body string) *http.Request {
	request := httptest.NewRequest("POST", "https://issuer.test/oauth/register", strings.NewReader(body))
	request.RemoteAddr = "192.0.2.1:50000"
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestDCRRegistersAndBindsIssuedClient(t *testing.T) {
	service := newRegistrationTestService(t)
	handler := NewRegistrationHandler(service, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, registrationRequest(`{"redirect_uris":["http://127.0.0.1:49152/callback"],"client_name":"Codex","client_id":"attacker-chosen-id","logo_uri":"http://127.0.0.1/internal"}`))
	if response.Code != 201 {
		t.Fatalf("registration = %d %s", response.Code, response.Body.String())
	}
	var client RegisteredClient
	if err := json.Unmarshal(response.Body.Bytes(), &client); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(client.ClientID, "dcr_") || client.ClientID == "attacker-chosen-id" || client.TokenEndpointAuthMethod != "none" || client.Scope != "agent-context" || client.ClientIDIssuedAt == 0 {
		t.Fatalf("registered metadata = %+v", client)
	}
	if strings.Contains(response.Body.String(), "client_secret") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("공개 등록 응답 계약 위반")
	}
	for _, target := range []struct {
		id, callback, resource string
		ok                     bool
	}{
		{client.ClientID, "http://127.0.0.1:49152/callback", "https://resource.test/mcp", true},
		{client.ClientID, "http://127.0.0.1:60000/callback", "https://resource.test/mcp", true},
		{client.ClientID, "http://127.0.0.1:49152/other", "https://resource.test/mcp", false},
		{client.ClientID, "http://localhost:49152/callback", "https://resource.test/mcp", false},
		{client.ClientID, "http://127.0.0.1:49152/callback", "https://other.test/mcp", false},
		{"attacker-chosen-id", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", false},
	} {
		if err := service.ValidateAuthorizationTarget(t.Context(), target.id, target.callback, target.resource); (err == nil) != target.ok {
			t.Errorf("target %+v: %v", target, err)
		}
	}
	second, err := New(newMemoryStore(), service.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.ValidateAuthorizationTarget(t.Context(), client.ClientID, client.RedirectURIs[0], "https://resource.test/mcp"); err != nil {
		t.Fatal("새 서비스가 등록을 재사용하지 못했습니다", err)
	}
}

func TestDCRRejectsInvalidMetadataAndCallbacks(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `{}`, `{} {}`, `{"redirect_uris":null}`, `{"redirect_uris":[null]}`,
		`{"redirect_uris":["https://client.test/callback"],"redirect_uris":["https://other.test/callback"]}`,
		`{"redirect_uris":["https://client.test/callback"],"client_secret":""}`,
		`{"redirect_uris":["https://client.test/callback"],"client_name":null}`,
		`{"redirect_uris":["https://client.test/callback"],"grant_types":null}`,
		`{"redirect_uris":["https://client.test/callback"],"grant_types":[]}`,
		`{"redirect_uris":["https://client.test/callback"],"grant_types":["client_credentials"]}`,
		`{"redirect_uris":["https://client.test/callback"],"response_types":["token"]}`,
		`{"redirect_uris":["https://client.test/callback"],"token_endpoint_auth_method":"client_secret_basic"}`,
		`{"redirect_uris":["https://client.test/callback"],"scope":"agent-context admin"}`,
		`{"redirect_uris":["https://client.test/callback"],"software_statement":"unverified"}`,
	} {
		t.Run(body, func(t *testing.T) {
			response := httptest.NewRecorder()
			NewRegistrationHandler(newRegistrationTestService(t), nil).ServeHTTP(response, registrationRequest(body))
			if response.Code != 400 {
				t.Errorf("status=%d", response.Code)
			}
		})
	}
	for _, callback := range []string{"", "/callback", "http://client.test:49152/callback", "http://127.0.0.1/callback", "http://127.0.0.1:0/callback", "http://127.0.0.1:65536/callback", "http://[::ffff:127.0.0.1]:49152/callback", "http://[::1%25lo0]:49152/callback", "https://user@client.test/callback", "https://client.test/callback#", "https://*.test/callback", "https://client.test:/callback", "https://client.test/callback?bad=%xx", strings.Repeat("x", 1025)} {
		t.Run(callback, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"redirect_uris": []string{callback}})
			response := httptest.NewRecorder()
			NewRegistrationHandler(newRegistrationTestService(t), nil).ServeHTTP(response, registrationRequest(string(body)))
			if response.Code != 400 || !strings.Contains(response.Body.String(), "invalid_redirect_uri") {
				t.Fatalf("callback accepted: %d %s", response.Code, response.Body.String())
			}
		})
	}
	for _, callback := range []string{"https://client.test/callback?fixed=1", "http://[::1]:49152/callback"} {
		service := newRegistrationTestService(t)
		client, err := service.RegisterClient(t.Context(), ClientRegistration{RedirectURIs: []string{callback}})
		if err != nil {
			t.Fatal(callback, err)
		}
		if err := service.ValidateAuthorizationTarget(t.Context(), client.ClientID, callback, "https://resource.test/mcp"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDCRRequestLimitsAndMethods(t *testing.T) {
	service := newRegistrationTestService(t)
	handler := NewRegistrationHandler(service, nil)
	for _, method := range []string{"GET", "HEAD", "DELETE"} {
		r := registrationRequest(`{}`)
		r.Method = method
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 405 || w.Header().Get("Allow") != "POST" {
			t.Fatal("method accepted", method)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, registrationRequest(strings.Repeat(" ", 8193)))
	if w.Code != 413 {
		t.Fatal("body limit", w.Code)
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, func(r *http.Request) { r.Header.Add("Content-Type", "application/json") }, func(r *http.Request) { r.Header.Set("Authorization", "Basic c2VjcmV0") }, func(r *http.Request) { r.URL.RawQuery = "x=y" }} {
		r := registrationRequest(`{"redirect_uris":["https://client.test/callback"]}`)
		change(r)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("invalid request accepted", w.Code)
		}
	}
}

func TestDCRSourceRateLimitAndExpiry(t *testing.T) {
	service := newRegistrationTestService(t)
	now := time.Now()
	service.now = func() time.Time { return now }
	handler := NewRegistrationHandler(service, nil)
	for i := 0; i < 11; i++ {
		r := registrationRequest(`{"redirect_uris":["http://127.0.0.1:49152/callback"]}`)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := 201
		if i == 10 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("request %d: %d", i, w.Code)
		}
		if want == 429 && w.Header().Get("Retry-After") == "" {
			t.Fatal("missing retry")
		}
	}
	now = now.Add(time.Minute)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, registrationRequest(`{"redirect_uris":["http://127.0.0.1:49152/callback"]}`))
	if w.Code != 201 {
		t.Fatal("만료된 예산 미회복", w.Code)
	}
	h := NewRegistrationHandler(service, nil).(*registrationHandler)
	for i := 0; i < 4096; i++ {
		r := registrationRequest(`{}`)
		r.RemoteAddr = fmt.Sprintf("[2001:db8:%x::1]:50000", i)
		if wait := h.allow(r); wait != 0 {
			t.Fatal("early source saturation", i)
		}
	}
	r := registrationRequest(`{}`)
	r.RemoteAddr = "[2001:db8:ffff::1]:50000"
	if h.allow(r) <= 0 {
		t.Fatal("source capacity bypassed")
	}
	now = now.Add(time.Minute)
	if h.allow(r) != 0 {
		t.Fatal("source expiry not reclaimed")
	}
}

type failedClientStore struct {
	testClients
	cause error
}

func (s failedClientStore) OAuthClient(context.Context, string) (store.OAuthClient, error) {
	return store.OAuthClient{}, s.cause
}
func (s failedClientStore) RegisterOAuthClient(context.Context, store.OAuthClient) error {
	return s.cause
}
func TestDCRStoreErrorsAndCancellation(t *testing.T) {
	service := newRegistrationTestService(t)
	service.oauth.Clients = failedClientStore{cause: errors.New("private database detail")}
	if err := service.ValidateAuthorizationTarget(t.Context(), "client", "http://127.0.0.1:49152/callback", "https://resource.test/mcp"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("DB 장애를 미등록으로 분류", err)
	}
	_, err := service.ExchangeAuthorizationCode(t.Context(), "code", "client", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", oauthVerifier)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("코드 교환이 DB 장애를 자격 오류로 분류", err)
	}
	w := httptest.NewRecorder()
	NewRegistrationHandler(service, nil).ServeHTTP(w, registrationRequest(`{"redirect_uris":["http://127.0.0.1:49152/callback"]}`))
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("registration store failure", w.Code, w.Body.String())
	}
	service.oauth.Clients = failedClientStore{cause: store.ErrOAuthClientCapacity}
	w = httptest.NewRecorder()
	NewRegistrationHandler(service, nil).ServeHTTP(w, registrationRequest(`{"redirect_uris":["http://127.0.0.1:49152/callback"]}`))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("capacity handling", w.Code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.RegisterClient(ctx, ClientRegistration{RedirectURIs: []string{"https://client.test/callback"}}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}

func TestDCRMetadataLimitsAndIPv6Budget(t *testing.T) {
	base := "https://client.test/"
	for _, length := range []int{1024, 1025} {
		service := newRegistrationTestService(t)
		_, err := service.RegisterClient(t.Context(), ClientRegistration{RedirectURIs: []string{base + strings.Repeat("x", length-len(base))}})
		if (err == nil) != (length == 1024) {
			t.Fatalf("URI bytes=%d: %v", length, err)
		}
	}
	for _, length := range []int{128, 129} {
		service := newRegistrationTestService(t)
		_, err := service.RegisterClient(t.Context(), ClientRegistration{RedirectURIs: []string{base}, ClientName: strings.Repeat("한", length)})
		if (err == nil) != (length == 128) {
			t.Fatalf("name runes=%d: %v", length, err)
		}
	}
	for _, count := range []int{8, 9} {
		callbacks := make([]string, count)
		for i := range callbacks {
			callbacks[i] = fmt.Sprintf("https://client.test/callback%d", i)
		}
		_, err := newRegistrationTestService(t).RegisterClient(t.Context(), ClientRegistration{RedirectURIs: callbacks})
		if (err == nil) != (count == 8) {
			t.Fatalf("callback count=%d: %v", count, err)
		}
	}
	service := newRegistrationTestService(t)
	h := NewRegistrationHandler(service, nil).(*registrationHandler)
	for i := range 10 {
		r := registrationRequest(`{}`)
		r.RemoteAddr = fmt.Sprintf("[2001:db8:1::%x]:49152", i+1)
		if h.allow(r) != 0 {
			t.Fatal("early shared /64 limit")
		}
	}
	r := registrationRequest(`{}`)
	r.RemoteAddr = "[2001:db8:1::ffff]:49152"
	if h.allow(r) <= 0 {
		t.Fatal("IPv6 /64 budget bypassed")
	}
	r.RemoteAddr = "[2001:db8:2::1]:49152"
	if h.allow(r) != 0 {
		t.Fatal("independent IPv6 /64 blocked")
	}
}

type canceledClientLookup struct {
	testClients
	cancel context.CancelFunc
}

func (s canceledClientLookup) OAuthClient(context.Context, string) (store.OAuthClient, error) {
	s.cancel()
	return store.OAuthClient{}, store.ErrNotFound
}
func TestDCRRegistryFailureInRefreshAndBearer(t *testing.T) {
	service, claims, sign := bearerFixture(t)
	token := sign(claims)
	service.oauth.Clients = failedClientStore{cause: errors.New("private registry outage")}
	if _, err := service.Refresh(t.Context(), strings.Repeat("A", 43), "first", service.oauth.Resource, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatal("refresh hid registry outage", err)
	}
	if _, err := service.VerifyBearer(t.Context(), token); !errors.Is(err, ErrUnavailable) {
		t.Fatal("bearer hid registry outage", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	service.oauth.Clients = canceledClientLookup{cancel: cancel}
	if err := service.ValidateAuthorizationTarget(ctx, "first", "http://127.0.0.1:49152/callback", service.oauth.Resource); !errors.Is(err, context.Canceled) {
		t.Fatal("lookup cancellation hidden as missing client", err)
	}
}

func TestDCRLocalhostCallbackRequiresExactMatch(t *testing.T) {
	service := newRegistrationTestService(t)
	registered := "http://localhost:49152/callback?fixed=1"
	client, err := service.RegisterClient(t.Context(), ClientRegistration{RedirectURIs: []string{registered}})
	if err != nil {
		t.Fatal("localhost callback registration failed", err)
	}
	for _, callback := range []string{registered, "http://localhost:49153/callback?fixed=1", "http://127.0.0.1:49152/callback?fixed=1", "http://[::1]:49152/callback?fixed=1", "http://localhost:49152/other?fixed=1", "http://localhost:49152/callback?fixed=2", "http://LOCALHOST:49152/callback?fixed=1", "http://localhost.:49152/callback?fixed=1"} {
		err := service.ValidateAuthorizationTarget(t.Context(), client.ClientID, callback, "https://resource.test/mcp")
		if (err == nil) != (callback == registered) {
			t.Errorf("callback %q accepted=%t", callback, err == nil)
		}
	}
	for _, callback := range []string{"http://localhost/callback", "http://localhost:0/callback", "http://localhost:65536/callback", "http://localhost.evil.test:49152/callback", "http://other.localhost:49152/callback", "http://localhost.:49152/callback", "http://LOCALHOST:49152/callback", "http://user@localhost:49152/callback", "http://localhost:49152/callback#fragment"} {
		if _, err := service.RegisterClient(t.Context(), ClientRegistration{RedirectURIs: []string{callback}}); err == nil {
			t.Errorf("unsafe localhost callback registered: %q", callback)
		}
	}
}
