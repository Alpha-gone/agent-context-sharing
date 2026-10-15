package authz

import (
	"context"
	"errors"
	"testing"
)

type droppedDiagnostic struct{ result, reason string }

func (event *droppedDiagnostic) RecordDiagnostic(_ context.Context, _, _, result, reason string) bool {
	event.result, event.reason = result, reason
	return false
}

func TestVerifyDiagnosticDropPreservesCredentialError(t *testing.T) {
	event := &droppedDiagnostic{}
	service := &Service{}
	service.SetDiagnostics(event)
	_, _, err := service.Verify(t.Context(), "credential-raw-value", "web")
	if !errors.Is(err, ErrInvalidCredential) || event.result != "failed" || event.reason != "invalid_credential" {
		t.Fatalf("검증 오류와 고정 코드: %v %+v", err, event)
	}
}
