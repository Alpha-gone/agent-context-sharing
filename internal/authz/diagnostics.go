package authz

import "context"

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
