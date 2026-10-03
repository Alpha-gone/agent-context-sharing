package remote

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
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

// TestRelayPerformance1KiB100Concurrent는 준비된 stdio→TLS 중계의 보수적 상한을 잰다.
// 원격 loopback 왕복도 포함하므로 자체 처리 시간은 측정값보다 클 수 없다.
// race 빌드의 시간은 출시 판정에 쓰지 않으며 별도 일반 빌드 실행에서만 기준을 강제한다.
func TestRelayPerformance1KiB100Concurrent(t *testing.T) {
	fixture := newCatalogSource(t)
	now := time.Unix(1_800_000_000, 0)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var base atomic.Value
	var result atomic.Value
	result.Store(jsontext.Value(`{"content":[{"type":"text","text":""}]}`))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := base.Load().(string)
		w.Header().Set("Content-Type", "application/json")
		write := func(value any) {
			if err := json.MarshalWrite(w, value); err != nil {
				t.Error(err)
			}
		}
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			write(map[string]any{"resource": issuer + "/mcp", "authorization_servers": []string{issuer}, "dpop_bound_access_tokens_required": true, "dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{"agent-context"}})
			return
		case "/.well-known/oauth-authorization-server":
			write(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks", "code_challenge_methods_supported": []string{"S256"}, "dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{"agent-context"}, "token_endpoint_auth_methods_supported": []string{"none"}, "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "authorization_response_iss_parameter_supported": true})
			return
		case "/jwks":
			public := publicKey(key)
			write(map[string]any{"keys": []any{map[string]string{"crv": public.Crv, "kty": public.Kty, "x": public.X, "y": public.Y, "kid": "server-key"}}})
			return
		case "/token":
			_, thumbprint := checkTestProof(t, r.Header.Get("DPoP"), issuer+"/token", "", now)
			write(map[string]any{"access_token": signedTestToken(t, key, issuer, thumbprint, "account-a", now.Unix()+3600), "token_type": "DPoP", "expires_in": 3600, "scope": "agent-context"})
			return
		case "/mcp":
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("WWW-Authenticate", `DPoP`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			ID string `json:"id"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error("성능 시험 원격 JSON 오류")
			return
		}
		var raw jsontext.Value
		switch r.Header.Get("Mcp-Method") {
		case "server/discover":
			raw = fixture.discovery
		case "tools/list":
			raw = fixture.tools
		default:
			raw = result.Load().(jsontext.Value)
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%q,"result":%s}`, request.ID, raw)
	}))
	defer server.Close()
	base.Store(server.URL)
	loopback := server.Client().Transport
	defer loopback.(*http.Transport).CloseIdleConnections()
	env := map[string]string{"AGENT_CONTEXT_CLIENT_REMOTE_URL": server.URL + "/mcp", "AGENT_CONTEXT_CLIENT_ID": "perf-client", "AGENT_CONTEXT_CLIENT_AGENT_ID": uuid.NewV7().String(), "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": "5s", "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "5s"}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	manager, err := authorize.New(cfg, authorize.Options{Transport: loopback, Now: func() time.Time { return now }, OpenBrowser: func(ctx context.Context, target string) error {
		parsed, err := url.Parse(target)
		if err != nil {
			return err
		}
		query := parsed.Query()
		callback, err := url.Parse(query.Get("redirect_uri"))
		if err != nil {
			return err
		}
		callback.RawQuery = url.Values{"state": {query.Get("state")}, "iss": {server.URL}, "code": {"perf-code"}}.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, callback.String(), nil)
		if err != nil {
			return err
		}
		response, err := (&http.Client{Transport: loopback}).Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		if response.StatusCode != http.StatusOK {
			return authorize.ErrAuthorization
		}
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	c, err := NewHTTP(cfg, manager, HTTPOptions{Transport: loopback, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.ListTools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	in, send := io.Pipe()
	receive, out := io.Pipe()
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	policy, err := contract.NewPolicy("all", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer out.Close()
		done <- host.Run(ctx, in, out, slog.New(slog.NewJSONHandler(io.Discard, nil)), "perf", host.NewTools(c, policy, uuid.NewV7()))
	}()
	t.Cleanup(func() {
		cancel()
		send.Close()
		receive.Close()
		out.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	reader := bufio.NewReader(receive)
	frame := func(id string) []byte {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"graph_list","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"performancePadding":""}}}`, id)
		padding := strings.Repeat("x", 1024-len(body))
		body = strings.Replace(body, `"performancePadding":""`, `"performancePadding":"`+padding+`"`, 1)
		if len(body) != 1024 {
			t.Fatal("1 KiB 요청 크기 불일치")
		}
		return append([]byte(body), '\n')
	}
	warmup := func() []byte {
		t.Helper()
		if _, err := send.Write(frame("warmup")); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	initial := warmup()
	if len(initial) >= 1025 {
		t.Fatal("시험 응답 외피가 1 KiB를 넘는다")
	}
	result.Store(jsontext.Value(`{"content":[{"type":"text","text":"` + strings.Repeat("x", 1025-len(initial)) + `"}]}`))
	if line := warmup(); len(line) != 1025 {
		t.Fatalf("1 KiB 응답 크기=%d", len(line)-1)
	}
	var starts sync.Map
	latencies := make([]time.Duration, 0, 100)
	var workers sync.WaitGroup
	var writer sync.Mutex
	start := make(chan struct{})
	for i := range 100 {
		workers.Go(func() {
			<-start
			id := fmt.Sprintf("%06d", i)
			body := frame(id)
			starts.Store(id, time.Now())
			writer.Lock()
			_, err := send.Write(body)
			writer.Unlock()
			if err != nil {
				t.Error("성능 시험 전송 실패")
			}
		})
	}
	close(start)
	for range 100 {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			ID     string         `json:"id"`
			Error  jsontext.Value `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if json.Unmarshal(line, &response) != nil || len(response.Error) != 0 || response.Result.IsError || len(line) != 1025 {
			t.Fatal("1 KiB 동시 응답 계약 위반")
		}
		started, ok := starts.LoadAndDelete(response.ID)
		if !ok {
			t.Fatal("동시 응답 ID 혼선")
		}
		latencies = append(latencies, time.Since(started.(time.Time)))
	}
	workers.Wait()
	slices.Sort(latencies)
	p95 := latencies[94]
	t.Logf("1 KiB 요청/응답, 동시 100개, stdio 대기·TLS 원격 왕복 포함 p95=%s", p95)
	if os.Getenv("CLIENT_PERFORMANCE_REQUIRED") == "1" && p95 > 50*time.Millisecond {
		t.Fatalf("자체 처리 시간의 보수적 상한 p95=%s > 50ms", p95)
	}
}
