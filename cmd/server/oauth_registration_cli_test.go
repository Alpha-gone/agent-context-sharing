package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
)

// 브라우저·사용자 자격을 대신하지 않고 실제 CLI의 발견→DCR→인가 URL 생성을 검사한다.
func TestDCRCodexCLIRegistrationIntegration(t *testing.T) {
	binary := os.Getenv("TEST_DCR_CODEX_BINARY")
	if binary == "" {
		t.Skip("TEST_DCR_CODEX_BINARY가 없어 실제 Codex DCR handshake를 건너뜁니다")
	}
	version, err := exec.CommandContext(t.Context(), binary, "--version").Output()
	if err != nil {
		t.Fatal("Codex version 조회 실패")
	}
	t.Log(strings.TrimSpace(string(version)))
	database := newPublicAuthenticationStore(t)
	server := httptest.NewUnstartedServer(nil)
	certificate, rootPEM := test08Certificate(t)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	origin := "https://" + server.Listener.Addr().String()
	service, err := authz.New(database, authz.Config{Issuer: origin, BcryptCost: 4, OAuth: &authz.OAuthConfig{Resource: origin + "/mcp", Clients: database}, AccessTokens: database})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := authz.NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(database, nil, transportSecurity{directTLS: true})
	app.oauthMetadata = handler
	app.oauthRegistration = authz.NewRegistrationHandler(service, app.transport.clientAddress)
	app.mcp = handler.ProtectBearer(func(_ http.ResponseWriter, _ *http.Request, _ authz.BearerPrincipal) {})
	server.Config.Handler = app.handler()
	server.StartTLS()
	t.Cleanup(server.Close)
	directory := t.TempDir()
	ca := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(ca, rootPEM, 0600); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf(`features.mcp_2026_07_28 = true
check_for_update_on_startup = false
analytics.enabled = false
mcp_oauth_credentials_store = "file"
[mcp_servers.dcr]
url = %q
scopes = ["agent-context"]
[mcp_servers.dcr.oauth]
callback_url = "http://127.0.0.1:49152/callback"
callback_port = 49152
`, origin+"/mcp")
	if err := os.WriteFile(filepath.Join(directory, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "mcp", "login", "dcr", "--no-browser")
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.Env = []string{"CODEX_HOME=" + directory, "CODEX_CA_CERTIFICATE=" + ca, "SSL_CERT_FILE=" + ca, "BROWSER=/usr/bin/true"}
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG"} {
		if value := os.Getenv(key); value != "" {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	output := &test08AuthorizationOutput{origin: origin, ready: make(chan string, 1)}
	command.Stdout = output
	command.Stderr = output
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); cancel(); _ = command.Wait() }()
	select {
	case raw := <-output.ready:
		uri, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		query := uri.Query()
		if !strings.HasPrefix(query.Get("client_id"), "dcr_") || query.Get("code_challenge_method") != "S256" || query.Get("resource") != origin+"/mcp" {
			t.Fatal("CLI가 발급 ID·S256·고정 resource를 사용하지 않았습니다")
		}
		if err := service.ValidateAuthorizationTarget(t.Context(), query.Get("client_id"), query.Get("redirect_uri"), query.Get("resource")); err != nil {
			t.Fatal("CLI 등록 결합 불일치", err)
		}
	case <-ctx.Done():
		t.Fatal("실제 CLI가 DCR 등록 후 인가 URL을 생성하지 못했습니다")
	}
}
