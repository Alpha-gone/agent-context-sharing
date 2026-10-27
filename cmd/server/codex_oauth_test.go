package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
)

// 첫 인가의 verifier·코드·토큰을 시험이 대신 만들지 않는다. 실제 CLI와
// 브라우저가 교환하고 저장한 자격을 새 CLI 프로세스가 다시 사용해야 한다.
func TestTEST08CodexCLIOAuthIntegration(t *testing.T) {
	for _, scenario := range []string{"restart_and_revoke", "idle_expiry", "absolute_expiry", "refresh_response_loss", "concurrent_process_refresh", "uncertain_write"} {
		t.Run(scenario, func(t *testing.T) { test08CodexOAuth(t, scenario) })
	}
}

func test08CodexOAuth(t *testing.T, scenario string) {
	t.Helper()
	binary := test08CLI(t)
	chrome := os.Getenv("WEB_BROWSER_CHROME")
	if chrome == "" {
		chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	if _, err := os.Stat(chrome); err != nil {
		t.Fatal("TEST-08 OAuth 인수에는 Chrome이 필요합니다")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("TEST-08 OAuth 인수에는 Node.js가 필요합니다")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	database, pool, address := newServerContractDatabase(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic provider unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	server := httptest.NewUnstartedServer(nil)
	server.Config.ErrorLog = slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError)
	t.Cleanup(server.Close)
	certificate, rootPEM := test08Certificate(t)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	origin := "https://" + server.Listener.Addr().String()
	cfg := arch06Configuration(t, address, provider.URL)
	cfg.MCPResourceURL, _ = url.Parse(origin + "/mcp")
	cfg.AuthorizationServerURL, _ = url.Parse(origin)
	cfg.MCPAllowedOrigins = []string{origin}
	registered := cfg.OAuthClients["codex-cli-first-test"]
	registered.Resources = []string{origin + "/mcp"}
	cfg.OAuthClients["codex-cli-first-test"] = registered
	service, err := authz.New(database, authz.Config{Issuer: origin, BcryptCost: 4, OAuth: oauthConfiguration(cfg), AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database, Connections: database})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(ctx, "test08_oauth", "test08-oauth-password"); err != nil {
		t.Fatal(err)
	}
	assembly, err := assembleApplication(ctx, cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	assemblies := []*serverAssembly{assembly}
	t.Cleanup(func() {
		for _, built := range assemblies {
			if err := built.close(nil, true); err != nil {
				t.Error(err)
			}
		}
	})
	var calls atomic.Int64
	var primaryCalls, secondaryCalls atomic.Int64
	var exchanges atomic.Int64
	var unauthorized, unavailable, tokenRejected, tokenSucceeded atomic.Int64
	var dropTokenResponse atomic.Bool
	var dropWriteResponse atomic.Bool
	var simultaneous atomic.Bool
	var refreshWaiters atomic.Int64
	refreshArrived := make(chan struct{})
	script := new(test08Model)
	type target struct{ handler http.Handler }
	var active atomic.Pointer[target]
	var alternate atomic.Pointer[target]
	var routed atomic.Uint64
	var writeRequests atomic.Uint64
	active.Store(&target{assembly.app.handler()})
	newReplica := func() *serverAssembly {
		t.Helper()
		secondConfig := cfg
		secondConfig.Diagnostic.InstanceID = "test08-second"
		second, err := assembleApplication(ctx, secondConfig, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		assemblies = append(assemblies, second)
		return second
	}
	if scenario == "uncertain_write" {
		alternate.Store(&target{newReplica().app.handler()})
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := active.Load().handler
		secondary := false
		if other := alternate.Load(); other != nil {
			sequence := routed.Add(1)
			if scenario == "uncertain_write" && r.Header.Get("Mcp-Name") == "graph_update" {
				sequence = writeRequests.Add(1)
			}
			if sequence%2 == 0 {
				next, secondary = other.handler, true
			}
		}
		if r.URL.Path == "/mcp" {
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			if recorder.Code == http.StatusOK && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				calls.Add(1)
				if secondary {
					secondaryCalls.Add(1)
				} else {
					primaryCalls.Add(1)
				}
				script.observe(r.Header.Get("Mcp-Name"), recorder.Body.Bytes())
			}
			if recorder.Code == http.StatusUnauthorized {
				unauthorized.Add(1)
			}
			if recorder.Code == http.StatusInternalServerError {
				unavailable.Add(1)
			}
			if r.Header.Get("Mcp-Name") == "graph_update" && recorder.Code == http.StatusOK && dropWriteResponse.CompareAndSwap(true, false) {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error("쓰기 응답 유실 주입 실패")
					return
				}
				connection.Close()
				return
			}
			for key, values := range recorder.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			if simultaneous.Load() {
				if refreshWaiters.Add(1) == 2 {
					close(refreshArrived)
				}
				select {
				case <-refreshArrived:
				case <-time.After(time.Second):
				case <-r.Context().Done():
				}
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			if recorder.Code == http.StatusOK {
				tokenSucceeded.Add(1)
			} else {
				tokenRejected.Add(1)
			}
			if recorder.Code == http.StatusOK && dropTokenResponse.CompareAndSwap(true, false) {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error("갱신 응답 유실 주입 실패")
					return
				}
				connection.Close()
				return
			}
			for key, values := range recorder.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		next.ServeHTTP(w, r)
	})
	server.StartTLS()
	root := t.TempDir()
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, rootPEM, 0600); err != nil {
		t.Fatal(err)
	}
	inference := httptest.NewServer(http.HandlerFunc(script.serve))
	t.Cleanup(inference.Close)
	configuration := fmt.Sprintf(`model = "test08-local"
model_provider = "test08"
approval_policy = "never"
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
scopes = ["agent-context"]
required = true
startup_timeout_sec = 10
default_tools_approval_mode = "approve"
[mcp_servers.test08.oauth]
client_id = "codex-cli-first-test"
callback_url = "http://127.0.0.1:49152/callback"
callback_port = 49152
`, inference.URL, origin+"/mcp")
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	childEnv := []string{"CODEX_HOME=" + root, "CODEX_CA_CERTIFICATE=" + ca, "SSL_CERT_FILE=" + ca, "BROWSER=/usr/bin/true"}
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG"} {
		if value := os.Getenv(key); value != "" {
			childEnv = append(childEnv, key+"="+value)
		}
	}
	command := func(args ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, binary, args...)
		c.Dir, c.Env, c.WaitDelay = root, childEnv, 2*time.Second
		c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
		return c
	}
	login := command("mcp", "login", "test08", "--no-browser")
	output := &test08AuthorizationOutput{origin: origin, ready: make(chan string, 1)}
	login.Stdout, login.Stderr = output, output
	stdin, err := login.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := login.Start(); err != nil {
		t.Fatal("CLI OAuth 시작 실패")
	}
	finished := make(chan error, 1)
	go func() { finished <- login.Wait() }()
	waited := false
	defer func() {
		if !waited {
			_ = login.Process.Signal(os.Interrupt)
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				_ = login.Process.Kill()
				<-finished
			}
		}
	}()
	var authorization string
	select {
	case authorization = <-output.ready:
	case <-finished:
		waited = true
		t.Fatal("CLI OAuth가 인가 URL 없이 종료되었습니다")
	case <-ctx.Done():
		t.Fatal("CLI OAuth 인가 URL 대기 시간 초과")
	}
	uri, err := url.Parse(authorization)
	if err != nil || uri.Query().Get("client_id") != "codex-cli-first-test" || uri.Query().Get("redirect_uri") != "http://127.0.0.1:49152/callback" || uri.Query().Get("resource") != origin+"/mcp" || uri.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("CLI 인가 요청의 사전 등록·callback·resource·S256 결합 불일치")
	}
	listener, err := net.DialTimeout("tcp", "127.0.0.1:49152", time.Second)
	if err != nil {
		t.Fatal("실제 CLI의 고정 callback 수신을 확인하지 못했습니다")
	}
	listener.Close()
	spki := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	browserConfig, err := json.Marshal(map[string]any{"chrome": chrome, "profile": t.TempDir(), "spki": base64.StdEncoding.EncodeToString(spki[:]), "authorization": authorization, "origin": origin, "login": "test08_oauth", "password": "test08-oauth-password"})
	if err != nil {
		t.Fatal(err)
	}
	browser := exec.CommandContext(ctx, node, "testdata/codex-oauth.cjs")
	browser.Stdin = bytes.NewReader(browserConfig)
	browser.WaitDelay = 3 * time.Second
	browser.Cancel = func() error { return browser.Process.Signal(os.Interrupt) }
	var evidence bytes.Buffer
	browser.Stdout = &evidence
	if err := browser.Run(); err != nil {
		t.Fatal("실제 브라우저 OAuth 실패: ", evidence.String())
	}
	select {
	case err := <-finished:
		waited = true
		if err != nil {
			t.Fatal("브라우저 callback 뒤 CLI OAuth 교환·저장 실패")
		}
	case <-ctx.Done():
		t.Fatal("CLI OAuth callback 완료 대기 시간 초과")
	}
	var grants int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM oauth_grant WHERE client_id='codex-cli-first-test'").Scan(&grants); err != nil || grants != 1 || exchanges.Load() != 1 {
		t.Fatal("실제 CLI 최초 코드 교환의 단일 계열 저장 불일치")
	}
	credentials := filepath.Join(root, ".credentials.json")
	info, err := os.Stat(credentials)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("CLI 자격 파일의 존재·0600 보호를 확인하지 못했습니다")
	}
	before := calls.Load()
	_ = command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Return TEST08.").Run()
	if calls.Load() < before+2 || exchanges.Load() != 1 {
		t.Fatal("새 CLI 프로세스의 저장 자격 재사용을 확인하지 못했습니다")
	}
	script.mu.Lock()
	script.enabled = true
	script.replay = scenario == "uncertain_write"
	script.mu.Unlock()
	if scenario == "uncertain_write" {
		dropWriteResponse.Store(true)
	}
	if err := command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Run the TEST08 fixture tools.").Run(); err != nil {
		script.mu.Lock()
		t.Logf("script step=%d observed_calls=%d failure=%s catalog=%d names=%v", script.step, len(script.observed), script.failure, script.catalog, script.names)
		script.mu.Unlock()
		t.Fatal("실제 CLI의 합성 모델 구동 업무 도구 호출 실패")
	}
	script.mu.Lock()
	valid := script.step == 9 && len(script.observed) == 8 && script.version == 2 && script.failure == ""
	if scenario == "uncertain_write" {
		valid = script.step == 10 && len(script.observed) >= 9 && script.version == 2 && script.failure == "" && !dropWriteResponse.Load()
	}
	toolCalls := len(script.observed)
	script.enabled = false
	script.mu.Unlock()
	if !valid {
		t.Fatal("실제 CLI 등록·읽기·행위자 쓰기·현재 판 재조회 불일치")
	}
	if err := test08ExpireCredential(credentials); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	_ = command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Return TEST08.").Run()
	if calls.Load() < before+2 || exchanges.Load() != 2 {
		t.Fatal("클라이언트 만료 표본의 실제 갱신·자격 재사용 실패")
	}
	var refresh, consumed int
	if err := pool.QueryRow(ctx, "SELECT count(*), count(consumed_at) FROM oauth_refresh_token").Scan(&refresh, &consumed); err != nil || refresh != 2 || consumed != 1 {
		t.Fatal("실제 갱신의 단일 소비·후속 자격 저장 불일치")
	}
	probe := func() { _ = command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Return TEST08.").Run() }
	credentialsBefore, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal("시험 소유 자격 스냅숏 실패")
	}
	switch scenario {
	case "restart_and_revoke":
		address := server.Listener.Addr().String()
		handler := server.Config.Handler
		server.Close()
		if err := assembly.close(nil, true); err != nil {
			t.Fatal(err)
		}
		assembly, err = assembleApplication(ctx, cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		assemblies = append(assemblies, assembly)
		active.Store(&target{assembly.app.handler()})
		restarted := httptest.NewUnstartedServer(handler)
		restarted.Config.ErrorLog = slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError)
		restarted.Listener.Close()
		restarted.Listener, err = net.Listen("tcp", address)
		if err != nil {
			t.Fatal("동일 origin의 서버 재시작 실패")
		}
		restarted.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
		restarted.StartTLS()
		t.Cleanup(restarted.Close)
		before = calls.Load()
		probe()
		if calls.Load() < before+2 || exchanges.Load() != 2 {
			t.Fatal("서버 전체 교체 뒤 저장 자격 재사용 실패")
		}
		second := newReplica()
		alternate.Store(&target{second.app.handler()})
		before = calls.Load()
		beforePrimary, beforeSecondary := primaryCalls.Load(), secondaryCalls.Load()
		probe()
		if calls.Load() < before+2 || primaryCalls.Load() <= beforePrimary || secondaryCalls.Load() <= beforeSecondary || exchanges.Load() != 2 {
			t.Fatal("두 인스턴스 사이의 저장 자격 재사용 실패")
		}
		alternate.Store(nil)
		if err := second.close(nil, true); err != nil {
			t.Fatal(err)
		}
		if err := service.RotateSigningKey(ctx); err != nil {
			t.Fatal("시험 서명 키 회전 실패")
		}
		before = calls.Load()
		probe()
		if calls.Load() < before+2 || exchanges.Load() != 2 {
			t.Fatal("서명 키 회전 뒤 기존 접근 자격 사용 실패")
		}
		// 다른 인스턴스가 회전한 새 kid에는 명세의 1초 부재 재조회
		// 제한이 적용된다. 기존 키의 즉시 사용과 새 키 전파를 구분한다.
		select {
		case <-time.After(1100 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("키 전파 대기 취소")
		}
		if err := test08ExpireCredential(credentials); err != nil {
			t.Fatal(err)
		}
		probe()
		if exchanges.Load() != 3 || tokenSucceeded.Load() != 3 {
			t.Logf("key_rotation token_attempts=%d token_succeeded=%d token_rejected=%d mcp_401=%d mcp_500=%d", exchanges.Load(), tokenSucceeded.Load(), tokenRejected.Load(), unauthorized.Load(), unavailable.Load())
			t.Fatal("회전 키로 발급한 후속 자격 사용 실패")
		}
		// 실제 연결 풀을 닫아 저장 장애를 만든다. 재인가를 유도하거나
		// 호스트의 저장 자격을 지우지 않고 500으로 구분해야 한다.
		credentialsBefore, err = os.ReadFile(credentials)
		if err != nil {
			t.Fatal("시험 자격 스냅숏 실패")
		}
		assembly.database.Close()
		before = calls.Load()
		probe()
		after, err := os.ReadFile(credentials)
		if err != nil || !bytes.Equal(after, credentialsBefore) || calls.Load() != before || unavailable.Load() == 0 || exchanges.Load() != 3 {
			t.Fatal("저장 장애의 500 분류·자격 보존 불일치")
		}
		if err := assembly.close(nil, true); err != nil {
			t.Fatal(err)
		}
		assembly, err = assembleApplication(ctx, cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		assemblies = append(assemblies, assembly)
		active.Store(&target{assembly.app.handler()})
		probe()
		if calls.Load() < before+2 || exchanges.Load() != 3 {
			t.Fatal("저장소 복구 뒤 기존 자격 재사용 실패")
		}
		var accountID, grantID string
		if err := pool.QueryRow(ctx, "SELECT account_id::text,grant_id::text FROM public.oauth_grant").Scan(&accountID, &grantID); err != nil {
			t.Fatal("시험 연결 식별 실패")
		}
		owner, ownerErr := model.ParseID(accountID)
		grant, grantErr := model.ParseID(grantID)
		if ownerErr != nil || grantErr != nil || service.RevokeConnection(ctx, owner, "revoke_grant", grant) != nil {
			t.Fatal("본인 연결 관리의 실제 계열 폐기 실패")
		}
		before = calls.Load()
		probe()
		if calls.Load() != before || unauthorized.Load() == 0 {
			t.Fatal("폐기 뒤 기존 접근 자격이 차단되지 않았습니다")
		}
		if err := os.WriteFile(credentials, credentialsBefore, 0600); err != nil {
			t.Fatal("시험 소유 자격 복원 실패")
		}
		if err := test08ExpireCredential(credentials); err != nil {
			t.Fatal(err)
		}
		probe()
		if calls.Load() != before || tokenRejected.Load() == 0 || tokenSucceeded.Load() != 3 {
			t.Fatal("폐기 뒤 갱신 자격이 차단되지 않았습니다")
		}
	case "idle_expiry", "absolute_expiry":
		// 호스트와 서명된 접근 토큰의 시계는 바꾸지 않는다. 격리 DB의
		// 계열·갱신 시각을 같은 간격으로 이동해 7/30일 경계를 구성한다.
		days := 8
		if scenario == "absolute_expiry" {
			days = 31
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.WithoutCancel(ctx))
		if _, err := tx.Exec(ctx, `UPDATE public.oauth_grant SET authorized_at=authorized_at-make_interval(days=>$1),last_rotated_at=last_rotated_at-make_interval(days=>$1),absolute_expires_at=absolute_expires_at-make_interval(days=>$1),idle_expires_at=idle_expires_at-make_interval(days=>$1),retain_until=retain_until-make_interval(days=>$1)`, days); err != nil {
			t.Fatal("시험 계열 시각 이동 실패")
		}
		if _, err := tx.Exec(ctx, `UPDATE public.oauth_refresh_token SET issued_at=issued_at-make_interval(days=>$1),expires_at=expires_at-make_interval(days=>$1)`, days); err != nil || tx.Commit(ctx) != nil {
			t.Fatal("시험 갱신 시각 이동 실패")
		}
		var rotated, idle time.Time
		if err := pool.QueryRow(ctx, "SELECT last_rotated_at,idle_expires_at FROM public.oauth_grant").Scan(&rotated, &idle); err != nil {
			t.Fatal(err)
		}
		before = calls.Load()
		probe()
		if calls.Load() < before+2 || exchanges.Load() != 2 {
			t.Fatal("갱신 계열 만료가 유효 접근 자격의 수명을 줄였습니다")
		}
		if err := test08ExpireCredential(credentials); err != nil {
			t.Fatal(err)
		}
		before = calls.Load()
		probe()
		var unchanged bool
		if err := pool.QueryRow(ctx, "SELECT last_rotated_at=$1 AND idle_expires_at=$2 FROM public.oauth_grant", rotated, idle).Scan(&unchanged); err != nil || !unchanged || calls.Load() != before || tokenRejected.Load() == 0 || tokenSucceeded.Load() != 2 {
			t.Fatal("유휴/절대 만료 거부·비활동 시각 보존 불일치")
		}
	case "refresh_response_loss", "concurrent_process_refresh":
		if err := test08ExpireCredential(credentials); err != nil {
			t.Fatal(err)
		}
		if scenario == "refresh_response_loss" {
			dropTokenResponse.Store(true)
			probe()
			// 소비한 응답을 받지 못한 호스트의 이전 자격 재시도다.
			probe()
		} else {
			before = calls.Load()
			simultaneous.Store(true)
			var workers sync.WaitGroup
			for range 2 {
				workers.Go(probe)
			}
			workers.Wait()
		}
		var revoked bool
		var reason string
		if err := pool.QueryRow(ctx, "SELECT revoked_at IS NOT NULL,COALESCE(revocation_reason,'') FROM public.oauth_grant").Scan(&revoked, &reason); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*),count(consumed_at) FROM public.oauth_refresh_token").Scan(&refresh, &consumed); err != nil || refresh != 3 || consumed != 2 || tokenSucceeded.Load() != 3 {
			t.Fatal("응답 유실/동시 갱신의 단일 소비 불변식 불일치")
		}
		t.Logf("TEST-08 refresh boundary scenario=%s revoked=%t reason=%s token_attempts=%d token_rejected=%d", scenario, revoked, reason, exchanges.Load(), tokenRejected.Load())
		if revoked && reason != "refresh_token_reuse" {
			t.Fatal("재사용 외의 이유로 시험 계열이 폐기되었습니다")
		}
		if scenario == "refresh_response_loss" && (!revoked || tokenRejected.Load() == 0 || dropTokenResponse.Load()) {
			t.Fatal("갱신 응답 유실 뒤 이전 자격 재사용이 폐기되지 않았습니다")
		}
		if scenario == "concurrent_process_refresh" && (revoked || exchanges.Load() != 3 || tokenRejected.Load() != 0 || calls.Load() < before+4) {
			t.Fatal("동시 CLI 프로세스가 저장 자격 갱신을 직렬화하지 못했습니다")
		}
	case "uncertain_write":
		var version, records int
		if writeRequests.Load() < 2 || primaryCalls.Load() == 0 || secondaryCalls.Load() == 0 {
			t.Fatal("쓰기 응답 유실 뒤 다른 인스턴스의 재생을 확인하지 못했습니다")
		}
		if err := pool.QueryRow(ctx, "SELECT version FROM public.context_graph").Scan(&version); err != nil || version != 2 {
			t.Fatal("응답 유실 뒤 동일 키 재생이 그래프 판을 중복 적용했습니다")
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM public.idempotency_record WHERE tool_name='graph_update'").Scan(&records); err != nil || records != 1 {
			t.Fatal("응답 유실 뒤 최초 결과 보존 불일치")
		}
	}
	t.Logf("TEST-08 OAuth config=test08-oauth-v3 time=%s correlation=oauth-%s callback_listener=true browser=%s code_exchange=1 file_mode=0600 new_process_reuse=true tool_calls=%d graph_version=2 forced_client_expiry_refresh=1", time.Now().UTC().Format(time.RFC3339), scenario, strings.TrimSpace(evidence.String()), toolCalls)
}

type test08AuthorizationOutput struct {
	mu     sync.Mutex
	origin string
	data   []byte
	ready  chan string
}

func (output *test08AuthorizationOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if len(output.data)+len(data) <= 32<<10 {
		output.data = append(output.data, data...)
		complete, _, ok := strings.CutLast(string(output.data), "\n")
		if !ok {
			return len(data), nil
		}
		for field := range strings.FieldsSeq(complete) {
			uri, err := url.Parse(field)
			if err == nil && strings.HasPrefix(field, output.origin+"/authorize?") && uri.Path == "/authorize" {
				select {
				case output.ready <- field:
				default:
				}
			}
		}
	}
	return len(data), nil
}

// 실제 자격은 유지하고 시험 소유 파일의 클라이언트 만료 메타데이터만 지난
// 시각으로 바꾼다. 서버 접근 토큰의 자연 1시간 만료 시험과 구분한다.
func test08ExpireCredential(path string) error {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1<<20 {
		return fmt.Errorf("시험 소유 자격 파일 읽기 실패")
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return fmt.Errorf("시험 소유 자격 파일 해석 실패")
	}
	changed := 0
	var walk func(map[string]any)
	walk = func(object map[string]any) {
		for key, child := range object {
			if key == "expires_at" {
				switch child.(type) {
				case float64:
					object[key] = time.Now().Add(-time.Minute).Unix()
					changed++
				case string:
					object[key] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
					changed++
				}
			}
			if nested, ok := child.(map[string]any); ok {
				walk(nested)
			}
		}
	}
	walk(value)
	if changed != 1 {
		return fmt.Errorf("클라이언트 만료 메타데이터 필드 수 불일치: %d", changed)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("시험 자격 파일 인코딩 실패")
	}
	return os.WriteFile(path, encoded, 0600)
}
