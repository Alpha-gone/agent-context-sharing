// Package client는 기동 구성 검증과 호스트 MCP 경계를 조립한다.
package client

import (
	"context"
	"io"
	"log/slog"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/host"
)

// Serve는 구성 오류를 외부 접근과 호스트 입력 수락 전에 거부한다.
func Serve(ctx context.Context, getenv func(string) string, in io.ReadCloser, out io.Writer, logger *slog.Logger, version string) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		// Load는 입력값 없이 환경 변수 이름과 고정 조치 문구만 반환한다.
		logger.Error("", "event", "configuration_rejected", "outcome", err.Error())
		return err
	}
	authentication, err := authorize.New(cfg, authorize.Options{})
	if err != nil {
		return err
	}
	defer authentication.Close()
	// 실제 보호 요청과의 연결은 remote HTTP 어댑터가 마련된 뒤 조립한다.
	return host.Run(ctx, in, out, logger, version)
}
