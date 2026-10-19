package authz

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func oauthTestConfig() Config {
	const resource = "https://resource.test/mcp"
	return Config{Issuer: "https://issuer.test", BcryptCost: 4, OAuth: &OAuthConfig{Resource: resource, Clients: map[string]OAuthClient{
		"first":       {RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{resource}, LoopbackPortPolicy: "exact"},
		"native":      {RedirectURIs: []string{"http://127.0.0.1:4000/callback?mode=fixed"}, Resources: []string{resource}, LoopbackPortPolicy: "same_ip"},
		"native6":     {RedirectURIs: []string{"http://[::1]:4000/callback"}, Resources: []string{resource}, LoopbackPortPolicy: "same_ip"},
		"nativeUpper": {RedirectURIs: []string{"HTTP://127.0.0.1:4000/callback"}, Resources: []string{resource}, LoopbackPortPolicy: "same_ip"},
		"web":         {RedirectURIs: []string{"https://client.test/callback?mode=fixed"}, Resources: []string{resource}, LoopbackPortPolicy: "same_ip"},
	}}}
}

func TestOAuthAuthorizationTargetBinding(t *testing.T) {
	service, err := New(&sessionFaultStore{}, oauthTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, client, callback, resource string
		valid                            bool
	}{
		{"first exact", "first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", true},
		{"first changed port", "first", "http://127.0.0.1:49153/callback", "https://resource.test/mcp", false},
		{"first encoded path", "first", "http://127.0.0.1:49152/%63allback", "https://resource.test/mcp", false},
		{"first query", "first", "http://127.0.0.1:49152/callback?x=1", "https://resource.test/mcp", false},
		{"first ipv6", "first", "http://[::1]:49152/callback", "https://resource.test/mcp", false},
		{"first localhost", "first", "http://localhost:49152/callback", "https://resource.test/mcp", false},
		{"native port", "native", "http://127.0.0.1:54321/callback?mode=fixed", "https://resource.test/mcp", true},
		{"native minimum port", "native", "http://127.0.0.1:1/callback?mode=fixed", "https://resource.test/mcp", true},
		{"native maximum port", "native", "http://127.0.0.1:65535/callback?mode=fixed", "https://resource.test/mcp", true},
		{"native no port", "native", "http://127.0.0.1/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native empty port", "native", "http://127.0.0.1:/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native zero port", "native", "http://127.0.0.1:0/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native large port", "native", "http://127.0.0.1:65536/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native named port", "native", "http://127.0.0.1:https/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native other ip", "native", "http://127.0.0.2:54321/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native public ip", "native", "http://192.0.2.1:54321/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native mapped", "native", "http://[::ffff:127.0.0.1]:54321/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native user info", "native", "http://user@127.0.0.1:54321/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native fragment", "native", "http://127.0.0.1:54321/callback?mode=fixed#", "https://resource.test/mcp", false},
		{"native missing query", "native", "http://127.0.0.1:54321/callback", "https://resource.test/mcp", false},
		{"native changed query", "native", "http://127.0.0.1:54321/callback?mode=other", "https://resource.test/mcp", false},
		{"native path suffix", "native", "http://127.0.0.1:54321/callback/?mode=fixed", "https://resource.test/mcp", false},
		{"native encoded path", "native", "http://127.0.0.1:54321/%63allback?mode=fixed", "https://resource.test/mcp", false},
		{"native changed scheme", "native", "https://127.0.0.1:54321/callback?mode=fixed", "https://resource.test/mcp", false},
		{"native uppercase scheme", "native", "HTTP://127.0.0.1:54321/callback?mode=fixed", "https://resource.test/mcp", false},
		{"registered uppercase scheme", "nativeUpper", "HTTP://127.0.0.1:54321/callback", "https://resource.test/mcp", true},
		{"registered uppercase scheme changed", "nativeUpper", "http://127.0.0.1:54321/callback", "https://resource.test/mcp", false},
		{"native whitespace", "native", " http://127.0.0.1:54321/callback?mode=fixed", "https://resource.test/mcp", false},
		{"ipv6 port", "native6", "http://[::1]:54321/callback", "https://resource.test/mcp", true},
		{"ipv6 different spelling", "native6", "http://[0:0:0:0:0:0:0:1]:54321/callback", "https://resource.test/mcp", false},
		{"ipv6 zone", "native6", "http://[::1%25lo0]:54321/callback", "https://resource.test/mcp", false},
		{"https exact", "web", "https://client.test/callback?mode=fixed", "https://resource.test/mcp", true},
		{"https port", "web", "https://client.test:443/callback?mode=fixed", "https://resource.test/mcp", false},
		{"https host case", "web", "https://CLIENT.test/callback?mode=fixed", "https://resource.test/mcp", false},
		{"unknown client", "unregistered", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", false},
		{"URL client is not fetched", "https://client.test/metadata", "https://client.test/callback?mode=fixed", "https://resource.test/mcp", false},
		{"empty client", "", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", false},
		{"empty callback", "first", "", "https://resource.test/mcp", false},
		{"empty resource", "first", "http://127.0.0.1:49152/callback", "", false},
		{"other resource", "first", "http://127.0.0.1:49152/callback", "https://other.test/mcp", false},
		{"resource spelling", "first", "http://127.0.0.1:49152/callback", "https://RESOURCE.test/mcp", false},
		{"resource query", "first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp?x=1", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := service.ValidateAuthorizationTarget(test.client, test.callback, test.resource)
			if test.valid && err != nil {
				t.Fatal("등록된 결합 거부")
			}
			if !test.valid && !errors.Is(err, ErrInvalidAuthorizationTarget) {
				t.Fatal("등록되지 않은 결합 허용")
			}
		})
	}
}

func TestOAuthRegistrationSnapshotIsIndependent(t *testing.T) {
	input := oauthTestConfig()
	service, err := New(&sessionFaultStore{}, input)
	if err != nil {
		t.Fatal(err)
	}
	client := input.OAuth.Clients["first"]
	client.RedirectURIs[0], client.Resources[0] = "https://other.test/callback", "https://other.test/mcp"
	input.OAuth.Resource = "https://other.test/mcp"
	delete(input.OAuth.Clients, "first")
	if err := service.ValidateAuthorizationTarget("first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp"); err != nil {
		t.Fatal("호출자의 변경이 활성 등록을 바꿨습니다")
	}
}

func TestOAuthMetadataIsFixedAndDoesNotReadStore(t *testing.T) {
	service, err := New(&sessionFaultStore{}, oauthTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		serve    http.HandlerFunc
		expected map[string]any
	}{
		{"resource", handler.ProtectedResourceMetadata, map[string]any{"resource": "https://resource.test/mcp", "authorization_servers": []string{"https://issuer.test"}, "scopes_supported": []string{"agent-context"}, "bearer_methods_supported": []string{"header"}}},
		{"issuer", handler.AuthorizationServerMetadata, map[string]any{"issuer": "https://issuer.test", "authorization_endpoint": "https://issuer.test/authorize", "token_endpoint": "https://issuer.test/token", "jwks_uri": "https://issuer.test/jwks.json", "scopes_supported": []string{"agent-context"}, "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "authorization_response_iss_parameter_supported": true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://attacker.test/metadata", nil)
			request.Header.Set("Forwarded", "host=other.test;proto=http")
			response := httptest.NewRecorder()
			test.serve(response, request)
			var actual map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
				t.Fatal("메타데이터 JSON 해석 실패")
			}
			expected, err := json.Marshal(test.expected, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := json.Marshal(actual, json.Deterministic(true))
			if err != nil || string(canonical) != string(expected) || response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Location") != "" || len(response.Result().Cookies()) != 0 {
				t.Fatal("고정 메타데이터·무리디렉션·무세션 계약 불일치")
			}
		})
	}
}

func TestOAuthMetadataWithoutRegistrationFailsClosed(t *testing.T) {
	service := testService(t, &sessionFaultStore{})
	if !errors.Is(service.ValidateAuthorizationTarget("first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp"), ErrInvalidAuthorizationTarget) {
		t.Fatal("등록 없는 서비스가 대조를 허용했습니다")
	}
	handler, err := NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	for _, serve := range []http.HandlerFunc{handler.ProtectedResourceMetadata, handler.AuthorizationServerMetadata} {
		response := httptest.NewRecorder()
		serve(response, httptest.NewRequest(http.MethodGet, "/metadata", nil))
		if response.Code != 503 || response.Header().Get("Location") != "" {
			t.Fatal("등록 없는 서비스가 발견 정보를 만들었습니다")
		}
	}
}
