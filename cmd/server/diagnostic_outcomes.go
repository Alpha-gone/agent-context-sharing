package main

import (
	"context"
	"errors"
	"net"
	"net/http"
)

type httpDiagnosticKey struct{}
type httpDiagnosticOutcome struct{ reason string }

// markHTTPReason은 고정된 내부 사유만 미들웨어 사이에 전달한다.
func markHTTPReason(request *http.Request, reason string) {
	if outcome, ok := request.Context().Value(httpDiagnosticKey{}).(*httpDiagnosticOutcome); ok {
		outcome.reason = reason
	}
}

func httpDiagnosticResult(ctx context.Context, status int, reason string) (result, code, level string) {
	if reason == "" {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			reason = "deadline_exceeded"
		case errors.Is(ctx.Err(), context.Canceled):
			reason = "canceled"
		}
	}
	switch reason {
	case "handler_panic", "internal_error":
		return "failed", reason, "ERROR"
	case "deadline_exceeded", "canceled":
		return "failed", reason, "INFO"
	case "https_required":
		return "rejected", reason, "WARN"
	}
	switch status {
	case http.StatusUnauthorized:
		return "rejected", "invalid_credential", "WARN"
	case http.StatusForbidden:
		return "rejected", "permission_denied", "WARN"
	case http.StatusNotFound:
		return "rejected", "not_found", "WARN"
	case http.StatusMethodNotAllowed:
		return "rejected", "method_not_allowed", "WARN"
	case http.StatusRequestEntityTooLarge:
		return "rejected", "payload_too_large", "WARN"
	case http.StatusTooManyRequests:
		return "rejected", "capacity_exceeded", "WARN"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "failed", "deadline_exceeded", "INFO"
	case http.StatusServiceUnavailable, http.StatusBadGateway:
		return "failed", "target_unavailable", "ERROR"
	}
	if status >= 500 {
		return "failed", "internal_error", "ERROR"
	}
	if status >= 400 {
		return "rejected", "invalid_argument", "WARN"
	}
	return "completed", "completed", "INFO"
}

func systemDiagnosticResult(err error, fallback string) (level, reason string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "INFO", "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "INFO", "canceled"
	default:
		if _, ok := errors.AsType[interface {
			error
			SQLState() string
		}](err); ok {
			return "ERROR", "database_unavailable"
		}
		if _, ok := errors.AsType[net.Error](err); ok && (fallback == "database_unavailable" || fallback == "embedding_schema_mismatch") {
			return "ERROR", "database_unavailable"
		}
		return "ERROR", fallback
	}
}
