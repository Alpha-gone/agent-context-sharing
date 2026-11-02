package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
)

// 브라우저·사용자 자격을 대신하지 않고 실제 CLI의 발견→DCR→인가 URL 생성을 검사한다.
func TestLocalhostClaudeCodeRegistrationIntegration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS script PTY를 사용하는 Claude Code 시험입니다")
	}
	binary := os.Getenv("TEST_CLAUDE_BINARY")
	if binary == "" {
		t.Skip("TEST_CLAUDE_BINARY가 없어 실제 Claude Code DCR handshake를 건너뜁니다")
	}
	version, err := exec.CommandContext(t.Context(), binary, "--version").Output()
	if err != nil {
		t.Fatal("Claude Code version 조회 실패")
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
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/script", "-q", "/dev/null", binary, "mcp", "login", "issue300", "--no-browser")
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.Env = []string{"CLAUDE_CONFIG_DIR=" + directory, "NODE_EXTRA_CA_CERTS=" + ca, "SSL_CERT_FILE=" + ca, "BROWSER=/usr/bin/true", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "NO_COLOR=1"}
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG"} {
		if value := os.Getenv(key); value != "" {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Dir = directory
	add := exec.CommandContext(ctx, binary, "mcp", "add", "--transport", "http", "--scope", "user", "--callback-port", "49153", "issue300", origin+"/mcp")
	add.Env = command.Env
	add.Dir = directory
	if _, err := add.CombinedOutput(); err != nil {
		t.Fatal("isolated Claude Code MCP configuration failed", err)
	}
	output := &localhostAuthorizationOutput{origin: origin, ready: make(chan string, 1)}
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
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	defer func() {
		_, _ = input.Write([]byte{3})
		_ = input.Close()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			cancel()
			<-finished
		}
	}()
	select {
	case raw := <-output.ready:
		uri, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		query := uri.Query()
		if !strings.HasPrefix(query.Get("client_id"), "dcr_") || query.Get("code_challenge_method") != "S256" || query.Get("resource") != origin+"/mcp" || query.Get("redirect_uri") != "http://localhost:49153/callback" {
			t.Fatal("CLI가 발급 ID·S256·고정 resource를 사용하지 않았습니다")
		}
		if err := service.ValidateAuthorizationTarget(t.Context(), query.Get("client_id"), query.Get("redirect_uri"), query.Get("resource")); err != nil {
			t.Fatal("CLI 등록 결합 불일치", err)
		}
	case <-ctx.Done():
		output.mu.Lock()
		diagnostic := regexp.MustCompile(`https?://[^\s]+`).ReplaceAllString(string(output.data), "[URL omitted]")
		output.mu.Unlock()
		t.Log(diagnostic)
		t.Fatal("실제 CLI가 DCR 등록 후 인가 URL을 생성하지 못했습니다")
	}
}

// Claude Code는 인증 URL을 OSC 8 hyperlink로 출력하므로 제어 문자를 경계로 읽는다.
type localhostAuthorizationOutput struct {
	mu     sync.Mutex
	origin string
	data   []byte
	ready  chan string
}

func (output *localhostAuthorizationOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if len(output.data)+len(data) > 32<<10 {
		return len(data), nil
	}
	output.data = append(output.data, data...)
	pattern := regexp.MustCompile(`(` + regexp.QuoteMeta(output.origin) + `/authorize\?[^\s\x1b\x07]+)[\s\x1b\x07]`)
	for _, match := range pattern.FindAllSubmatch(output.data, -1) {
		raw := string(match[1])
		uri, err := url.Parse(raw)
		if err == nil && uri.Query().Get("client_id") != "" && uri.Query().Get("code_challenge_method") == "S256" {
			select {
			case output.ready <- raw:
			default:
			}
		}
	}
	return len(data), nil
}
