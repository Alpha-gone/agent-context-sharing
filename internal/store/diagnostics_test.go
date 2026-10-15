package store

import (
	"context"
	"testing"
)

type pingDiagnostic struct {
	calls                 int
	stage, result, reason string
}

func (event *pingDiagnostic) RecordDiagnostic(_ context.Context, _, stage, result, reason string) bool {
	event.calls++
	event.stage, event.result, event.reason = stage, result, reason
	return false
}

func TestPingDiagnosticDropPreservesUninitializedError(t *testing.T) {
	database := &Store{}
	event := &pingDiagnostic{}
	database.SetDiagnostics(event)
	if err := database.Ping(t.Context()); err == nil || event.calls != 1 || event.stage != "database_ping" || event.result != "failed" || event.reason != "database_unavailable" {
		t.Fatalf("DB 없는 진단 주입 경계: %v %+v", err, event)
	}
}
