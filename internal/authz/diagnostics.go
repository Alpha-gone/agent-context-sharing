package authz

import (
	"context"
	"errors"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// Diagnostics는 고정 코드만 받으며 기록 누락으로 업무 결과를 바꾸지 않는다.
// 요청 객체·본문·error·URL·HTTP 헤더·자격은 전달하지 않는다.
type Diagnostics interface {
	RecordDiagnostic(ctx context.Context, level, stage, result, reasonCode string) bool
}

// SetDiagnostics는 조립 시 기록기를 주입한다. 요청·작업자를 시작하기 전에만 호출한다.
// nil이면 진단 전달을 생략하며 기존 실행기의 동작을 유지한다.
func (s *Service) SetDiagnostics(diagnostics Diagnostics) {
	s.diagnostics = diagnostics
}

func (s *Service) authObservation(ctx context.Context, stage string, account model.ID, client string, grant model.ID, err error) {
	level, result, reason := "INFO", "completed", "completed"
	if err != nil {
		level, result, reason = "WARN", "failed", "invalid_credential"
		if e, ok := errors.AsType[*authDiagnosticFailure](err); ok {
			reason = e.reason
		}
		if e, ok := errors.AsType[*store.OAuthRejection](err); ok {
			reason = e.Reason
			if e.Binding.AccountID.IsV7() {
				account, client, grant = e.Binding.AccountID, e.Binding.ClientID, e.Binding.ID
			}
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		level, result, reason = "INFO", "failed", "deadline_exceeded"
	case errors.Is(ctx.Err(), context.Canceled):
		level, result, reason = "INFO", "failed", "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		level, result, reason = "INFO", "failed", "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		level, result, reason = "INFO", "failed", "canceled"
	case errors.Is(err, ErrUnavailable):
		level, reason = "ERROR", "authentication_unavailable"
	}
	observer, ok := s.diagnostics.(diagnostic.Observer)
	if !ok {
		if s.diagnostics != nil {
			s.diagnostics.RecordDiagnostic(ctx, level, stage, result, reason)
		}
		return
	}
	fields := diagnostic.Fields{}
	verified := account.IsV7()
	if verified {
		fields.Identity.AccountID = account.String()
		// 공개 client는 성공한 등록 대조 뒤에만 받으며 안전한 이름이 아니면 생략한다.
		if s.oauth != nil {
			if _, registered := s.oauth.Clients[client]; registered {
				fields.Identity.ClientID = client
			}
		}
		if grant.IsV7() {
			fields.Identity.GrantID = grant.String()
			trace, _ := diagnostic.TraceFromContext(ctx).WithVerifiedFlow(grant.String())
			ctx = diagnostic.WithTrace(ctx, trace)
		}
	}
	// client 이름 제한 때문에 이벤트 전체가 누락되지 않도록 선택 ID만 생략한다.
	check := fields
	if fields.Identity.ClientID != "" {
		check.Identity.ClientID = ""
		if !safeClient(fields.Identity.ClientID) {
			fields = check
		}
	}
	diagnostic.Observe(observer, ctx, level, stage, result, reason, verified, fields)
}
func safeClient(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// 공개 자격 오류의 의미·문자열을 유지하며 진단 원인만 덧붙인다.
type authDiagnosticFailure struct {
	cause  error
	reason string
}

func (e *authDiagnosticFailure) Error() string { return e.cause.Error() }
func (e *authDiagnosticFailure) Unwrap() error { return e.cause }
func authRejected(reason string) error         { return &authDiagnosticFailure{ErrInvalidCredential, reason} }
