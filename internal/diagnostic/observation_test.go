package diagnostic

import (
	"context"
	"testing"
)

func TestOBS05ResumeJobAndLevelFilter(t *testing.T) {
	source := testSource(t)
	sink := &eventSink{accept: true}
	filtered, err := LevelSink(sink, "WARN")
	if err != nil {
		t.Fatal(err)
	}
	reporter, _ := NewReporter(System, filtered, source)
	trace := NewRequestTrace().WithTestRun(NewTestRun())
	ctx := WithTrace(t.Context(), trace)
	job := ResumeJob(ctx, trace.CorrelationID())
	if TraceFromContext(job).CorrelationID() != trace.CorrelationID() || TraceFromContext(job).TestRunID() != trace.TestRunID() || TraceFromContext(job).JobID() == "" {
		t.Fatal("저장 연결 복원 실패")
	}
	if !reporter.RecordDiagnostic(job, "INFO", "worker", "completed", "completed") || len(sink.events) != 0 {
		t.Fatal("수준 제외를 기록 실패로 처리")
	}
	if !reporter.RecordDiagnostic(job, "ERROR", "worker", "failed", "store_unavailable") || len(sink.events) != 1 {
		t.Fatal("오류 수준 누락")
	}
	independent := ResumeJob(ctx, "untrusted input")
	if TraceFromContext(independent).CorrelationID() != "" || TraceFromContext(independent).JobID() == TraceFromContext(job).JobID() {
		t.Fatal("잘못된 저장 연결을 신뢰")
	}
	if Observe(nil, context.Background(), "INFO", "worker", "completed", "completed", false, Fields{}) {
		t.Fatal("미주입 수락")
	}
}
