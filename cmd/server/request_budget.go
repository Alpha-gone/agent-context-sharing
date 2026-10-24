package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"
)

// withRequestBudget는 수신·하위 호출에 같은 마감을 전달하고 채널별 오류를 유지한다.
func withRequestBudget(next http.Handler, budget time.Duration, failure func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), budget)
		defer cancel()
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				failure(w, request)
			}
			return
		}
		deadline, _ := ctx.Deadline()
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			failure(w, request)
			return
		}
		// 읽기 마감은 net/http가 응답 뒤 본문 정리와 다음 요청 경계까지 소유한다.
		// 여기서 지우면 미완성 본문의 자동 drain이 시간 제한 없이 기다릴 수 있다.
		response := &requestBudgetResponse{ResponseWriter: w}
		next.ServeHTTP(response, request.WithContext(ctx))
		// 소켓 마감이 context 타이머보다 먼저 깨어나는 경우도 같은 예산 소진이다.
		if !response.wrote.Load() && (errors.Is(ctx.Err(), context.DeadlineExceeded) || !time.Now().Before(deadline)) {
			failure(w, request)
		}
	})
}

func httpBudgetFailure(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/mcp" || request.URL.Path == "/token" {
		mcpBudgetFailure(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeStatus(w, http.StatusInternalServerError, "internal_error")
}

type requestBudgetResponse struct {
	http.ResponseWriter
	wrote atomic.Bool
}

func (w *requestBudgetResponse) WriteHeader(status int) {
	if status >= 200 {
		w.wrote.Store(true)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestBudgetResponse) Write(data []byte) (int, error) {
	w.wrote.Store(true)
	return w.ResponseWriter.Write(data)
}

func (w *requestBudgetResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
