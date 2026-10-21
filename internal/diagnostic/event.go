// Package diagnostic는 업무 타입·DB·HTTP 객체와 독립적인 진단 이벤트 경계를 소유한다.
// 파일 준비·비동기 기록은 Files·Recorder가, 회전·보존은 후속 관측성 항목이 담당한다.
package diagnostic

import (
	"context"
	"fmt"
	"slices"
	"time"
)

type Kind string

const (
	Request    Kind = "request"
	Auth       Kind = "auth"
	Search     Kind = "search"
	Index      Kind = "index"
	Management Kind = "management"
	System     Kind = "system"
)

// Event는 호출자가 전달하는 안전한 공통 필드다. 인스턴스·기동 식별자와
// 파일 경로는 기록기 조립의 책임이며 호출자가 덮어쓰지 않는다.
type Event struct {
	Time             time.Time `json:"time"`
	Kind             Kind      `json:"kind"`
	Level            string    `json:"level"`
	InstanceID       string    `json:"instance_id"`
	BootID           string    `json:"boot_id"`
	CorrelationID    string    `json:"correlation_id,omitempty"`
	FlowID           string    `json:"flow_id,omitempty"`
	JobID            string    `json:"job_id,omitempty"`
	TestRunID        string    `json:"test_run_id,omitempty"`
	Stage            string    `json:"stage"`
	Result           string    `json:"result"`
	ReasonCode       string    `json:"reason_code"`
	IdentityVerified bool      `json:"identity_verified"`
	Fields           Fields    `json:"fields,omitzero"`
}

func (event Event) Validate() error {
	if event.Time.IsZero() || event.Time.Location() != time.UTC || event.Time.Year() < 1 || event.Time.Year() > 9999 {
		return fmt.Errorf("진단 시각은 UTC여야 한다")
	}
	if !validKind(event.Kind) || !slices.Contains([]string{"DEBUG", "INFO", "WARN", "ERROR"}, event.Level) {
		return fmt.Errorf("진단 종류 또는 수준이 올바르지 않다")
	}
	if !listed(event.Stage, stages(event.Kind)) || !listed(event.Result, results) || !listed(event.ReasonCode, reasons) {
		return fmt.Errorf("진단 단계·결과·원인은 고정 코드여야 한다")
	}
	if !validInstance(event.InstanceID) || !validID(event.BootID) ||
		(event.CorrelationID == "" && event.JobID == "") ||
		(event.Kind == Request && event.CorrelationID == "") {
		return fmt.Errorf("진단 실행·흐름 식별자가 필요하다")
	}
	for _, id := range []string{event.CorrelationID, event.FlowID, event.JobID, event.TestRunID} {
		if id != "" && !validID(id) {
			return fmt.Errorf("진단 흐름 식별자가 올바르지 않다")
		}
	}
	if event.FlowID != "" && !event.IdentityVerified {
		return fmt.Errorf("확인된 인가 흐름만 기록할 수 있다")
	}
	if !event.Fields.valid(event.Kind, event.IdentityVerified) {
		return fmt.Errorf("진단 종류의 허용 필드가 아니다")
	}
	return nil
}

// Filename은 심각도와 무관한 고정 basename만 반환한다.
func (kind Kind) Filename() (string, error) {
	if !validKind(kind) {
		return "", fmt.Errorf("진단 종류가 올바르지 않다")
	}
	return string(kind) + ".jsonl", nil
}

// Sink는 즉시 수락 여부를 반환한다. false는 진단 누락이며 업무 결과를 바꾸지 않는다.
// 구현은 요청 객체·goroutine을 보관하지 않고 이벤트 값만 수락해야 한다.
type Sink interface {
	Emit(context.Context, Event) bool
}

// Reporter는 종류를 고정하여 업무 패키지의 좁은 진단 인터페이스를 구현한다.
type Reporter struct {
	kind   Kind
	sink   Sink
	source Source
}

func NewReporter(kind Kind, sink Sink, source Source) (*Reporter, error) {
	if !validKind(kind) || sink == nil || !validInstance(source.instance) || !validID(source.boot) {
		return nil, fmt.Errorf("진단 종류와 이벤트 수신기가 필요하다")
	}
	return &Reporter{kind: kind, sink: sink, source: source}, nil
}

// RecordDiagnostic은 처리 경계의 허용된 고정 코드만 받으며 확인한 대상은 싣지 않는다.
// 호출자는 진입점에서 만든 요청 또는 작업 Trace를 ctx로 전달해야 한다.
func (reporter *Reporter) RecordDiagnostic(ctx context.Context, level, stage, result, reasonCode string) bool {
	return reporter.RecordFields(ctx, level, stage, result, reasonCode, false, Fields{})
}

// RecordFields는 확인한 대상만 값으로 받는다. verified는 전달한 모든 대상과
// flow_id의 확인을 뜻하며 UUID 형식 검사가 실제 인증·대상 조회를 대신하지 않는다.
// ctx에서는 Trace만 복사하고 요청 객체·취소 상태·임의 값을 이벤트에 보관하지 않는다.
func (reporter *Reporter) RecordFields(ctx context.Context, level, stage, result, reasonCode string, verified bool, fields Fields) bool {
	trace := TraceFromContext(ctx)
	event := Event{Time: time.Now().UTC(), Kind: reporter.kind, Level: level, Stage: stage, Result: result, ReasonCode: reasonCode,
		InstanceID: reporter.source.instance, BootID: reporter.source.boot, CorrelationID: trace.correlation,
		FlowID: trace.flow, JobID: trace.job, TestRunID: trace.testRun, IdentityVerified: verified, Fields: fields}
	if event.Validate() != nil {
		return false
	}
	return reporter.sink.Emit(ctx, event)
}

func validKind(kind Kind) bool {
	return slices.Contains([]Kind{Request, Auth, Search, Index, Management, System}, kind)
}
