package search

import (
	"context"

	"agent_context_sharing/internal/diagnostic"
)

// Diagnostics는 고정 코드만 받으며 기록 누락으로 업무 결과를 바꾸지 않는다.
// 요청 객체·본문·error·URL·HTTP 헤더·자격은 전달하지 않는다.
type Diagnostics interface {
	RecordDiagnostic(ctx context.Context, level, stage, result, reasonCode string) bool
}

// SetDiagnostics는 조립 시 기록기를 주입한다. 요청·작업자를 시작하기 전에만 호출한다.
// nil이면 진단 전달을 생략하며 기존 실행기의 동작을 유지한다.
func (service *Service) SetDiagnostics(diagnostics Diagnostics) {
	service.diagnostics = diagnostics
}

func (service *Service) observe(ctx context.Context, stage, result, reason string, fields diagnostic.SearchFields) {
	level := "INFO"
	if result == "failed" {
		level = "WARN"
	}
	if observer, ok := service.diagnostics.(diagnostic.Observer); ok {
		diagnostic.Observe(observer, ctx, level, stage, result, reason, false, diagnostic.Fields{Search: fields})
	} else if service.diagnostics != nil && stage == "flow" {
		service.diagnostics.RecordDiagnostic(ctx, level, stage, result, reason)
	}
}
func (service *Service) observeChannel(ctx context.Context, name string, metric Channel) {
	result, reason := "completed", "completed"
	if metric.Failure != "" {
		result, reason = "failed", "channel_unavailable"
		if metric.Failure == "disabled" || metric.Failure == "no_entry_points" {
			result, reason = "skipped", "completed"
		}
	}
	service.observe(ctx, "channel", result, reason, diagnostic.SearchFields{Channel: name, Candidates: uint64(metric.Candidates), LatencyMS: uint64(metric.Latency.Milliseconds())})
}
