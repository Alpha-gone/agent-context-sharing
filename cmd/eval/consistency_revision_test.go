package main

import (
	"context"
	"errors"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
)

func TestRevisionCandidateRetriesAndNeverPublishesChangedRevision(t *testing.T) {
	for _, test := range []struct {
		name        string
		changes     int
		wantRetries int
		exhausted   bool
	}{
		{"stable", 0, 0, false}, {"one_change", 1, 1, false}, {"limit", 3, 3, false}, {"exhausted", 4, 3, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var revision int64
			reads := 0
			attempt := consistencyAttempt{}
			err := readStableRevision(t.Context(), model.ID{}, func(context.Context, model.ID) (int64, error) { return revision, nil }, func(context.Context) error {
				reads++
				if reads <= test.changes {
					revision++
				}
				return nil
			}, &attempt)
			if errors.Is(err, errRevisionExhausted) != test.exhausted || attempt.Retries != test.wantRetries || attempt.Exhausted != test.exhausted || reads != test.wantRetries+1 {
				t.Fatalf("reads=%d attempt=%+v err=%v", reads, attempt, err)
			}
		})
	}
}

func TestRevisionCandidateStopsOnFailureOrCancellation(t *testing.T) {
	sentinel := errors.New("revision unavailable")
	for _, at := range []string{"before", "read", "after", "cancel"} {
		t.Run(at, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			attempt := consistencyAttempt{}
			err := readStableRevision(ctx, model.ID{}, func(context.Context, model.ID) (int64, error) {
				calls++
				if (at == "before" && calls == 1) || (at == "after" && calls == 2) {
					return 0, sentinel
				}
				return 0, nil
			}, func(context.Context) error {
				if at == "cancel" {
					cancel()
					return nil
				}
				if at == "read" {
					return sentinel
				}
				return nil
			}, &attempt)
			want := sentinel
			if at == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(err, want) || attempt.Exhausted || attempt.Retries != 0 {
				t.Fatalf("attempt=%+v err=%v", attempt, err)
			}
		})
	}
}

func TestRoutingSignalsRejectDriftAndFailures(t *testing.T) {
	for _, at := range []string{"same", "drift", "missing", "different_query", "failure"} {
		t.Run(at, func(t *testing.T) {
			signals := map[string]search.RouteSignals{"q": {SemanticTop: 0.8}}
			measured := measurement{Signals: map[string]search.RouteSignals{"q": {SemanticTop: 0.8}}}
			switch at {
			case "drift":
				measured.Signals["q"] = search.RouteSignals{SemanticTop: 0.9}
			case "missing":
				measured.Signals = nil
			case "different_query":
				measured.Signals = map[string]search.RouteSignals{"other": {SemanticTop: 0.8}}
			case "failure":
				measured.UseCases = map[string]useCaseMetrics{useCaseFact: {Failures: map[string]int{"keyword": 1}}}
			}
			if err := retainRouteSignals(signals, measured); (err != nil) != (at != "same") {
				t.Fatalf("%s: %v", at, err)
			}
		})
	}
}

func TestRoutingThresholdsDoNotApproveMissingQuality(t *testing.T) {
	if qualityNotWorse([]float64{-1, -1}, []float64{-1, -1}) {
		t.Fatal("미측정 품질을 비악화 근거로 승인했다")
	}
	if _, err := chooseThresholds([]thresholdResult{{Feasible: true}}); err == nil {
		t.Fatal("사용 사례 없는 임계값을 선택했다")
	}
}
