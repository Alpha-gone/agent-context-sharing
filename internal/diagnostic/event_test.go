package diagnostic

import (
	"context"
	"testing"
	"time"
)

type eventSink struct {
	events []Event
	accept bool
}

func (sink *eventSink) Emit(_ context.Context, event Event) bool {
	sink.events = append(sink.events, event)
	return sink.accept
}

func TestReporterKindsAndIndependentLevel(t *testing.T) {
	for _, kind := range []Kind{Request, Auth, Search, Index, Management, System} {
		t.Run(string(kind), func(t *testing.T) {
			sink := &eventSink{accept: true}
			reporter, err := NewReporter(kind, sink)
			if err != nil {
				t.Fatal(err)
			}
			if !reporter.RecordDiagnostic(t.Context(), "ERROR", "write", "rolled_back", "constraint_violation") {
				t.Fatal("유효한 이벤트가 거부됐다")
			}
			event := sink.events[0]
			if event.Kind != kind || event.Level != "ERROR" || event.Time.Location() != time.UTC || event.Validate() != nil {
				t.Fatalf("종류·수준·UTC 보존: %+v", event)
			}
		})
	}
}

func TestReporterRejectsUnstructuredValuesAndReportsDrop(t *testing.T) {
	sink := &eventSink{}
	reporter, err := NewReporter(Request, sink)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "https://example.test/?token=secret", "database error: secret", "body\ntext", "자격", string(make([]byte, 65))} {
		if reporter.RecordDiagnostic(t.Context(), "WARN", "validation", "failed", value) {
			t.Fatalf("임의 문자열을 수락했다: %q", value)
		}
	}
	if len(sink.events) != 0 {
		t.Fatal("거부된 이벤트가 수신기에 전달됐다")
	}
	if reporter.RecordDiagnostic(t.Context(), "WARN", "validation", "failed", "invalid_argument") || len(sink.events) != 1 {
		t.Fatal("수신기 누락 결과를 보존하지 않았다")
	}
	if _, err := NewReporter("other", sink); err == nil {
		t.Fatal("미지원 종류를 수락했다")
	}
	if _, err := NewReporter(Request, nil); err == nil {
		t.Fatal("수신기 누락을 수락했다")
	}
}

func TestEventRequiresUTCAndKnownLevel(t *testing.T) {
	event := Event{Time: time.Now().UTC(), Kind: Management, Level: "INFO", Stage: "commit", Result: "unknown", ReasonCode: "commit_unknown"}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	event.Time = event.Time.In(time.FixedZone("UTC offset", 0))
	if event.Validate() == nil {
		t.Fatal("UTC 외의 Location을 허용했다")
	}
	event.Time = time.Now().UTC()
	event.Level = "FATAL"
	if event.Validate() == nil {
		t.Fatal("미지원 수준을 허용했다")
	}
}
