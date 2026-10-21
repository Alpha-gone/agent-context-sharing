package diagnostic

import (
	"context"
	"fmt"
	"uuid"
)

// Source는 배포 슬롯과 한 번의 프로세스 기동을 식별한다. 같은 기동의
// 모든 Reporter에 같은 Source를 전달하고 재기동 시 새로 생성한다.
type Source struct{ instance, boot string }

func NewSource(instance string) (Source, error) {
	if !validInstance(instance) {
		return Source{}, fmt.Errorf("진단 인스턴스 식별자가 올바르지 않다")
	}
	return Source{instance: instance, boot: uuid.NewV7().String()}, nil
}

func (source Source) InstanceID() string { return source.instance }
func (source Source) BootID() string     { return source.boot }

// Trace에는 서버 발급 식별자만 둔다. HTTP 헤더·멱등성 request_id·토큰·해시를
// correlation_id로 가져오는 생성자는 제공하지 않는다.
type Trace struct{ correlation, flow, job, testRun string }
type TestRun struct{ id string }

func NewRequestTrace() Trace { return Trace{correlation: uuid.NewV7().String()} }
func NewTestRun() TestRun    { return TestRun{id: uuid.NewV7().String()} }

// NewJob는 원래 요청 연결을 보존한다. 요청 없는 회차는 Trace{}.NewJob()를 쓴다.
func (trace Trace) NewJob() Trace {
	trace.job = uuid.NewV7().String()
	return trace
}

func (trace Trace) WithTestRun(run TestRun) Trace {
	trace.testRun = run.id
	return trace
}

// WithVerifiedFlow는 서버가 이미 확인한 비밀이 아닌 계열 식별자만 받는다.
func (trace Trace) WithVerifiedFlow(id string) (Trace, error) {
	if !validID(id) {
		return Trace{}, fmt.Errorf("인가 흐름 식별자가 올바르지 않다")
	}
	trace.flow = id
	return trace, nil
}

func (trace Trace) CorrelationID() string { return trace.correlation }
func (trace Trace) JobID() string         { return trace.job }
func (trace Trace) TestRunID() string     { return trace.testRun }

type traceKey struct{}

func WithTrace(ctx context.Context, trace Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, trace)
}

func TraceFromContext(ctx context.Context) Trace {
	trace, _ := ctx.Value(traceKey{}).(Trace)
	return trace
}

func validID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value && id[6]>>4 == 7 && id[8]>>6 == 2
}

func validInstance(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}
