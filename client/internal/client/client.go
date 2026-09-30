// Package client는 기동 구성 검증과 호스트 MCP 경계를 조립한다.
package client

import (
	"context"
	"io"
	"log/slog"

	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/host"
)

// Serve는 구성 오류를 외부 접근과 호스트 입력 수락 전에 거부한다.
func Serve(ctx context.Context, getenv func(string) string, in io.ReadCloser, out io.Writer, logger *slog.Logger, version string) error {
	if _, err := config.Load(getenv); err != nil {
		return err
	}
	return host.Run(ctx, in, out, logger, version)
}
