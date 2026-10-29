package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 실제 CLI의 첫 전송을 먼저 대조한다. 여기서 발급한 시험 Bearer는 OAuth
// 호스트 인수를 대신하지 않으며, 전송 실패 뒤 인가·저장 성공을 추정하지 않는다.
func TestTEST08CodexCLITransportIntegration(t *testing.T) {
	binary := test08CLI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	database, _, address := newServerContractDatabase(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic provider unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	certificate, rootPEM := test08Certificate(t)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	tlsLog := new(test08TLSLog)
	server.Config.ErrorLog = log.New(tlsLog, "", 0)
	origin := "https://" + server.Listener.Addr().String()
	cfg := arch06Configuration(t, address, provider.URL)
	cfg.MCPResourceURL, _ = url.Parse(origin + "/mcp")
	cfg.AuthorizationServerURL, _ = url.Parse(origin)
	cfg.MCPAllowedOrigins = []string{origin}
	client := cfg.OAuthClients["codex-cli-first-test"]
	client.Resources = []string{cfg.MCPResourceURL.String()}
	cfg.OAuthClients["codex-cli-first-test"] = client
	service, err := authz.New(database, authz.Config{Issuer: origin, BcryptCost: 4, OAuth: oauthConfiguration(cfg), AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database, Connections: database})
	if err != nil {
		t.Fatal(err)
	}
	account, err := service.Register(t.Context(), "test08_owner", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	callback := "http://127.0.0.1:49152/callback"
	code, err := service.IssueAuthorizationCode(t.Context(), account, authz.AuthorizationRequest{ClientID: "codex-cli-first-test", RedirectURI: callback, Resource: cfg.MCPResourceURL.String(), ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := service.ExchangeAuthorizationCode(t.Context(), code, "codex-cli-first-test", callback, cfg.MCPResourceURL.String(), "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if err != nil {
		t.Fatal("시험 접근 토큰 발급 실패")
	}
	assembly, err := assembleApplication(t.Context(), cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := assembly.close(nil, true); err != nil {
			t.Error(err)
		}
	})
	var mu sync.Mutex
	var observations []test08WireObservation
	var caControlRequests atomic.Int64
	next := assembly.app.handler()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/model/") {
			caControlRequests.Add(1)
			http.Error(w, "local inference guard", http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/mcp" {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
		if err != nil || len(body) > 4<<20 {
			http.Error(w, "test observation limit", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var wire struct {
			Method string `json:"method"`
			Params struct {
				ProtocolVersion string         `json:"protocolVersion"`
				Meta            map[string]any `json:"_meta"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &wire)
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		var response struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &response)
		observation := test08WireObservation{HTTPMethod: r.Method, RPCMethod: wire.Method, HeaderRevision: r.Header.Get("MCP-Protocol-Version"), HeaderMethod: r.Header.Get("Mcp-Method"), InitializeRevision: wire.Params.ProtocolVersion, Status: recorder.Code}
		observation.Revision, _ = wire.Params.Meta[sdk.MetaKeyProtocolVersion].(string)
		_, observation.Capabilities = wire.Params.Meta[sdk.MetaKeyClientCapabilities]
		if response.Error != nil {
			observation.ErrorCode = response.Error.Code
		}
		mu.Lock()
		observations = append(observations, observation)
		mu.Unlock()
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
	server.StartTLS()
	root := t.TempDir()
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, rootPEM, 0600); err != nil {
		t.Fatal(err)
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no model inference in transport acceptance", http.StatusBadRequest)
	}))
	t.Cleanup(model.Close)
	configuration := fmt.Sprintf(`model = "test08-local"
model_provider = "test08"
features.mcp_2026_07_28 = true
analytics.enabled = false
check_for_update_on_startup = false
mcp_oauth_credentials_store = "file"
[model_providers.test08]
name = "TEST-08 local inference guard"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
[mcp_servers.test08]
url = %q
bearer_token_env_var = "ACS_TEST08_BEARER"
required = true
startup_timeout_sec = 10
`, model.URL, origin+"/mcp")
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	childEnv := []string{}
	// 부모의 API 키·proxy·기존 Codex 자격을 전달하지 않는다. CODEX_HOME은
	// 이 자식의 공식 상태 루트로만 사용하며 사용자 설정을 수정하지 않는다.
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG"} {
		if value := os.Getenv(key); value != "" {
			childEnv = append(childEnv, key+"="+value)
		}
	}
	childEnv = append(childEnv, "CODEX_HOME="+root, "CODEX_CA_CERTIFICATE="+ca, "SSL_CERT_FILE="+ca, "ACS_TEST08_BEARER="+tokens.AccessToken)
	runCLI := func(timeout time.Duration, overrides ...string) error {
		childContext, stop := context.WithTimeout(ctx, timeout)
		defer stop()
		args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--json"}
		for _, override := range overrides {
			args = append(args, "-c", override)
		}
		args = append(args, "Return TEST08.")
		command := exec.CommandContext(childContext, binary, args...)
		command.Dir, command.Env, command.WaitDelay = root, childEnv, 2*time.Second
		command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
		return command.Run()
	}
	// 같은 인증서의 일반 HTTPS 클라이언트 도달 여부와 MCP 연결을 구분한다.
	// 모델 대역은 항상 거부하며 실제 추론을 실행하지 않는다.
	_ = runCLI(8*time.Second, "mcp_servers.test08.enabled=false", fmt.Sprintf("model_providers.test08.base_url=%q", origin+"/model"))
	// 기능의 존재와 기본 활성화는 별개다. 같은 설치 판에서 비활성 대조와
	// 활성 시험을 모두 실행하여 이전 프로토콜 요청을 미지원으로 오판하지 않는다.
	disabledErr := runCLI(20*time.Second, "features.mcp_2026_07_28=false")
	mu.Lock()
	disabled := slices.Clone(observations)
	observations = nil
	mu.Unlock()
	if disabledErr == nil || len(disabled) != 1 || disabled[0].RPCMethod != "initialize" || disabled[0].InitializeRevision != "2025-06-18" || disabled[0].Status != http.StatusBadRequest || disabled[0].ErrorCode != -32020 {
		t.Fatal("TEST-08 기능 비활성 대조가 예상한 이전 프로토콜 거부와 다릅니다")
	}
	t.Log("TEST-08 mcp_2026_07_28=false: legacy initialize rejected as expected; not a host support verdict")
	// CLI 원문에는 요청·경로가 포함될 수 있으므로 시험 로그로 복제하지 않는다.
	err = runCLI(20 * time.Second)
	mu.Lock()
	observed := slices.Clone(observations)
	mu.Unlock()
	data, marshalErr := json.Marshal(observed)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	t.Logf("TEST-08 config=test08-transport-v2 time=%s surface=macOS/CLI/exec arch=%s version=0.160.1 mcp_2026_07_28=true correlation=transport-1 ca_control_requests=%d tls_unknown_certificate=%d wire=%s", time.Now().UTC().Format(time.RFC3339), runtime.GOARCH, caControlRequests.Load(), tlsLog.unknown.Load(), data)
	if caControlRequests.Load() == 0 {
		t.Fatal("같은 CA를 사용하는 일반 HTTPS 대조에 도달하지 못했습니다")
	}
	if len(observed) == 0 {
		t.Fatal("실제 CLI의 MCP 요청이 관찰되지 않았습니다")
	}
	for _, observation := range observed {
		if observation.HTTPMethod == "POST" && (observation.Revision != "2026-07-28" || !observation.Capabilities || observation.HeaderRevision != "2026-07-28" || observation.HeaderMethod != observation.RPCMethod || observation.Status != http.StatusOK) {
			t.Fatal("TEST-08 실제 CLI의 첫 MCP 전송이 채택한 계약과 일치하지 않습니다")
		}
	}
	listed := false
	for _, observation := range observed {
		listed = listed || (observation.RPCMethod == "tools/list" && observation.Status == http.StatusOK)
	}
	if !listed {
		t.Fatal("MCP 도구 목록을 인수하지 못했습니다")
	}
	// 로컬 모델의 의도적인 거부로 CLI는 실패할 수 있다. 여기서 인수하는
	// 것은 실제 도구 목록 전송까지이며 OAuth·과업·전체 지원은 별도다.
	t.Logf("TEST-08 transport-only accepted; local model guard exit=%t; full host acceptance pending", err != nil)
}

func test08CLI(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("TEST_CODEX_BINARY")
	if binary == "" {
		if os.Getenv("TEST_CODEX_REQUIRED") != "" {
			t.Fatal("TEST_CODEX_BINARY가 필요합니다")
		}
		t.Skip("TEST_CODEX_BINARY가 없어 실제 Codex CLI 인수를 건너뜁니다")
	}
	if runtime.GOOS != "darwin" || runtime.Version() != "go1.27.1" {
		t.Fatal("TEST-08 대상은 macOS·Go 1.27.1입니다")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli 0.160.1" {
		t.Fatal("TEST-08 대상 Codex CLI 0.160.1을 확인하지 못했습니다")
	}
	return binary
}

// httptest의 기본 인증서는 CA=true인 인증서를 서버 인증서로도 쓴다.
// 실제 호스트의 end-entity 검증을 위해 별도 CA와 CA=false leaf를 만든다.
func test08Certificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	return test08CertificateValidFor(t, time.Hour)
}

func test08CertificateValidFor(t *testing.T, lifetime time.Duration) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "TEST-08 temporary CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(lifetime), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "TEST-08 loopback"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(lifetime), BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	if err != nil {
		t.Fatal(err)
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
}

type test08TLSLog struct{ unknown atomic.Int64 }

func (output *test08TLSLog) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("unknown certificate")) {
		output.unknown.Add(1)
	}
	return len(data), nil
}

type test08WireObservation struct {
	HTTPMethod         string `json:"http_method"`
	RPCMethod          string `json:"rpc_method"`
	HeaderRevision     string `json:"header_revision"`
	HeaderMethod       string `json:"header_method"`
	InitializeRevision string `json:"initialize_revision"`
	Revision           string `json:"revision"`
	Capabilities       bool   `json:"capabilities"`
	Status             int    `json:"status"`
	ErrorCode          int    `json:"error_code"`
}
