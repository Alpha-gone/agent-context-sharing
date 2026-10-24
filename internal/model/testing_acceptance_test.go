package model

import (
	"slices"
	"testing"
	"time"
)

func TestTEST02ReferenceCardinality(t *testing.T) {
	a := sourceContext(t, contractTime())
	b := sourceContext(t, contractTime())
	b.GraphID = a.GraphID
	derived := contractDerived(t)
	derived.GraphID = a.GraphID
	derived.Derived.DerivedFrom = []ID{a.ID, b.ID}
	event := eventContext(t, contractTime(), contractTime())
	event.GraphID = a.GraphID
	event.Event.MemberIDs = []ID{a.ID, b.ID}
	for _, sample := range []struct {
		name string
		refs []ID
		bad  bool
	}{
		{"empty", nil, true},
		{"one", []ID{a.ID}, false},
		{"two", []ID{a.ID, b.ID}, false},
		{"reversed", []ID{b.ID, a.ID}, false},
		{"duplicate", []ID{a.ID, a.ID}, true},
		{"invalid", []ID{a.ID, {}}, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			d, e := cloneContext(derived), cloneContext(event)
			d.Derived.DerivedFrom, e.Event.MemberIDs = slices.Clone(sample.refs), slices.Clone(sample.refs)
			for field, err := range map[string]error{"derived_from": d.Validate(), "member_refs": e.Validate()} {
				if sample.bad {
					assertField(t, err, field)
				} else if err != nil {
					t.Fatalf("%s: %v", field, err)
				}
			}
		})
	}
	for _, sample := range []struct {
		name    string
		sources []Context
	}{
		{"one_missing", []Context{a}},
		{"duplicate_replaces_missing", []Context{a, a}},
		{"unexpected_extra", []Context{a, b, a}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			assertField(t, ValidateDerivedSources(derived, sample.sources), "derived_from")
			assertField(t, ValidateEventMembers(event, sample.sources), "member_refs")
		})
	}
	if err := ValidateDerivedSources(derived, []Context{b, a}); err != nil {
		t.Fatalf("조회 순서가 달라도 근거 집합은 같다: %v", err)
	}
	if err := ValidateEventMembers(event, []Context{a, b}); err != nil {
		t.Fatal(err)
	}
}

func TestTEST02EventTimeBoundaries(t *testing.T) {
	start, end := contractTime(), contractTime().Add(time.Hour)
	for _, sample := range []struct {
		name string
		at   time.Time
		end  *time.Time
		bad  bool
	}{
		{"before_start", start.Add(-time.Nanosecond), new(end), true},
		{"at_start", start, new(end), false},
		{"at_end", end, new(end), false},
		{"after_end", end.Add(time.Nanosecond), new(end), true},
		{"zero_duration", start, new(start), false},
		{"ongoing_future", end.AddDate(10, 0, 0), nil, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			source := sourceContext(t, start)
			source.Source.OccurredAt = sample.at
			event := eventContext(t, start, start)
			event.GraphID, event.Event.MemberIDs, event.Event.End = source.GraphID, []ID{source.ID}, sample.end
			err := ValidateEventMembers(event, []Context{source})
			if sample.bad {
				assertField(t, err, "member_refs")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	// 파생 유효 구간은 사실의 유효성을 뜻하므로 사건 시간 범위로 제한하지 않는다.
	derived := contractDerived(t)
	derived.Derived.ValidFrom, derived.Derived.ValidTo = new(end.AddDate(1, 0, 0)), new(end.AddDate(2, 0, 0))
	event := eventContext(t, start, start)
	event.GraphID, event.Event.MemberIDs, event.Event.End = derived.GraphID, []ID{derived.ID}, new(start)
	if err := ValidateEventMembers(event, []Context{derived}); err != nil {
		t.Fatal(err)
	}
}
