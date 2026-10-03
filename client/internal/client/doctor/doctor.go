// Package doctor는 MCP 도구와 분리된 순차 진단과 안전한 결과 출력을 소유한다.
package doctor

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/contract"
	"agent_context_sharing/client/internal/client/remote"
)

var names = []string{"configuration", "dns_tls", "protected_resource_metadata", "authorization_server_metadata", "browser_loopback", "authorization", "server_discover", "tools_list"}

// Check는 하나의 고정 진단 단계의 공개 결과다.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Action     string `json:"action,omitempty"`
}

// Report는 사람 읽기 출력과 JSON 출력이 공유하는 안정된 진단 외피다.
type Report struct {
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

// ExitCode는 성공·검사 실패·건너뜀을 구분한다. 사용법 오류 2는 명령 경계가 소유한다.
func (r Report) ExitCode() int {
	switch r.Status {
	case "pass":
		return 0
	case "fail":
		return 1
	default:
		return 3
	}
}

// Write는 진단 외피만 출력하고 내부 오류·URL·자격 증명을 출력하지 않는다.
func (r Report) Write(out io.Writer, asJSON bool) error {
	if asJSON {
		return json.MarshalWrite(out, r)
	}
	for _, check := range r.Checks {
		if _, err := fmt.Fprintf(out, "%s: %s (%dms)%s\n", check.Name, check.Status, check.DurationMS, actionSuffix(check.Action)); err != nil {
			return err
		}
	}
	return nil
}

func actionSuffix(action string) string {
	if action == "" {
		return ""
	}
	return "; " + action
}

// Options는 실제 진단에 사용하는 외부 경계를 주입한다. nil은 운영 기본값이다.
type Options struct {
	Authorization authorize.Options
	HTTP          remote.HTTPOptions
	Network       func(context.Context, string) error
}

// Run은 구성·연결·발견·브라우저·인가·원격 계약만 검사한다. 도구 호출 API는 사용하지 않는다.
func Run(ctx context.Context, getenv func(string) string, options Options) Report {
	var cfg config.Config
	var authentication *authorize.Manager
	var upstream *remote.Client
	var resource authorize.ResourceMetadata
	var configurationError error
	defer func() {
		if upstream != nil {
			upstream.Close()
		}
		if authentication != nil {
			authentication.Close()
		}
	}()
	if options.Network == nil {
		options.Network = func(ctx context.Context, target string) error {
			return checkNetwork(ctx, target, options.HTTP.Transport)
		}
	}
	checks := []func(context.Context) error{
		func(context.Context) error {
			var err error
			cfg, err = config.Load(getenv)
			if err != nil {
				configurationError = err
				return contract.ErrConfiguration
			}
			authentication, err = authorize.New(cfg, options.Authorization)
			if err != nil {
				return err
			}
			upstream, err = remote.NewHTTP(cfg, authentication, options.HTTP)
			return err
		},
		func(ctx context.Context) error { return options.Network(ctx, cfg.RemoteURL()) },
		func(ctx context.Context) error {
			var err error
			resource, err = authentication.CheckProtectedResource(ctx)
			return err
		},
		func(ctx context.Context) error { return authentication.CheckAuthorizationServer(ctx, resource) },
		func(ctx context.Context) error { return authentication.CheckBrowserLoopback(ctx) },
		func(ctx context.Context) error {
			_, err := authentication.Credentials(ctx, authorize.Challenge{})
			return err
		},
		func(ctx context.Context) error { _, err := upstream.Discover(ctx); return err },
		func(ctx context.Context) error { _, err := upstream.ListTools(ctx); return err },
	}
	report := evaluate(ctx, checks, func(index int) time.Duration {
		if index == 4 || index == 5 {
			return cfg.AuthTimeout()
		}
		return cfg.RequestTimeout()
	})
	if configurationError != nil {
		report.Checks[0].Action = configurationError.Error()
	}
	return report
}

func evaluate(ctx context.Context, steps []func(context.Context) error, timeout func(int) time.Duration) Report {
	report := Report{Status: "pass", Checks: make([]Check, 0, len(names))}
	blocked := false
	for index, name := range names {
		check := Check{Name: name, Status: "skipped"}
		if blocked {
			check.Action = "앞 단계의 문제를 해결한 뒤 진단을 다시 실행하십시오."
		} else if index >= len(steps) || steps[index] == nil {
			report.Status, blocked = "skipped", true
			check.Action = "이 실행 환경에서 진단을 수행할 수 없습니다."
		} else {
			started := time.Now()
			work, cancel := context.WithCancel(ctx)
			if duration := timeout(index); index != 0 && duration > 0 {
				cancel()
				work, cancel = context.WithTimeout(ctx, duration)
			}
			err := work.Err()
			if err == nil {
				err = steps[index](work)
			}
			if work.Err() != nil {
				err = work.Err()
			}
			cancel()
			check.DurationMS = time.Since(started).Milliseconds()
			check.Status = "pass"
			if err != nil {
				check.Status, report.Status, blocked = "fail", "fail", true
				check.Action = contract.ClassifyError(err).Message
			}
		}
		report.Checks = append(report.Checks, check)
	}
	return report
}

func checkNetwork(ctx context.Context, target string, transports ...http.RoundTripper) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return contract.ErrConfiguration
	}
	transport := http.DefaultTransport
	if len(transports) != 0 && transports[0] != nil {
		transport = transports[0]
	}
	if source, ok := transport.(*http.Transport); ok {
		owned := source.Clone()
		owned.DisableCompression = true
		defer owned.CloseIdleConnections()
		transport = owned
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return contract.ErrConfiguration
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return contract.ErrTransport
	}
	defer response.Body.Close()
	return nil
}
