package model

import (
	"errors"
	"testing"
	"time"
)

func TestNavigationReferences(t *testing.T) {
	at := time.Now().UTC()
	self, target := newTestID(t, at), newTestID(t, at)
	for _, sample := range []struct {
		name string
		kind DerivationKind
		refs []ID
		fail bool
	}{
		{"omitted", DerivationKindProposition, nil, false},
		{"summary", DerivationKindSummary, []ID{target}, false},
		{"not_summary", DerivationKindProposition, []ID{target}, true},
		{"duplicate", DerivationKindSummary, []ID{target, target}, true},
		{"self", DerivationKindSummary, []ID{self}, true},
		{"invalid", DerivationKindSummary, []ID{{}}, true},
		{"over_limit", DerivationKindSummary, make([]ID, 101), true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			err := ValidateNavigationReferences(self, sample.kind, sample.refs)
			if sample.fail {
				if field, ok := errors.AsType[FieldError](err); !ok || field.Field != "navigation_refs" {
					t.Fatalf("탐색 입력 오류=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	refs := make([]ID, 100)
	for i := range refs {
		refs[i] = newTestID(t, at)
	}
	if err := ValidateNavigationReferences(self, DerivationKindSummary, refs); err != nil {
		t.Fatal(err)
	}
}

func TestNavigationImmutable(t *testing.T) {
	at := time.Now().UTC()
	previous := baseContext(t, at)
	previous.Layer = LayerDerived
	previous.Derived = &DerivedAttributes{Kind: DerivationKindSummary, SummaryScope: SummaryScopeLocal,
		EvidenceState: EvidenceStateObservation, DerivedFrom: []ID{newTestID(t, at)}, NavigationRefs: []ID{newTestID(t, at)}}
	next := previous
	next.Derived = new(*previous.Derived)
	next.Derived.NavigationRefs = nil
	next.Version++
	if field, ok := errors.AsType[FieldError](ValidateUpdate(previous, next)); !ok || field.Field != "navigation_refs" {
		t.Fatalf("탐색 참조 변경을 허용했다: %v", ValidateUpdate(previous, next))
	}
}
