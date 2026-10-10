package remote

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/contract"
	"agent_context_sharing/client/internal/client/host"
)

var testEncoding = base64.RawURLEncoding

type testPublicKey struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func publicKey(key *ecdsa.PrivateKey) testPublicKey {
	return testPublicKey{"P-256", "EC", testEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))), testEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}
}

func keyHash(key testPublicKey) string {
	body, _ := json.Marshal(key)
	hash := sha256.Sum256(body)
	return testEncoding.EncodeToString(hash[:])
}

func signedTestToken(t *testing.T, key *ecdsa.PrivateKey, issuer, thumbprint, subject string, exp int64) string {
	t.Helper()
	return signedTestTokenWithKid(t, key, issuer, thumbprint, subject, exp, "server-key")
}

func signedTestTokenWithKid(t *testing.T, key *ecdsa.PrivateKey, issuer, thumbprint, subject string, exp int64, kid string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": kid, "typ": "JWT"})
	body, _ := json.Marshal(map[string]any{"iss": issuer, "sub": subject, "aud": []string{issuer + "/mcp"}, "exp": exp, "cnf": map[string]string{"jkt": thumbprint}, "jti": uuid.NewV7().String()})
	unsigned := testEncoding.EncodeToString(header) + "." + testEncoding.EncodeToString(body)
	hash := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return unsigned + "." + testEncoding.EncodeToString(signature)
}

func checkTestProof(t *testing.T, raw, target, token string, now time.Time) (string, string) {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Error("DPoP compact JWT가 아닙니다")
		return "", ""
	}
	headerBytes, e1 := testEncoding.DecodeString(parts[0])
	bodyBytes, e2 := testEncoding.DecodeString(parts[1])
	signature, e3 := testEncoding.DecodeString(parts[2])
	var header struct {
		Alg string        `json:"alg"`
		Typ string        `json:"typ"`
		JWK testPublicKey `json:"jwk"`
	}
	var claims struct {
		HTM string `json:"htm"`
		HTU string `json:"htu"`
		IAT int64  `json:"iat"`
		JTI string `json:"jti"`
		ATH string `json:"ath"`
	}
	if e1 != nil || e2 != nil || e3 != nil || len(signature) != 64 || json.Unmarshal(headerBytes, &header) != nil || json.Unmarshal(bodyBytes, &claims) != nil {
		t.Error("DPoP 외피가 다릅니다")
		return "", ""
	}
	x, _ := testEncoding.DecodeString(header.JWK.X)
	y, _ := testEncoding.DecodeString(header.JWK.Y)
	key := ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !key.Curve.IsOnCurve(key.X, key.Y) || !ecdsa.Verify(&key, hash[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) || header.Alg != "ES256" || header.Typ != "dpop+jwt" || claims.HTM != "POST" || claims.HTU != target || claims.IAT != now.Unix() {
		t.Error("DPoP 서명·요청 결합이 다릅니다")
	}
	ath := ""
	if token != "" {
		hash := sha256.Sum256([]byte(token))
		ath = testEncoding.EncodeToString(hash[:])
	}
	if claims.ATH != ath {
		t.Error("DPoP 접근 토큰 결합이 다릅니다")
	}
	return claims.JTI, keyHash(header.JWK)
}

func TestHTTPWithRealTLSAuthorizationAndConcurrentCalls(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotatedKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var rotated atomic.Bool
	var keyRequests, rotatedRequests atomic.Int32
	fixture := newCatalogSource(t)
	var base atomic.Value
	var subject atomic.Value
	subject.Store("account-a")
	var forceUnauthorized atomic.Bool
	var opens, exchanges, applied, writeAttempts, active, peak atomic.Int32
	var mu sync.Mutex
	var authorization url.Values
	proofIDs, requestIDs, writeKeys := map[string]bool{}, map[string]bool{}, map[string]bool{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := base.Load().(string)
		w.Header().Set("Content-Type", "application/json")
		writeJSON := func(body any) {
			if err := json.MarshalWrite(w, body); err != nil {
				t.Error(err)
			}
		}
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			writeJSON(map[string]any{"resource": issuer + "/mcp", "authorization_servers": []string{issuer}, "dpop_bound_access_tokens_required": true, "dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{"agent-context"}})
		case "/.well-known/oauth-authorization-server":
			writeJSON(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks", "code_challenge_methods_supported": []string{"S256"}, "dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{"agent-context"}, "token_endpoint_auth_methods_supported": []string{"none"}, "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "authorization_response_iss_parameter_supported": true})
		case "/jwks":
			keyRequests.Add(1)
			public := publicKey(key)
			keys := []any{map[string]string{"crv": public.Crv, "kty": public.Kty, "x": public.X, "y": public.Y, "kid": "server-key"}}
			if rotated.Load() {
				public = publicKey(rotatedKey)
				keys = append(keys, map[string]string{"crv": public.Crv, "kty": public.Kty, "x": public.X, "y": public.Y, "kid": "rotated-key"})
			}
			writeJSON(map[string]any{"keys": keys})
		case "/token":
			exchanges.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			mu.Lock()
			query := authorization.Clone()
			mu.Unlock()
			verifierHash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("resource") != issuer+"/mcp" || r.Form.Get("code") != "test-code" || r.Form.Get("redirect_uri") != query.Get("redirect_uri") || testEncoding.EncodeToString(verifierHash[:]) != query.Get("code_challenge") || r.Header.Get("Authorization") != "" {
				t.Error("실제 토큰 교환 결합이 다릅니다")
			}
			_, thumbprint := checkTestProof(t, r.Header.Get("DPoP"), issuer+"/token", "", now)
			writeJSON(map[string]any{"access_token": signedTestToken(t, key, issuer, thumbprint, subject.Load().(string), now.Unix()+3600), "token_type": "DPoP", "expires_in": 3600, "scope": "agent-context"})
		case "/mcp":
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
				Params struct {
					Name      string `json:"name"`
					Arguments struct {
						PageSize int `json:"page_size"`
					} `json:"arguments"`
				} `json:"params"`
			}
			if err := json.UnmarshalRead(r.Body, &request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("WWW-Authenticate", `DPoP resource_metadata="`+issuer+`/.well-known/oauth-protected-resource/mcp", scope="agent-context"`)
				w.WriteHeader(401)
				return
			}
			current := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
			}
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "DPoP ")
			proofID, thumbprint := checkTestProof(t, r.Header.Get("DPoP"), issuer+"/mcp", token, now)
			mu.Lock()
			if proofIDs[proofID] || requestIDs[request.ID] {
				t.Error("HTTP 시도의 proof·원격 ID를 재사용했습니다")
			}
			proofIDs[proofID], requestIDs[request.ID] = true, true
			mu.Unlock()
			if forceUnauthorized.Swap(false) {
				w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token"`)
				w.WriteHeader(401)
				return
			}
			var result jsontext.Value
			switch request.Method {
			case "server/discover":
				result = fixture.discovery
			case "tools/list":
				result = fixture.tools
				rotated.Store(true)
				w.Header().Set("Mcp-Access-Token", signedTestTokenWithKid(t, rotatedKey, issuer, thumbprint, "account-a", now.Unix()+7200, "rotated-key"))
				w.Header().Set("Mcp-Access-Token-Expires-At", strconv.FormatInt(now.Unix()+7200, 10))
			case "tools/call":
				if opens.Load() == 1 {
					parts := strings.Split(token, ".")
					header, _ := testEncoding.DecodeString(parts[0])
					var jwtHeader struct {
						Kid string `json:"kid"`
					}
					if json.Unmarshal(header, &jwtHeader) != nil || jwtHeader.Kid != "rotated-key" {
						t.Error("키 회전 갱신 토큰이 다음 도구 호출에 적용되지 않았습니다")
					}
					rotatedRequests.Add(1)
				}
				if request.Params.Name == "graph_create" {
					attempt := writeAttempts.Add(1)
					mu.Lock()
					writeKeys[r.Header.Get("Idempotency-Key")] = true
					mu.Unlock()
					if attempt == 1 {
						w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token"`)
						w.WriteHeader(401)
						return
					}
					if attempt == 2 {
						applied.Add(1)
						connection, _, err := http.NewResponseController(w).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						connection.Close()
						return
					}
					result = jsontext.Value(`{"content":[],"isError":false,"structuredContent":{"version":9007199254740993,"cursor":"opaque+/="}}`)
				} else {
					time.Sleep(time.Duration(request.Params.Arguments.PageSize%3) * time.Millisecond)
					result = jsontext.Value(fmt.Sprintf(`{"content":[],"isError":false,"structuredContent":{"index":%d}}`, request.Params.Arguments.PageSize))
				}
			default:
				t.Error("미지원 method")
			}
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		default:
			t.Error("예상하지 않은 HTTP 경로")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	base.Store(server.URL)
	env := map[string]string{"AGENT_CONTEXT_CLIENT_REMOTE_URL": server.URL + "/mcp", "AGENT_CONTEXT_CLIENT_ID": "client", "AGENT_CONTEXT_CLIENT_AGENT_ID": uuid.NewV7().String(), "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": "5s", "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "1s"}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	manager, err := authorize.New(cfg, authorize.Options{Transport: server.Client().Transport, Now: func() time.Time { return now }, OpenBrowser: func(ctx context.Context, target string) error {
		if opens.Add(1) == 1 {
			if err := waitDelay(ctx, 2*time.Second); err != nil {
				return err
			}
		}
		parsed, err := url.Parse(target)
		if err != nil {
			return err
		}
		mu.Lock()
		authorization = parsed.Query()
		query := authorization.Clone()
		mu.Unlock()
		callback, _ := url.Parse(query.Get("redirect_uri"))
		callback.RawQuery = url.Values{"state": {query.Get("state")}, "iss": {server.URL}, "code": {"test-code"}}.Encode()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, callback.String(), nil)
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return authorize.ErrAuthorization
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	c, err := NewHTTP(cfg, manager, HTTPOptions{Transport: server.Client().Transport, Now: func() time.Time { return now }, Jitter: func(time.Duration) (time.Duration, error) { return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := c.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 || exchanges.Load() != 1 {
		t.Fatal("초기 실제 보호 도전이 단일 인가로 이어지지 않았습니다")
	}
	if keyRequests.Load() != 2 {
		t.Fatalf("최초 JWKS와 회전 재조회 횟수 = %d", keyRequests.Load())
	}
	identity, err := manager.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for index := range 100 {
		wg.Go(func() {
			result, err := c.CallTool(t.Context(), "graph_list", jsontext.Value(fmt.Sprintf(`{"page_size":%d}`, index+1)))
			if err != nil {
				t.Error(err)
				return
			}
			if !strings.Contains(string(result.Raw()), fmt.Sprintf(`"index":%d}`, index+1)) {
				t.Error("다른 동시 호출의 응답을 받았습니다")
			}
		})
	}
	wg.Wait()
	if rotatedRequests.Load() != 100 || keyRequests.Load() != 2 {
		t.Fatal("회전 토큰·정상 응답 보존 또는 불필요한 JWKS 조회 제한이 다릅니다")
	}
	testTLSStdioRelay(t, c, cfg)
	if peak.Load() > 8 {
		t.Fatal("실제 TLS 동시 전송이 8개를 넘었습니다")
	}
	result, err := c.CallTool(t.Context(), "graph_create", jsontext.Value(`{"name":"write-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Raw()), "9007199254740993") || opens.Load() != 2 || exchanges.Load() != 2 || applied.Load() != 1 || writeAttempts.Load() != 3 {
		t.Fatal("재인가·응답 유실·멱등성 재전송·큰 정수 보존이 다릅니다")
	}
	mu.Lock()
	keys := len(writeKeys)
	emptyKey := writeKeys[""]
	mu.Unlock()
	if keys != 1 || emptyKey {
		t.Fatal("실제 재인가·재시도에서 쓰기 키가 바뀌었습니다")
	}
	if next, err := manager.Identity(t.Context()); err != nil || next != identity {
		t.Fatal("재인가에서 계정 주체가 바뀌었습니다")
	}
	subject.Store("account-b")
	forceUnauthorized.Store(true)
	if result, err := c.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); result != nil || !errors.Is(err, contract.ErrIdentityChanged) || exchanges.Load() != 3 {
		t.Fatal("실제 재인가에서 다른 계정의 토큰·응답을 사용했거나 반복 인가했습니다")
	}
	subject.Store("account-a")
	if _, err := c.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); err != nil || exchanges.Load() != 4 || c.rejected {
		t.Fatalf("현재 계정의 새 요청이 복구되지 않았습니다: %v", err)
	}
}

func testTLSStdioRelay(t *testing.T, upstream *Client, cfg config.Config) {
	t.Helper()
	in, input := io.Pipe()
	output, out := io.Pipe()
	defer input.Close()
	defer output.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- host.Run(ctx, in, out, slog.New(slog.NewJSONHandler(io.Discard, nil)), "tls-host", host.NewTools(upstream, cfg.PublicationPolicy(), cfg.AgentID()))
		out.Close()
	}()
	sent := make(chan error, 1)
	go func() {
		for index := 1; index <= 100; index++ {
			frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":"host-%d","method":"tools/call","params":{"name":"graph_list","arguments":{"page_size":%d},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, index, index)
			if _, err := io.WriteString(input, frame+"\n"); err != nil {
				sent <- err
				return
			}
		}
		sent <- nil
	}()
	reader := bufio.NewReader(output)
	seen := make(map[string]bool)
	for range 100 {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			ID     string `json:"id"`
			Result struct {
				StructuredContent struct {
					Index int `json:"index"`
				} `json:"structuredContent"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		if response.Result.IsError || response.ID != fmt.Sprintf("host-%d", response.Result.StructuredContent.Index) || seen[response.ID] {
			t.Fatalf("TLS → stdio 응답 혼선: %s", line)
		}
		seen[response.ID] = true
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	input.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stdio EOF 종료 시간 초과")
	}
}

func TestHTTPRealConnectionFailureIsBeforeWrite(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	env := map[string]string{"AGENT_CONTEXT_CLIENT_REMOTE_URL": "https://" + address + "/mcp", "AGENT_CONTEXT_CLIENT_ID": "client", "AGENT_CONTEXT_CLIENT_AGENT_ID": uuid.NewV7().String(), "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": "1s", "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "2s"}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	delays := 0
	c, err := NewHTTP(cfg, readyAuthentication(), HTTPOptions{Transport: &http.Transport{Proxy: nil}, Jitter: func(time.Duration) (time.Duration, error) { delays++; return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := executeTest(t.Context(), c, "graph_create"); !errors.Is(err, contract.ErrTransport) || delays != 3 {
		t.Fatalf("실제 dial 실패를 전달 뒤로 분류했습니다: %v", err)
	}
}
