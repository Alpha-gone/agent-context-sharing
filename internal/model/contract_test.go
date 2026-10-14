package model

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"
)

func contractTime() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }

func contractDerived(t *testing.T) Context {
	t.Helper()
	c := baseContext(t, contractTime())
	c.Layer = LayerDerived
	c.Derived = new(DerivedAttributes{Kind: DerivationKindProposition, EvidenceState: EvidenceStateObservation, DerivedFrom: []ID{newTestID(t, contractTime())}})
	return c
}

func contractGraph(t *testing.T) Graph {
	t.Helper()
	return Graph{ID: newTestID(t, contractTime()), Name: "그래프", CreatedBy: newTestID(t, contractTime()), CreatedAt: contractTime(), LastActivityAt: contractTime(), Version: 1}
}

func cloneContext(c Context) Context {
	if c.Source != nil {
		c.Source = new(*c.Source)
	}
	if c.Derived != nil {
		c.Derived = new(*c.Derived)
		c.Derived.DerivedFrom = slices.Clone(c.Derived.DerivedFrom)
	}
	if c.Event != nil {
		c.Event = new(*c.Event)
		c.Event.MemberIDs = slices.Clone(c.Event.MemberIDs)
	}
	return c
}

func assertField(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s 위반이 허용됐다", want)
	}
	field, ok := errors.AsType[FieldError](fmt.Errorf("검증 경계: %w", err))
	if !ok || field.Field != want {
		t.Fatalf("오류 필드 = %#v, %v; want %s", field, err, want)
	}
}

func TestARCH02UUIDContract(t *testing.T) {
	const canonical = "019289c4-1800-7000-8000-000000000001"
	id, err := ParseID(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{canonical, strings.ToUpper(canonical), strings.ReplaceAll(canonical, "-", "")} {
		parsed, err := ParseID(raw)
		if err != nil || parsed != id || parsed.String() != canonical {
			t.Fatalf("UUID 왕복 %q: %v %v", raw, parsed, err)
		}
	}
	for _, raw := range []string{"", "urn:uuid:" + canonical, "{" + canonical + "}", canonical[:35], strings.Replace(canonical, "7000", "4000", 1), strings.Replace(canonical, "8000", "c000", 1), strings.Replace(canonical, "019", "xyz", 1), "00000000-0000-0000-0000-000000000000"} {
		t.Run(raw, func(t *testing.T) { _, err := ParseID(raw); assertField(t, err, "id") })
	}
	at, err := id.Time()
	if err != nil || at.Location() != time.UTC || at.UnixMilli() != 0x019289c41800 {
		t.Fatalf("UUID 시각 %s %v", at, err)
	}
	if _, err := (ID{}).Time(); err == nil {
		t.Fatal("빈 ID의 시각이 허용됐다")
	}
	last := id
	for range 200 {
		next, err := NewID()
		if err != nil || !next.IsV7() || uuid.UUID(next).Compare(uuid.UUID(last)) <= 0 {
			t.Fatalf("UUIDv7 증가·유일성: %v %v", next, err)
		}
		last = next
	}
	_, err = ParseID(strings.Replace(canonical, "019", "xyz", 1))
	if errors.Unwrap(err) == nil {
		t.Fatal("표준 UUID 해석 원인이 사라졌다")
	}
}

func TestARCH02CommonAndLayerContract(t *testing.T) {
	cases := []struct {
		name, field string
		mutate      func(*Context)
	}{
		{"context_id", "context_id", func(c *Context) { c.ID = ID{} }},
		{"graph_id", "graph_id", func(c *Context) { c.GraphID = ID{} }},
		{"created_by", "created_by", func(c *Context) { c.CreatedBy = ID{} }},
		{"created_by_agent", "created_by_agent", func(c *Context) { c.CreatedByAgent = ID{} }},
		{"empty_body", "body", func(c *Context) { c.Body = "" }},
		{"invalid_utf8", "body", func(c *Context) { c.Body = string([]byte{0xff}) }},
		{"nul", "body", func(c *Context) { c.Body = "앞\x00뒤" }},
		{"missing_time", "recorded_at", func(c *Context) { c.RecordedAt = time.Time{} }},
		{"offset_time", "recorded_at", func(c *Context) { c.RecordedAt = c.RecordedAt.In(time.FixedZone("KST", 9*3600)) }},
		{"zero_version", "version", func(c *Context) { c.Version = 0 }},
		{"source_version", "version", func(c *Context) { c.Version = 2 }},
		{"deleted_time", "deleted_at", func(c *Context) { c.DeletedAt = new(time.Time{}) }},
		{"unknown_layer", "layer", func(c *Context) { c.Layer = "unknown" }},
		{"missing_source", "layer", func(c *Context) { c.Source = nil }},
		{"extra_layer", "layer", func(c *Context) { c.Event = new(EventAttributes{}) }},
		{"source_channel", "source_channel", func(c *Context) { c.Source.Reference.Channel = "unknown" }},
		{"source_scheme", "locator", func(c *Context) { c.Source.Reference.Locator = "relative/path" }},
		{"source_uri", "locator", func(c *Context) { c.Source.Reference.Locator = "web://host/%zz" }},
		{"source_uri_nul", "locator", func(c *Context) { c.Source.Reference.Locator = "web://host/\x00" }},
		{"source_time", "occurred_at", func(c *Context) { c.Source.OccurredAt = time.Time{} }},
		{"origin_kind", "origin_kind", func(c *Context) { c.Source.OriginKind = "unknown" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := sourceContext(t, contractTime())
			tc.mutate(&c)
			assertField(t, c.Validate(), tc.field)
		})
	}
	for _, channel := range []SourceChannel{SourceChannelConversation, SourceChannelFile, SourceChannelWeb, SourceChannelAPI, SourceChannelOther} {
		for _, origin := range []OriginKind{OriginKindUserUtterance, OriginKindAgentOutput, OriginKindExternalContent} {
			t.Run(string(channel)+"/"+string(origin), func(t *testing.T) {
				c := sourceContext(t, contractTime())
				c.Source.Reference.Channel = channel
				c.Source.OriginKind = origin
				if err := c.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	for _, kind := range []DerivationKind{DerivationKindProposition, DerivationKindSummary, DerivationKindReflection} {
		for _, state := range []EvidenceState{EvidenceStateObservation, EvidenceStateExperience, EvidenceStateOpinion} {
			t.Run(string(kind)+"/"+string(state), func(t *testing.T) {
				c := contractDerived(t)
				c.Derived.Kind = kind
				c.Derived.EvidenceState = state
				if kind == DerivationKindSummary {
					c.Derived.SummaryScope = SummaryScopeGlobal
				}
				if state == EvidenceStateOpinion {
					c.Derived.ConfidenceState = ConfidenceStateUncertain
				}
				if err := c.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestARCH02DerivedContract(t *testing.T) {
	cases := []struct {
		name, field string
		mutate      func(*Context)
	}{
		{"missing_references", "derived_from", func(c *Context) { c.Derived.DerivedFrom = nil }},
		{"invalid_reference", "derived_from", func(c *Context) { c.Derived.DerivedFrom = []ID{{}} }},
		{"self_reference", "derived_from", func(c *Context) { c.Derived.DerivedFrom = []ID{c.ID} }},
		{"kind", "derivation_kind", func(c *Context) { c.Derived.Kind = "unknown" }},
		{"summary_missing_scope", "summary_scope", func(c *Context) { c.Derived.Kind = DerivationKindSummary }},
		{"scope_without_summary", "summary_scope", func(c *Context) { c.Derived.SummaryScope = SummaryScopeLocal }},
		{"evidence", "evidence_state", func(c *Context) { c.Derived.EvidenceState = "unknown" }},
		{"opinion_missing_confidence", "confidence_state", func(c *Context) { c.Derived.EvidenceState = EvidenceStateOpinion }},
		{"confidence_without_opinion", "confidence_state", func(c *Context) { c.Derived.ConfidenceState = ConfidenceStateSupported }},
		{"from_not_utc", "valid_from", func(c *Context) { c.Derived.ValidFrom = new(time.Time{}) }},
		{"reverse_validity", "valid_to", func(c *Context) {
			c.Derived.ValidFrom = new(contractTime())
			c.Derived.ValidTo = new(contractTime().Add(-time.Second))
		}},
		{"invalid_supersedes", "supersedes", func(c *Context) { c.Derived.Supersedes = new(ID{}) }},
		{"self_supersedes", "supersedes", func(c *Context) { c.Derived.Supersedes = new(c.ID) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { c := contractDerived(t); tc.mutate(&c); assertField(t, c.Validate(), tc.field) })
	}
	c := contractDerived(t)
	c.Derived.DerivedFrom = nil
	if err := c.ValidateProperties(); err != nil {
		t.Fatal(err)
	}
	assertField(t, c.Validate(), "derived_from")
	c = contractDerived(t)
	c.Derived.ValidFrom = new(contractTime())
	c.Derived.ValidTo = new(contractTime())
	if err := c.Validate(); err != nil {
		t.Fatalf("유효 시간 경계 같음: %v", err)
	}
	for _, confidence := range []ConfidenceState{ConfidenceStateSupported, ConfidenceStateUncertain, ConfidenceStateDisputed} {
		c.Derived.EvidenceState = EvidenceStateOpinion
		c.Derived.ConfidenceState = confidence
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestARCH02UpdateContract(t *testing.T) {
	common := []struct {
		name   string
		mutate func(*Context)
	}{
		{"id", func(c *Context) { c.ID = newTestID(t, contractTime()) }},
		{"graph", func(c *Context) { c.GraphID = newTestID(t, contractTime()) }},
		{"recorded", func(c *Context) { c.RecordedAt = c.RecordedAt.Add(time.Second) }},
		{"account", func(c *Context) { c.CreatedBy = newTestID(t, contractTime()) }},
		{"agent", func(c *Context) { c.CreatedByAgent = newTestID(t, contractTime()) }},
	}
	for _, layer := range []Layer{LayerSource, LayerDerived, LayerEvent} {
		var previous Context
		switch layer {
		case LayerSource:
			previous = sourceContext(t, contractTime())
		case LayerDerived:
			previous = contractDerived(t)
		case LayerEvent:
			previous = eventContext(t, contractTime(), contractTime())
		}
		for _, tc := range common {
			t.Run(string(layer)+"/"+tc.name, func(t *testing.T) {
				next := cloneContext(previous)
				if layer != LayerSource {
					next.Version++
				}
				tc.mutate(&next)
				assertField(t, ValidateUpdate(previous, next), "context_id")
			})
		}
		if layer != LayerSource {
			next := cloneContext(previous)
			next.Body = "새 본문"
			next.Version++
			if err := ValidateUpdate(previous, next); err != nil {
				t.Fatal(err)
			}
			next.Version = previous.Version
			assertField(t, ValidateUpdate(previous, next), "expected_version")
		}
	}
	source := sourceContext(t, contractTime())
	for _, change := range []func(*Context){func(c *Context) { c.Body = "수정" }, func(c *Context) { c.Source.OriginKind = OriginKindAgentOutput }, func(c *Context) { c.Source.Reference.Locator = "file://new" }, func(c *Context) { c.Source.OccurredAt = c.Source.OccurredAt.Add(time.Second) }} {
		next := cloneContext(source)
		change(&next)
		assertField(t, ValidateUpdate(source, next), "layer")
	}
	next := cloneContext(source)
	next.DeletedAt = new(contractTime())
	if err := ValidateUpdate(source, next); err != nil {
		t.Fatal(err)
	}
	derived := contractDerived(t)
	derived.Derived.DerivedFrom = append(derived.Derived.DerivedFrom, newTestID(t, contractTime()))
	next = cloneContext(derived)
	next.Version++
	slices.Reverse(next.Derived.DerivedFrom)
	if err := ValidateUpdate(derived, next); err != nil {
		t.Fatalf("근거 순서는 정체성을 바꾸지 않는다: %v", err)
	}
	next.Derived.DerivedFrom[0] = newTestID(t, contractTime())
	assertField(t, ValidateUpdate(derived, next), "derived_from")
	next = cloneContext(derived)
	next.Version++
	next.Derived.EvidenceState = EvidenceStateExperience
	assertField(t, ValidateUpdate(derived, next), "derivation_kind")
	next = cloneContext(derived)
	next.Version++
	next.Derived.EvidenceInvalidated = true
	if err := ValidateEvidenceInvalidation(derived, next); err != nil {
		t.Fatal(err)
	}
	next.Derived.DerivedFrom[0] = newTestID(t, contractTime())
	assertField(t, ValidateEvidenceInvalidation(derived, next), "derived_from")
}

func TestARCH02ResolvedReferences(t *testing.T) {
	source := sourceContext(t, contractTime())
	derived := contractDerived(t)
	derived.GraphID = source.GraphID
	derived.Derived.DerivedFrom = []ID{source.ID}
	if err := ValidateDerivedSources(derived, []Context{source}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "different_id", "other_graph", "event", "deleted"} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneContext(source)
			sources := []Context{candidate}
			switch name {
			case "missing":
				sources = nil
			case "different_id":
				sources[0].ID = newTestID(t, contractTime())
			case "other_graph":
				sources[0].GraphID = newTestID(t, contractTime())
			case "event":
				sources[0] = eventContext(t, contractTime(), contractTime())
				sources[0].GraphID = derived.GraphID
			case "deleted":
				sources[0].DeletedAt = new(contractTime())
			}
			assertField(t, ValidateDerivedSources(derived, sources), "derived_from")
		})
	}
	next := cloneContext(derived)
	next.ID = newTestID(t, contractTime())
	next.Derived.Supersedes = new(derived.ID)
	if err := ValidateSupersedes(next, derived); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "wrong_id", "other_graph", "source", "different_kind"} {
		t.Run("supersedes/"+name, func(t *testing.T) {
			before := cloneContext(derived)
			after := cloneContext(next)
			switch name {
			case "missing":
				after.Derived.Supersedes = nil
			case "wrong_id":
				after.Derived.Supersedes = new(newTestID(t, contractTime()))
			case "other_graph":
				before.GraphID = newTestID(t, contractTime())
			case "source":
				before = source
			case "different_kind":
				before.Derived.Kind = DerivationKindReflection
			}
			assertField(t, ValidateSupersedes(after, before), "supersedes")
		})
	}
	event := eventContext(t, contractTime(), contractTime())
	event.GraphID = source.GraphID
	event.Event.MemberIDs = []ID{source.ID}
	event.Event.End = new(contractTime())
	if err := ValidateEventMembers(event, []Context{source}); err != nil {
		t.Fatal(err)
	}
	event.Event.MemberIDs = nil
	assertField(t, event.Validate(), "member_refs")
	event.Event.MemberIDs = []ID{source.ID}
	event.Event.Start = contractTime().Add(time.Second)
	event.Event.End = nil
	assertField(t, ValidateEventMembers(event, []Context{source}), "member_refs")
	event.Event.Start = contractTime()
	event.Event.End = new(contractTime().Add(-time.Second))
	assertField(t, event.Validate(), "end")
	event.Event.End = new(time.Time{})
	assertField(t, event.Validate(), "start")
}

func TestARCH02GraphContract(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Graph)
	}{
		{"empty", func(g *Graph) { g.Name = "" }}, {"space", func(g *Graph) { g.Name = " \t\n" }},
		{"name_nul", func(g *Graph) { g.Name = "a\x00" }}, {"description_utf8", func(g *Graph) { g.Description = string([]byte{0xff}) }},
		{"graph_id", func(g *Graph) { g.ID = ID{} }}, {"creator", func(g *Graph) { g.CreatedBy = ID{} }},
		{"created_time", func(g *Graph) { g.CreatedAt = time.Time{} }}, {"activity_time", func(g *Graph) { g.LastActivityAt = time.Time{} }},
		{"negative_size", func(g *Graph) { g.StoredChars = -1 }}, {"zero_version", func(g *Graph) { g.Version = 0 }},
		{"deleted", func(g *Graph) { g.DeletedAt = new(time.Time{}) }}, {"grace", func(g *Graph) { g.GraceStartedAt = new(time.Time{}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := contractGraph(t)
			tc.mutate(&g)
			if err := g.Validate(); err == nil {
				t.Fatal("잘못된 그래프가 허용됐다")
			}
		})
	}
	before := contractGraph(t)
	after := before
	after.Name = "변경"
	after.Description = "설명"
	after.Version++
	if err := ValidateGraphUpdate(before, after); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Graph){func(g *Graph) { g.ID = newTestID(t, contractTime()) }, func(g *Graph) { g.CreatedAt = g.CreatedAt.Add(time.Second) }, func(g *Graph) { g.LastActivityAt = g.LastActivityAt.Add(time.Second) }, func(g *Graph) { g.StoredChars++ }, func(g *Graph) { g.DeletedAt = new(contractTime()) }, func(g *Graph) { g.GraceStartedAt = new(contractTime()) }, func(g *Graph) { g.Version = before.Version }} {
		candidate := after
		mutate(&candidate)
		if err := ValidateGraphUpdate(before, candidate); err == nil {
			t.Fatal("그래프 정보 수정에서 불변·시스템 속성 변경이 허용됐다")
		}
	}
	for _, grade := range []GraphGrade{GraphGradeOwner, GraphGradeEditor, GraphGradeViewer} {
		if !grade.Valid() {
			t.Fatal(grade)
		}
	}
	for _, grade := range []GraphGrade{"", "admin", "Owner"} {
		if grade.Valid() {
			t.Fatal(grade)
		}
	}
}

func contractRelation(t *testing.T) (Relation, Context, Context) {
	t.Helper()
	from := eventContext(t, contractTime(), contractTime())
	to := eventContext(t, contractTime(), contractTime().Add(time.Hour))
	to.GraphID = from.GraphID
	r := Relation{ID: newTestID(t, contractTime()), GraphID: from.GraphID, Type: RelationTypePrecedes, FromContextID: from.ID, ToContextID: to.ID, State: RelationStateConfirmed, ProposedBy: ProposalSourceAgent, ProposedAt: contractTime(), ConfirmedBy: newTestID(t, contractTime()), ConfirmedByAgent: newTestID(t, contractTime()), ConfirmedAt: new(contractTime())}
	return r, from, to
}

func TestARCH02RelationContract(t *testing.T) {
	r, from, to := contractRelation(t)
	if err := ValidateRelation(r, from, to); err != nil {
		t.Fatal(err)
	}
	assertField(t, ValidateRelationCycle(r, true), "relation_type")
	if err := ValidateRelationCycle(r, false); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []RelationType{RelationTypePrecedes, RelationTypeCauses} {
		t.Run(string(kind), func(t *testing.T) {
			a, b := from.Event, to.Event
			if err := ValidateRelationTime(kind, *a, *b); err != nil {
				t.Fatal(err)
			}
			assertField(t, ValidateRelationTime(kind, *b, *a), "from_context_id")
		})
	}
	part := EventAttributes{Start: contractTime().Add(time.Minute), End: new(contractTime().Add(2 * time.Minute))}
	whole := EventAttributes{Start: contractTime(), End: new(contractTime().Add(time.Hour))}
	if err := ValidateRelationTime(RelationTypePartOf, part, whole); err != nil {
		t.Fatal(err)
	}
	part.End = nil
	assertField(t, ValidateRelationTime(RelationTypePartOf, part, whole), "to_context_id")
	whole.End = nil
	if err := ValidateRelationTime(RelationTypePartOf, part, whole); err != nil {
		t.Fatal(err)
	}
	assertField(t, ValidateRelationTime("unknown", part, whole), "relation_type")
	cases := []struct {
		name   string
		mutate func(*Relation)
	}{
		{"id", func(r *Relation) { r.ID = ID{} }}, {"graph", func(r *Relation) { r.GraphID = ID{} }},
		{"self", func(r *Relation) { r.ToContextID = r.FromContextID }}, {"type", func(r *Relation) { r.Type = "unknown" }},
		{"state", func(r *Relation) { r.State = "unknown" }}, {"proposal", func(r *Relation) { r.ProposedBy = "unknown" }},
		{"proposed_time", func(r *Relation) { r.ProposedAt = time.Time{} }}, {"confirmation", func(r *Relation) { r.ConfirmedAt = nil }},
		{"deleted_in_confirmed", func(r *Relation) { r.DeletedAt = new(contractTime()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := r
			tc.mutate(&bad)
			if err := bad.Validate(); err == nil {
				t.Fatal("관계 위반이 허용됐다")
			}
		})
	}
	r.State = RelationStateDiscarded
	r.DeletedAt = new(contractTime())
	if err := r.Validate(); err != nil {
		t.Fatalf("폐기 시 확정 이력 보존: %v", err)
	}
	r.State = RelationStateProposed
	r.DeletedAt = nil
	assertField(t, r.Validate(), "state")
	r.ConfirmedBy = ID{}
	r.ConfirmedByAgent = ID{}
	r.ConfirmedAt = nil
	r.Type = RelationTypeRelatesTo
	if r.FromContextID.Compare(r.ToContextID) > 0 {
		r.FromContextID, r.ToContextID = r.ToContextID, r.FromContextID
	}
	if err := ValidateRelationCycle(r, true); err != nil {
		t.Fatalf("대칭 관계는 순환을 허용한다: %v", err)
	}
	r.FromContextID, r.ToContextID = r.ToContextID, r.FromContextID
	assertField(t, r.Validate(), "from_context_id")
	r, from, to = contractRelation(t)
	to.GraphID = newTestID(t, contractTime())
	assertField(t, ValidateRelation(r, from, to), "from_context_id")
	to.GraphID = r.GraphID
	to.DeletedAt = new(contractTime())
	assertField(t, ValidateRelation(r, from, to), "to_context_id")
}

func TestARCH02ManagementOperationContract(t *testing.T) {
	base := ManagementOperation{Kind: OperationAdd, GraphID: newTestID(t, contractTime()), ContextID: newTestID(t, contractTime()), TargetVersion: 1, JudgmentInput: "판단의 근거", AccountID: newTestID(t, contractTime()), AgentID: newTestID(t, contractTime()), AppliedAt: contractTime(), Result: OperationApplied}
	for _, kind := range []OperationKind{OperationAdd, OperationUpdate, OperationSupersede, OperationDiscard, OperationKeep} {
		op := base
		op.Kind = kind
		if err := op.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []OperationKind{OperationAdd, OperationDiscard} {
		op := base
		op.ContextID = ID{}
		op.RelationID = newTestID(t, contractTime())
		op.TargetVersion = 0
		op.Kind = kind
		if err := op.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	rejected := base
	rejected.ContextID = ID{}
	rejected.TargetVersion = 0
	rejected.Result = OperationRejected
	rejected.RejectionReason = "입력 위반"
	if err := rejected.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, field string
		mutate      func(*ManagementOperation)
	}{
		{"kind", "operation_kind", func(o *ManagementOperation) { o.Kind = "unknown" }}, {"result", "result", func(o *ManagementOperation) { o.Result = "unknown" }},
		{"graph", "graph_id", func(o *ManagementOperation) { o.GraphID = ID{} }}, {"account", "account_id", func(o *ManagementOperation) { o.AccountID = ID{} }}, {"agent", "agent_id", func(o *ManagementOperation) { o.AgentID = ID{} }},
		{"two_targets", "context_id", func(o *ManagementOperation) { o.RelationID = newTestID(t, contractTime()) }}, {"no_target", "context_id", func(o *ManagementOperation) { o.ContextID = ID{} }},
		{"version", "version", func(o *ManagementOperation) { o.TargetVersion = 0 }}, {"judgment", "judgment_input", func(o *ManagementOperation) { o.JudgmentInput = "" }}, {"judgment_nul", "judgment_input", func(o *ManagementOperation) { o.JudgmentInput = "\x00" }},
		{"time", "applied_at", func(o *ManagementOperation) { o.AppliedAt = time.Time{} }}, {"reason_on_applied", "rejection_reason", func(o *ManagementOperation) { o.RejectionReason = "사유" }},
		{"relation_version", "relation_id", func(o *ManagementOperation) { o.ContextID = ID{}; o.RelationID = newTestID(t, contractTime()) }}, {"relation_update", "relation_id", func(o *ManagementOperation) {
			o.ContextID = ID{}
			o.RelationID = newTestID(t, contractTime())
			o.TargetVersion = 0
			o.Kind = OperationUpdate
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { op := base; tc.mutate(&op); assertField(t, op.Validate(), tc.field) })
	}
	rejected.RejectionReason = ""
	assertField(t, rejected.Validate(), "rejection_reason")
	if !errors.Is(fmt.Errorf("경계: %w", ErrNotFound), ErrNotFound) || !errors.Is(fmt.Errorf("경계: %w", ErrInvalidState), ErrInvalidState) {
		t.Fatal("도메인 오류 식별 실패")
	}
	conflict, ok := errors.AsType[VersionConflictError](fmt.Errorf("경계: %w", VersionConflictError{Current: 3}))
	if !ok || conflict.Current != 3 {
		t.Fatal("판 충돌 정보 손실")
	}
}
