package store

import (
	"testing"
	"time"
)

func TestConversationSegmentFreezeIntegration(t *testing.T) {
	database, _, pool := newAgentContractDatabase(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://synthetic/segment-freeze"), nil)
	if err != nil {
		t.Fatal(err)
	}
	event, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(), `INSERT INTO public.conversation_segment (event_context_id,graph_id,conversation_id,first_turn_index,last_turn_index,created_at) VALUES ($1,$2,'synthetic',0,1,statement_timestamp())`, event.ID.String(), graph.String())
	if err != nil {
		t.Fatal(err)
	}
	next := attachReferences(event, nil, event.Event.MemberIDs)
	next.Body += " 수정"
	next.Version++
	updated, err := database.UpdateContextWithOperation(t.Context(), graph, event.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var frozen time.Time
	if err := pool.QueryRow(t.Context(), `SELECT frozen_at FROM public.conversation_segment WHERE event_context_id=$1`, event.ID.String()).Scan(&frozen); err != nil || frozen.IsZero() {
		t.Fatal("구간 고정 실패", err)
	}
	if _, err := database.DiscardContext(t.Context(), graph, event.ID, registrationOperation(updated, OperationDiscard), WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := pool.QueryRow(t.Context(), `SELECT frozen_at FROM public.conversation_segment WHERE event_context_id=$1`, event.ID.String()).Scan(&after); err != nil || !after.Equal(frozen) {
		t.Fatal("고정 시각 변경", err)
	}
}
