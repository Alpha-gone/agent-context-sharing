// Package host는 호스트 측 MCP stdio 경계와 프로세스 수명주기를 소유한다.
package host

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const protocolVersion = "2026-07-28"

var hostProtocolVersions = []string{protocolVersion, "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Run은 단일 호스트 MCP 세션을 실행하고 EOF의 응답 기록과 요청별 취소를 처리한다.
func Run(ctx context.Context, in io.ReadCloser, out io.Writer, logger *slog.Logger, version string, tools ...*Tools) error {
	var relay *Tools
	if len(tools) != 0 {
		relay = tools[0]
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-context-client", Version: version}, &mcp.ServerOptions{
		SupportedProtocolVersions: hostProtocolVersions,
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{},
		},
		// SDK 진단에는 미검증 입력이 섞일 수 있으므로 공개 로그와 분리한다.
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	logger.Info("", "event", "host_start")
	err := server.Run(ctx, &stdioTransport{in: in, out: out, tools: relay, logger: logger, version: version})
	logger.Info("", "event", "host_stop")
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
