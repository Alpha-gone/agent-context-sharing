package main

import (
	"fmt"
	"net/http"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
)

type mcpAuthorization interface {
	ProtectBearer(func(http.ResponseWriter, *http.Request, authz.BearerPrincipal)) http.Handler
}

// newMCPHandler는 패키지 의존을 늘리지 않고 Bearer 결과를 전송 경계에 전달한다.
// MCP-06·07에서 실제 14종 업무 처리기를 조립한 뒤 기동 경로에서 사용한다.
func newMCPHandler(authorization mcpAuthorization, origins []string, handlers map[string]mcp.ToolHandler) (http.Handler, error) {
	if authorization == nil {
		return nil, fmt.Errorf("MCP Bearer 인증 경계가 필요하다")
	}
	return mcp.NewHandler(mcp.HTTPConfig{
		AllowedOrigins: origins, Handlers: handlers,
		Protect: func(next mcp.AuthenticatedHandler) http.Handler {
			return authorization.ProtectBearer(func(w http.ResponseWriter, request *http.Request, principal authz.BearerPrincipal) {
				next(w, request, mcp.Principal{AccountID: principal.AccountID, ClientID: principal.ClientID})
			})
		},
	})
}
