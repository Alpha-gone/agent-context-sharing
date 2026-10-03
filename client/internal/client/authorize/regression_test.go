package authorize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/contract"
)

func TestCallbackDoesNotWaitForOpenerExit(t *testing.T) {
	for _, lateError := range []bool{false, true} {
		t.Run(fmt.Sprint(lateError), func(t *testing.T) {
			f := newFixture(t)
			stopped := make(chan struct{})
			f.open = func(ctx context.Context, target string) error {
				defer close(stopped)
				if status, err := callbackRequest(callbackURL(target), ""); err != nil || status != http.StatusOK {
					t.Errorf("callback: %d %v", status, err)
					return ErrAuthorization
				}
				if lateError {
					return errors.New("실행기 종료 오류")
				}
				<-ctx.Done()
				return ctx.Err()
			}
			m := f.manager("2s")
			if _, err := m.Credentials(t.Context(), Challenge{}); err != nil {
				t.Fatal(err)
			}
			if f.exchanges.Load() != 1 {
				t.Fatal("정상 callback 뒤 코드가 정확히 한 번 교환되지 않았습니다")
			}
			select {
			case <-stopped:
			default:
				t.Fatal("opener 실행을 회수하지 않았습니다")
			}
			f.assertClosed()
		})
	}
}

func TestBlockedOpenerCancellationAndIdentity(t *testing.T) {
	for _, action := range []string{"cancel", "timeout", "close"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			started, stopped := make(chan struct{}), make(chan struct{})
			f.open = func(ctx context.Context, _ string) error {
				close(started)
				defer close(stopped)
				<-ctx.Done()
				return ctx.Err()
			}
			timeout := "5s"
			if action == "timeout" {
				timeout = "200ms"
			}
			m := f.manager(timeout)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := m.Credentials(ctx, Challenge{}); result <- err }()
			<-started
			lookupCtx, cancelLookup := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancelLookup()
			if _, err := m.Identity(lookupCtx); !errors.Is(err, ErrAuthorization) || f.opens.Load() != 1 {
				t.Fatal("주체 조회가 진행 중 인가를 기다렸거나 추가 인가를 시작했습니다")
			}
			if action == "cancel" {
				cancel()
			}
			if action == "close" {
				m.Close()
			}
			err := <-result
			if err == nil || action == "cancel" && !errors.Is(err, context.Canceled) || action == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("opener 대기 종료 결과: %v", err)
			}
			select {
			case <-stopped:
			default:
				t.Fatal("취소한 opener 실행을 회수하지 않았습니다")
			}
			f.assertClosed()
			if f.exchanges.Load() != 0 {
				t.Fatal("callback 없는 인가에서 토큰을 교환했습니다")
			}
		})
	}
}

func refreshHeader(raw string, exp int64) http.Header {
	return http.Header{"Mcp-Access-Token": {raw}, "Mcp-Access-Token-Expires-At": {fmt.Sprint(exp)}}
}

func TestRefreshNeverRegressesExpiration(t *testing.T) {
	f := newFixture(t)
	m := f.manager("5s")
	if _, err := m.Credentials(t.Context(), Challenge{}); err != nil {
		t.Fatal(err)
	}
	newExp := f.now.Unix() + 7200
	newest := f.token(m.jkt, f.subject, newExp)
	if err := m.Refresh(t.Context(), refreshHeader(newest, newExp)); err != nil {
		t.Fatal(err)
	}
	older := f.token(m.jkt, f.subject, newExp-3600)
	if err := m.Refresh(t.Context(), refreshHeader(older, newExp-3600)); err != nil || m.current.raw != newest {
		t.Fatal("늦게 도착한 오래된 토큰이 만료 시각을 앞당겼습니다")
	}
	// 오래된 응답도 서명·헤더·계정 검증을 생략하지 않는다.
	for _, invalid := range []struct {
		header http.Header
		want   error
	}{{refreshHeader("invalid", newExp-3600), contract.ErrProtocol}, {refreshHeader(older, newExp-1), contract.ErrProtocol}, {refreshHeader(f.token(m.jkt, "other-account", newExp-3600), newExp-3600), contract.ErrIdentityChanged}} {
		if err := m.Refresh(t.Context(), invalid.header); !errors.Is(err, invalid.want) || m.current.raw != newest {
			t.Fatal("오래된 갱신의 검증을 생략했거나 현재 토큰을 변경했습니다")
		}
	}
	equal := f.token(m.jkt, f.subject, newExp)
	if err := m.Refresh(t.Context(), refreshHeader(equal, newExp)); err != nil || m.current.raw != equal {
		t.Fatal("같은 만료 시각의 마지막 정상 갱신을 저장하지 않았습니다")
	}
	var wg sync.WaitGroup
	for index := range 100 {
		exp := newExp + int64(index+1)
		header := refreshHeader(f.token(m.jkt, f.subject, exp), exp)
		wg.Go(func() {
			if err := m.Refresh(t.Context(), header); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if m.current.expires.Unix() != newExp+100 {
		t.Fatal("동시 갱신에서 가장 늦은 만료 시각을 유지하지 않았습니다")
	}
}

func TestValidatedResourceUsedThroughoutAuthorization(t *testing.T) {
	f := newFixture(t)
	m := f.manager("5s")
	env := map[string]string{"AGENT_CONTEXT_CLIENT_REMOTE_URL": "https://RESOURCE.TEST:443/mcp", "AGENT_CONTEXT_CLIENT_ID": "client", "AGENT_CONTEXT_CLIENT_AGENT_ID": uuid.NewV7().String(), "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": "5s", "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "30s"}
	cfg, err := config.Load(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	m.cfg = cfg
	credential, err := m.Credentials(t.Context(), Challenge{})
	if err != nil {
		t.Fatal(err)
	}
	if m.metadata.resource != resourceURL || f.opens.Load() != 1 || f.exchanges.Load() != 1 {
		t.Fatal("검증한 보호 리소스 원문을 보존하지 않았습니다")
	}
	exp := f.now.Unix() + 7200
	if err := m.Refresh(t.Context(), refreshHeader(f.token(m.jkt, f.subject, exp), exp)); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, cfg.RemoteURL(), nil)
	if next, err := m.Authenticate(t.Context(), request); err != nil || next.identity != credential.identity {
		t.Fatal("구성 URL의 원격 요청에 검증한 자격 증명을 적용하지 못했습니다")
	}
}

func TestIdentityIsNonInteractiveAndExpiryHasLeeway(t *testing.T) {
	f := newFixture(t)
	m := f.manager("5s")
	if _, err := m.Identity(t.Context()); !errors.Is(err, ErrAuthorization) || f.opens.Load() != 0 {
		t.Fatal("토큰 없는 주체 조회가 인가를 시작했습니다")
	}
	first, err := m.Credentials(t.Context(), Challenge{})
	if err != nil {
		t.Fatal(err)
	}
	f.now = first.expires.Add(-expiryLeeway - time.Nanosecond)
	if identity, err := m.Identity(t.Context()); err != nil || identity != first.identity {
		t.Fatal("만료 여유 밖의 현재 주체를 반환하지 않았습니다")
	}
	if next, err := m.Credentials(t.Context(), Challenge{}); err != nil || next.raw != first.raw || f.opens.Load() != 1 {
		t.Fatal("만료 여유 밖에서 불필요한 인가를 시작했습니다")
	}
	f.now = first.expires.Add(-expiryLeeway)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if _, err := m.Identity(t.Context()); !errors.Is(err, ErrAuthorization) {
				t.Error("만료 여유 안의 주체를 사용했습니다")
			}
		})
	}
	wg.Wait()
	if f.opens.Load() != 1 {
		t.Fatal("주체 조회가 브라우저를 열었습니다")
	}
	if next, err := m.Credentials(t.Context(), Challenge{}); err != nil || next.raw == first.raw || next.identity != first.identity || f.opens.Load() != 2 {
		t.Fatal("만료 여유에서 자격 증명 준비가 새 인가를 시작하지 않았습니다")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Identity(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("주체 조회가 호출자 취소를 무시했습니다")
	}
	m.Close()
	if _, err := m.Identity(t.Context()); !errors.Is(err, ErrAuthorization) {
		t.Fatal("종료 뒤 주체를 반환했습니다")
	}
}

func TestApplyIsNonInteractive(t *testing.T) {
	f := newFixture(t)
	m := f.manager("5s")
	apply := func(ctx context.Context) (*http.Request, error) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, resourceURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.Apply(ctx, request)
		return request, err
	}
	if request, err := apply(t.Context()); !errors.Is(err, ErrAuthorization) || f.opens.Load() != 0 || len(request.Header) != 0 {
		t.Fatal("토큰 없는 적용이 인가를 시작했거나 헤더를 남겼습니다")
	}
	credential, err := m.Credentials(t.Context(), Challenge{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := apply(t.Context())
	if err != nil || first.Header.Get("Authorization") != "DPoP "+credential.raw || first.Header.Get("DPoP") == "" {
		t.Fatal("현재 자격 증명·proof를 적용하지 않았습니다")
	}
	second, err := apply(t.Context())
	if err != nil || second.Header.Get("DPoP") == first.Header.Get("DPoP") {
		t.Fatal("요청 적용에서 proof를 재사용했습니다")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if request, err := apply(canceled); !errors.Is(err, context.Canceled) || len(request.Header) != 0 {
		t.Fatal("취소한 요청에 자격 증명을 적용했습니다")
	}
	f.now = credential.expires.Add(-expiryLeeway)
	if request, err := apply(t.Context()); !errors.Is(err, ErrAuthorization) || len(request.Header) != 0 {
		t.Fatal("만료 여유 안의 토큰을 적용했습니다")
	}
	m.Close()
	if request, err := apply(t.Context()); !errors.Is(err, ErrAuthorization) || len(request.Header) != 0 || f.opens.Load() != 1 {
		t.Fatal("비대화형 적용이 종료 뒤 자격 증명을 적용하거나 인가를 시작했습니다")
	}
}

func TestTokensWithinExpiryLeewayAreRejected(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			f := newFixture(t)
			m := f.manager("5s")
			exp := f.now.Add(expiryLeeway).Unix()
			if initial {
				f.mutate = func(path string, body map[string]any) {
					if path == "/token" {
						body["access_token"] = f.token(m.jkt, f.subject, exp)
					}
				}
				if _, err := m.Credentials(t.Context(), Challenge{}); !errors.Is(err, contract.ErrProtocol) || m.current.raw != "" {
					t.Fatal("만료 직전의 초기 토큰을 저장했습니다")
				}
				return
			}
			current, err := m.Credentials(t.Context(), Challenge{})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Refresh(t.Context(), refreshHeader(f.token(m.jkt, f.subject, exp), exp)); !errors.Is(err, contract.ErrProtocol) || m.current.raw != current.raw {
				t.Fatal("만료 직전의 갱신 토큰을 허용했습니다")
			}
		})
	}
}

func TestLinuxBrowserCommandSelection(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		wsl     bool
		present []string
		want    string
	}{{"linux", false, []string{"xdg-open", "powershell.exe"}, "xdg-open"}, {"wslview", true, []string{"wslview", "powershell.exe"}, "wslview"}, {"powershell", true, []string{"powershell.exe"}, "powershell.exe"}, {"missing-wsl", true, nil, ""}, {"missing-linux", false, []string{"powershell.exe"}, ""}} {
		t.Run(scenario.name, func(t *testing.T) {
			// 따옴표 등이 포함된 URL도 고정 명령문에 보간하지 않는다.
			target := "https://issuer.test/authorize?state=';Write-Output%20secret;&code=secret"
			command, err := linuxBrowserCommand(t.Context(), target, scenario.wsl, func(name string) (string, error) {
				if slices.Contains(scenario.present, name) {
					return "/test/" + name, nil
				}
				return "", exec.ErrNotFound
			})
			if scenario.want == "" {
				if !errors.Is(err, ErrAuthorization) || command != nil {
					t.Fatal("실행 파일이 없는 환경을 허용했습니다")
				}
				return
			}
			if err != nil || command.Path != "/test/"+scenario.want {
				t.Fatal("브라우저 실행기 선택이 다릅니다")
			}
			if scenario.want == "powershell.exe" {
				stdin, err := io.ReadAll(command.Stdin)
				if err != nil || string(stdin) != target || strings.Contains(strings.Join(command.Args, " "), target) {
					t.Fatal("URL을 PowerShell 명령문에 보간했거나 표준 입력에서 손실했습니다")
				}
			} else if len(command.Args) != 2 || command.Args[1] != target {
				t.Fatal("URL을 독립된 실행 인자로 전달하지 않았습니다")
			}
		})
	}
	lookups := 0
	if command, err := linuxBrowserCommand(t.Context(), resourceURL, true, func(string) (string, error) {
		lookups++
		return "", os.ErrPermission
	}); !errors.Is(err, ErrAuthorization) || command != nil || lookups != 1 {
		t.Fatal("실행 권한 오류를 파일 부재로 취급해 대체 실행했습니다")
	}
}

func TestBrowserCommandProcess(t *testing.T) {
	if os.Getenv("CLIENT_TEST_BROWSER_PROCESS") == "1" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
}

func TestBrowserCommandCancellation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestBrowserCommandProcess$")
	command.Env = append(os.Environ(), "CLIENT_TEST_BROWSER_PROCESS=1")
	if err := runBrowserCommand(command); err == nil || ctx.Err() == nil || command.ProcessState == nil {
		t.Fatal("취소한 실행기를 회수하지 않았습니다")
	}
	if command.WaitDelay <= 0 || command.Stdout != io.Discard || command.Stderr != io.Discard {
		t.Fatal("출력 비노출이나 유한한 프로세스 대기 경계가 없습니다")
	}
}
