package store

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

type eventMemberProposalTracer struct{ reads atomic.Uint64 }

func (tracer *eventMemberProposalTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, `COLLATE "C" <> $2 COLLATE "C"`) && strings.Contains(data.SQL, "'layer'::text = 'event'") {
		tracer.reads.Add(1)
	}
	return ctx
}
func (*eventMemberProposalTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestEventMemberOrderDoesNotTriggerProposalIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, first := lifeProposalFixture(t, database)
	second := lifeDerived(t, database, graph, actor, first.ID)
	third := lifeDerived(t, database, graph, actor, first.ID)
	members := []model.ID{first.ID, second.ID, third.ID}
	slices.SortFunc(members, model.ID.Compare)
	reversed := slices.Clone(members)
	slices.Reverse(reversed)
	now := time.Now().UTC()
	event := proposalEvent(t, database, graph, actor, reversed, now, nil)
	if !slices.Equal(event.Event.MemberIDs, members) {
		t.Error("create response members are not canonical")
	}
	if !slices.Equal(reversed, []model.ID{members[2], members[1], members[0]}) {
		t.Fatal("create mutated caller members")
	}
	proposalEvent(t, database, graph, actor, members[:1], now, nil)
	database.relationProposals = lifeProposalConfig(1)
	tracer := new(eventMemberProposalTracer)
	configureTierPool(t, database, tracer)
	apply := func(name string, ids []model.ID, start time.Time, end *time.Time, wantReads uint64) {
		t.Helper()
		previous, err := database.Context(t.Context(), graph, event.ID)
		if err != nil {
			t.Fatal(err)
		}
		next := previous
		next.Event = new(*previous.Event)
		next.Event.MemberIDs = slices.Clone(ids)
		next.Event.Start, next.Event.End = start, end
		next.Version++
		if name == "body" {
			next.Body += " body-only"
		}
		input := slices.Clone(next.Event.MemberIDs)
		before := tracer.reads.Load()
		stored, err := database.UpdateContext(t.Context(), graph, previous.Version, next)
		if err != nil {
			t.Fatal(err)
		}
		if calls := tracer.reads.Load() - before; calls != wantReads {
			t.Errorf("%s proposal reads=%d want=%d", name, calls, wantReads)
		}
		read, err := database.Context(t.Context(), graph, event.ID)
		want := slices.SortedFunc(slices.Values(ids), model.ID.Compare)
		if err != nil || !slices.Equal(stored.Event.MemberIDs, want) || !slices.Equal(stored.Event.MemberIDs, read.Event.MemberIDs) {
			t.Errorf("%s response/read order mismatch: %v", name, err)
		}
		if !slices.Equal(next.Event.MemberIDs, input) {
			t.Fatalf("%s mutated caller members", name)
		}
	}
	apply("reorder", reversed, now, nil, 0)
	apply("membership", members[:2], now, nil, 1)
	apply("start", members[:2], now.Add(time.Second), nil, 1)
	apply("end", members[:2], now.Add(time.Second), new(now.Add(time.Minute)), 1)
	apply("ongoing", members[:2], now.Add(time.Second), nil, 1)
	apply("body", members[:2], now.Add(time.Second), nil, 0)
	assertConnectionRecovery(t, database, nil, 0)
}

func TestEventProposalMemberSetComparison(t *testing.T) {
	a, b, c := newTestID(t), newTestID(t), newTestID(t)
	now := time.Now().UTC()
	previous := model.Context{Event: &model.EventAttributes{MemberIDs: []model.ID{b, a}, Start: now}}
	for _, sample := range []struct {
		name string
		ids  []model.ID
		want bool
	}{{"permutation", []model.ID{a, b}, false}, {"replacement", []model.ID{a, c}, true}, {"removal", []model.ID{a}, true}, {"addition", []model.ID{a, b, c}, true}, {"duplicate", []model.ID{a, a}, true}} {
		t.Run(sample.name, func(t *testing.T) {
			next := model.Context{Event: &model.EventAttributes{MemberIDs: slices.Clone(sample.ids), Start: now}}
			if got := eventProposalInputChanged(previous, next); got != sample.want {
				t.Fatalf("changed=%t want=%t", got, sample.want)
			}
			if !slices.Equal(previous.Event.MemberIDs, []model.ID{b, a}) || !slices.Equal(next.Event.MemberIDs, sample.ids) {
				t.Fatal("comparison mutated members")
			}
		})
	}
}
