package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
)

type mcpAuthorization interface {
	ProtectBearer(func(http.ResponseWriter, *http.Request, authz.BearerPrincipal)) http.Handler
}

const mcpRequestTimeout = 30 * time.Second

// newMCPHandler는 패키지 의존을 늘리지 않고 Bearer 결과를 전송 경계에 전달한다.
// 전체 기동 조립 전에도 인증·업무·수신 본문의 시간 예산을 같은 경계에서 적용한다.
func newMCPHandler(authorization mcpAuthorization, origins []string, handlers map[string]mcp.ToolHandler) (http.Handler, error) {
	if authorization == nil {
		return nil, fmt.Errorf("MCP Bearer 인증 경계가 필요하다")
	}
	handler, err := mcp.NewHandler(mcp.HTTPConfig{
		AllowedOrigins: origins, Handlers: handlers,
		Protect: func(next mcp.AuthenticatedHandler) http.Handler {
			return authorization.ProtectBearer(func(w http.ResponseWriter, request *http.Request, principal authz.BearerPrincipal) {
				next(w, request, mcp.Principal{AccountID: principal.AccountID, ClientID: principal.ClientID})
			})
		},
	})
	if err != nil {
		return nil, err
	}
	return withMCPRequestBudget(handler, mcpRequestTimeout), nil
}

func withMCPRequestBudget(next http.Handler, budget time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), budget)
		defer cancel()
		deadline, _ := ctx.Deadline()
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			mcpBudgetFailure(w)
			return
		}
		defer controller.SetReadDeadline(time.Time{})
		response := &mcpBudgetResponse{ResponseWriter: w}
		next.ServeHTTP(response, request.WithContext(ctx))
		// SDK는 취소 때 응답 없이 반환할 수 있다. 이미 보낸 응답을 덧쓰지 않는다.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && !response.wrote.Load() {
			mcpBudgetFailure(w)
		}
	})
}

func mcpBudgetFailure(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(`{"error":"server_error"}`))
}

type mcpBudgetResponse struct {
	http.ResponseWriter
	wrote atomic.Bool
}

func (w *mcpBudgetResponse) WriteHeader(status int) {
	if status >= 200 {
		w.wrote.Store(true)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *mcpBudgetResponse) Write(data []byte) (int, error) {
	w.wrote.Store(true)
	return w.ResponseWriter.Write(data)
}

func (w *mcpBudgetResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
