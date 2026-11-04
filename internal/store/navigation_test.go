package store

import (
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestNavigationEntryValidity(t *testing.T) {
	at := time.Now().UTC()
	base := model.Context{Layer: model.LayerDerived, Derived: &model.DerivedAttributes{Kind: model.DerivationKindSummary}}
	for _, sample := range []struct {
		name   string
		change func(*model.Context)
		want   bool
	}{
		{"active_summary", func(*model.Context) {}, true},
		{"starts_now", func(v *model.Context) { v.Derived.ValidFrom = new(at) }, true},
		{"future", func(v *model.Context) { v.Derived.ValidFrom = new(at.Add(time.Second)) }, false},
		{"expires_now", func(v *model.Context) { v.Derived.ValidTo = new(at) }, true},
		{"expired", func(v *model.Context) { v.Derived.ValidTo = new(at.Add(-time.Second)) }, false},
		{"discarded", func(v *model.Context) { v.DeletedAt = new(at) }, false},
		{"proposition", func(v *model.Context) { v.Derived.Kind = model.DerivationKindProposition }, false},
		{"source", func(v *model.Context) { v.Derived = nil; v.Layer = model.LayerSource }, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			value := base
			value.Derived = new(*base.Derived)
			sample.change(&value)
			if got := usableGraphEntry(value, at); got != sample.want {
				t.Fatalf("대표 유효성=%t, want=%t", got, sample.want)
			}
		})
	}
}

func TestNavigationInvariantEndpoints(t *testing.T) {
	for _, sample := range []struct {
		name, layer, kind, target string
		want                      int
	}{
		{"event_target", "derived", "summary", "event", 0},
		{"source_target", "derived", "summary", "source", 0},
		{"derived_target", "derived", "summary", "derived", 0},
		{"not_summary", "derived", "proposition", "event", 1},
		{"not_derived", "event", "", "source", 1},
	} {
		t.Run(sample.name, func(t *testing.T) {
			snapshot := auditSnapshot{graphID: "g", contexts: map[string]auditContext{
				"a": {id: "a", graphID: "g", layer: sample.layer, kind: sample.kind},
				"b": {id: "b", graphID: "g", layer: sample.target},
			}, edges: []auditEdge{{label: "NAVIGATES_TO", graphID: "g", from: "a", to: "b"}}}
			violations, err := checkEndpointLayer(snapshot)
			if err != nil || len(violations) != sample.want {
				t.Fatalf("탐색 끝점 감사=%+v/%v", violations, err)
			}
			if sample.want == 0 {
				snapshot.edges[0].to = "a"
				violations, err = checkEndpointLayer(snapshot)
				if err != nil || len(violations) != 1 {
					t.Fatalf("자기 탐색 감사=%+v/%v", violations, err)
				}
			}
		})
	}
}
