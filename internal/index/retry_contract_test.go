package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestEmbeddingRetryClassificationPreservesWrappedErrors(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		want  bool
	}{
		{"dimension", DimensionError{Got: 127, Want: 128}, false},
		{"input_contract", ProviderError{Reason: "invalid input"}, false},
		{"response_contract", ProviderError{Reason: "invalid response"}, false},
		{"transient_provider", ProviderError{Reason: "transient", Retryable: true}, true},
		{"network_read", io.ErrUnexpectedEOF, true},
		{"timeout", context.DeadlineExceeded, true},
		{"cancellation", context.Canceled, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, err := range []error{test.cause, fmt.Errorf("outer: %w", test.cause), errors.Join(errors.New("outer"), test.cause)} {
				if got := retryableEmbeddingError(err); got != test.want {
					t.Fatalf("retryable(%v)=%t, want=%t", err, got, test.want)
				}
			}
		})
	}
}

func TestProviderOutcomeFailureReasonsPreserveWrappedErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		cause  error
		reason string
		level  string
	}{
		{"dimension", DimensionError{Got: 127, Want: 128}, "dimension_mismatch", "ERROR"},
		{"input_contract", ProviderError{Reason: "invalid input"}, "provider_contract_violation", "ERROR"},
		{"response_contract", ProviderError{Reason: "invalid response"}, "provider_contract_violation", "ERROR"},
		{"transient_provider", ProviderError{Reason: "transient", Retryable: true}, "provider_unavailable", "WARN"},
		{"network_read", io.ErrUnexpectedEOF, "provider_unavailable", "WARN"},
		{"timeout", context.DeadlineExceeded, "provider_unavailable", "WARN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, err := range []error{test.cause, fmt.Errorf("outer: %w", test.cause), errors.Join(errors.New("outer"), test.cause)} {
				if result, reason, level := providerOutcome(t.Context(), err); result != "failed" || reason != test.reason || level != test.level {
					t.Fatalf("providerOutcome(%v)=%s/%s/%s, want=failed/%s/%s", err, result, reason, level, test.reason, test.level)
				}
			}
		})
	}
}
