// Command client는 에이전트 호스트가 실행하는 MCP stdio 서비스를 제공한다.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"agent_context_sharing/client/internal/client"
	"agent_context_sharing/client/internal/client/config"
)

// releaseVersion은 공식 배포본에서 링크 시점에 주입한다.
var releaseVersion string

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
}

func run(ctx context.Context, args []string, getenv func(string) string, in io.ReadCloser, out, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			usage(stderr)
			return 2
		}
		if err := client.Serve(ctx, getenv, in, out, logger, buildVersion()); err != nil {
			logger.Error("", "event", "serve_failed")
			return 1
		}
		return 0
	case "doctor":
		if len(args) > 2 || len(args) == 2 && args[1] != "--json" {
			usage(stderr)
			return 2
		}
		if _, err := config.Load(getenv); err != nil {
			logger.Error("", "event", "configuration_rejected")
			return 1
		}
		// 진단 절차는 5단계에서 구현한다. 현재 결과를 성공으로 보고하지 않는다.
		if len(args) == 2 {
			fmt.Fprintln(out, `{"status":"skipped","checks":[{"name":"configuration","status":"pass"}]}`)
		} else {
			fmt.Fprintln(out, "configuration: pass; 나머지 진단: 미구현")
		}
		return 2
	default:
		usage(stderr)
		return 2
	}
}

func usage(stderr io.Writer) {
	fmt.Fprintln(stderr, "사용법: agent-context-client serve | doctor [--json]")
}

func buildVersion() string {
	if releaseVersion != "" {
		return releaseVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
