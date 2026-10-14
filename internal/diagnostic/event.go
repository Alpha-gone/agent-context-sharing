// Package diagnostic는 업무 타입·DB·HTTP 객체와 독립적인 진단 이벤트 경계를 소유한다.
// 파일 준비·대기열·회전·보존은 관측성 개발 항목에서 구현한다.
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
	Time       time.Time
	Kind       Kind
	Level      string
	Stage      string
	Result     string
	ReasonCode string
}

func (event Event) Validate() error {
	if event.Time.IsZero() || event.Time.Location() != time.UTC {
		return fmt.Errorf("진단 시각은 UTC여야 한다")
	}
	if !validKind(event.Kind) || !slices.Contains([]string{"DEBUG", "INFO", "WARN", "ERROR"}, event.Level) {
		return fmt.Errorf("진단 종류 또는 수준이 올바르지 않다")
	}
	if !validCode(event.Stage) || !validCode(event.Result) || !validCode(event.ReasonCode) {
		return fmt.Errorf("진단 단계·결과·원인은 고정 코드여야 한다")
	}
	return nil
}

// Sink는 즉시 수락 여부를 반환한다. false는 진단 누락이며 업무 결과를 바꾸지 않는다.
// 구현은 요청 객체·goroutine을 보관하지 않고 이벤트 값만 수락해야 한다.
type Sink interface {
	Emit(context.Context, Event) bool
}

// Reporter는 종류를 고정하여 업무 패키지의 좁은 진단 인터페이스를 구현한다.
type Reporter struct {
	kind Kind
	sink Sink
}

func NewReporter(kind Kind, sink Sink) (*Reporter, error) {
	if !validKind(kind) || sink == nil {
		return nil, fmt.Errorf("진단 종류와 이벤트 수신기가 필요하다")
	}
	return &Reporter{kind: kind, sink: sink}, nil
}

// RecordDiagnostic의 문자열은 입력값·오류 원문으로 만들지 않고 처리 경계의
// 고정 코드로 전달한다. 허용된 모양도 민감정보 전달의 허가를 뜻하지 않는다.
func (reporter *Reporter) RecordDiagnostic(ctx context.Context, level, stage, result, reasonCode string) bool {
	event := Event{Time: time.Now().UTC(), Kind: reporter.kind, Level: level, Stage: stage, Result: result, ReasonCode: reasonCode}
	if event.Validate() != nil {
		return false
	}
	return reporter.sink.Emit(ctx, event)
}

func validKind(kind Kind) bool {
	return slices.Contains([]Kind{Request, Auth, Search, Index, Management, System}, kind)
}

func validCode(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}
