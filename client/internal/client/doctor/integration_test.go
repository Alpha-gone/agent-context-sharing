package doctor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/contract"
	"agent_context_sharing/client/internal/client/remote"
)

func TestDoctorWithTLSMetadataBrowserAuthorizationAndRestart(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoding := base64.RawURLEncoding
	public := map[string]string{"crv": "P-256", "kty": "EC", "x": encoding.EncodeToString(key.X.FillBytes(make([]byte, 32))), "y": encoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))), "kid": "server-key"}
	now := time.Unix(1_800_000_000, 0)
	var issuer string
	var mu sync.Mutex
	var methods, thumbprints, tokens, states, callbacks []string
	var sensitive []string
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.MessageKey {
			return slog.Attr{}
		}
		return a
	}}))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if proof := r.Header.Get("DPoP"); proof != "" {
			mu.Lock()
			sensitive = append(sensitive, proof)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		write := func(v any) {
			if err := json.MarshalWrite(w, v); err != nil {
				t.Error(err)
			}
		}
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			write(map[string]any{"resource": issuer + "/mcp", "authorization_servers": []string{issuer}, "dpop_bound_access_tokens_required": true, "dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{"agent-context"}})
		case "/.well-known/oauth-authorization-server":
			write(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks", "code_challenge_methods_supported": []string{"S256"}, "dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{"agent-context"}, "token_endpoint_auth_methods_supported": []string{"none"}, "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "authorization_response_iss_parameter_supported": true})
		case "/jwks":
			write(map[string]any{"keys": []any{public}})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			mu.Lock()
			sensitive = append(sensitive, r.Form.Get("code_verifier"))
			mu.Unlock()
			if r.Form.Get("resource") != issuer+"/mcp" || r.Form.Get("code") != "private-code" {
				t.Error("토큰 교환 결합 오류")
			}
			parts := strings.Split(r.Header.Get("DPoP"), ".")
			if len(parts) != 3 {
				t.Error("토큰 proof 누락")
				w.WriteHeader(400)
				return
			}
			headerRaw, _ := encoding.DecodeString(parts[0])
			var proof struct {
				JWK map[string]string `json:"jwk"`
			}
			if err := json.Unmarshal(headerRaw, &proof); err != nil {
				t.Error(err)
			}
			canonical, _ := json.Marshal(proof.JWK, json.Deterministic(true))
			hash := sha256.Sum256(canonical)
			jkt := encoding.EncodeToString(hash[:])
			header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "server-key"})
			claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": []string{issuer + "/mcp"}, "sub": "diagnostic-account", "exp": now.Unix() + 3600, "cnf": map[string]string{"jkt": jkt}})
			unsigned := encoding.EncodeToString(header) + "." + encoding.EncodeToString(claims)
			digest := sha256.Sum256([]byte(unsigned))
			x, y, err := ecdsa.Sign(rand.Reader, key, digest[:])
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			token := unsigned + "." + encoding.EncodeToString(append(x.FillBytes(make([]byte, 32)), y.FillBytes(make([]byte, 32))...))
			mu.Lock()
			thumbprints = append(thumbprints, jkt)
			tokens = append(tokens, token)
			mu.Unlock()
			write(map[string]any{"access_token": token, "token_type": "DPoP", "expires_in": 3600, "scope": "agent-context"})
		case "/mcp":
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
				Params struct {
					Meta map[string]jsontext.Value `json:"_meta"`
				} `json:"params"`
			}
			if err := json.UnmarshalRead(r.Body, &request); err != nil {
				t.Error(err)
			}
			if r.Header.Get("Authorization") == "" || r.Header.Get("DPoP") == "" {
				t.Error("진단 보호 요청 인증 누락")
			}
			if !bytes.Contains(request.Params.Meta["io.modelcontextprotocol/clientInfo"], []byte(`"version":"doctor-test"`)) {
				t.Error("원격 clientInfo 누락")
			}
			mu.Lock()
			methods = append(methods, request.Method)
			mu.Unlock()
			var result jsontext.Value
			switch request.Method {
			case "server/discover":
				result = jsontext.Value(`{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"ttlMs":10000,"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"fixture","version":"1"}}}`)
			case "tools/list":
				result = jsontext.Value(`{"tools":` + string(contract.Manifest()) + `,"ttlMs":10000}`)
			default:
				t.Error("doctor가 도메인 도구를 호출했습니다")
				w.WriteHeader(400)
				return
			}
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		default:
			t.Error("예상 밖 외부 경로")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	issuer = server.URL
	env := map[string]string{"AGENT_CONTEXT_CLIENT_REMOTE_URL": issuer + "/mcp", "AGENT_CONTEXT_CLIENT_ID": "public-client", "AGENT_CONTEXT_CLIENT_AGENT_ID": uuid.NewV7().String(), "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": "3s", "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "1s"}
	opener := func(ctx context.Context, target string) error {
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		callback := target
		if u.Scheme == "https" {
			query := u.Query()
			mu.Lock()
			sensitive = append(sensitive, target, query.Get("code_challenge"))
			mu.Unlock()
			callback = query.Get("redirect_uri") + "?" + url.Values{"state": {query.Get("state")}, "iss": {issuer}, "code": {"private-code"}}.Encode()
			mu.Lock()
			states = append(states, query.Get("state"))
			callbacks = append(callbacks, query.Get("redirect_uri"))
			mu.Unlock()
		} else if u.Path != "/diagnostic" || u.RawQuery != "" {
			t.Error("진단 URL에 비밀·다른 경로가 있습니다")
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, callback, nil)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		if response.StatusCode != 200 {
			t.Errorf("callback 상태 %d", response.StatusCode)
		}
		// callback 수신은 실행기 반환과 독립적이어야 한다.
		<-ctx.Done()
		return ctx.Err()
	}
	options := Options{
		Authorization: authorize.Options{Transport: server.Client().Transport, OpenBrowser: opener, Now: func() time.Time { return now }},
		HTTP:          remote.HTTPOptions{Transport: server.Client().Transport, Version: "doctor-test", Logger: logger},
	}
	for range 2 {
		report := Run(t.Context(), func(k string) string { return env[k] }, options)
		if report.ExitCode() != 0 {
			t.Fatalf("실제 진단 흐름: %+v", report)
		}
		for _, asJSON := range []bool{false, true} {
			var out bytes.Buffer
			report.Write(&out, asJSON)
			mu.Lock()
			secrets := append([]string{issuer, "private-code", "diagnostic-account"}, tokens...)
			secrets = append(secrets, states...)
			secrets = append(secrets, sensitive...)
			mu.Unlock()
			for _, secret := range secrets {
				if strings.Contains(out.String()+logs.String(), secret) {
					t.Fatal("진단 비밀 유출")
				}
			}
		}
	}
	mu.Lock()
	if strings.Join(methods, ",") != "server/discover,tools/list,server/discover,tools/list" || len(thumbprints) != 2 || thumbprints[0] == thumbprints[1] || tokens[0] == tokens[1] || states[0] == states[1] {
		t.Error("재시작 상태 재사용·진단 도구 호출")
	}
	oldCallbacks := append([]string(nil), callbacks...)
	mu.Unlock()
	for _, callback := range oldCallbacks {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, callback, nil)
		response, err := http.DefaultClient.Do(request)
		cancel()
		if err == nil {
			response.Body.Close()
			t.Error("이전 callback이 살아 있습니다")
		}
	}
	if err := checkNetwork(t.Context(), issuer+"/mcp"); err == nil {
		t.Fatal("신뢰하지 않는 TLS 인증서를 승인했습니다")
	}
}
