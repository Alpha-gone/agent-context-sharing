package store

import (
	"testing"
	"time"
)

func TestIndexRetryDelayUsesCappedExponentialBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: time.Minute},
		{attempt: 2, want: 2 * time.Minute},
		{attempt: 3, want: 4 * time.Minute},
		{attempt: 4, want: 8 * time.Minute},
		{attempt: 5, want: 8 * time.Minute},
	}
	for _, test := range tests {
		if got := indexRetryDelay(test.attempt); got != test.want {
			t.Fatalf("재시도 간격(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}
