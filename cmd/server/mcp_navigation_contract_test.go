package main

import (
	"testing"

	"agent_context_sharing/internal/model"
)

func TestMCPConversationEntryReplayIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	createSource := func(turn int64, locator, at string) model.ID {
		args := f.args("node_create")
		args["layer"], args["body"], args["origin_kind"], args["occurred_at"] = "source", "대화 원문", "user_utterance", at
		args["source_ref"] = map[string]any{"channel": "conversation", "locator": locator}
		args["conversation_id"], args["turn_index"] = "dialogue", turn
		return f.id(t, "node_create", args, "node", "context_id")
	}
	first := createSource(0, "conversation://dialogue/0", "2026-10-20T12:01:00Z")
	second := createSource(1, "conversation://dialogue/1", "2026-10-20T12:02:00Z")
	args := f.args("node_create")
	args["layer"], args["body"], args["member_refs"] = "event", "앞 대화 구간", []string{first.String()}
	args["conversation_id"], args["event_start"], args["event_end"] = "dialogue", "2026-10-20T12:00:00Z", "2026-10-20T12:10:00Z"
	previous := f.id(t, "node_create", args, "node", "context_id")
	args["body"], args["member_refs"], args["event_start"] = "뒤 대화 구간", []string{second.String()}, "2026-10-20T12:02:00Z"
	args["previous_event_id"], args["request_id"] = previous.String(), "conversation-replay-303"
	current := f.id(t, "node_create", args, "node", "context_id")
	if replay := f.id(t, "node_create", args, "node", "context_id"); replay != current {
		t.Fatalf("사건·연결 재생이 다른 사건을 만들었다: %s / %s", current, replay)
	}
	relations, _, err := f.database.ListContextRelations(t.Context(), f.graph, current, []model.RelationState{model.RelationStateConfirmed}, []model.RelationType{model.RelationTypePrecedes}, "", 100)
	if err != nil || len(relations) != 1 || relations[0].FromContextID != previous || relations[0].ToContextID != current {
		t.Fatalf("명시적 관계 재생=%+v / %v", relations, err)
	}
	var audits int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM public.operation_log WHERE graph_id=$1 AND relation_id=$2 AND result='applied'", f.graph.String(), relations[0].ID.String()).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("관계 적용 감사 중복=%d / %v", audits, err)
	}
	summary := f.args("node_create")
	summary["layer"], summary["body"], summary["derived_from"], summary["derivation_kind"], summary["summary_scope"], summary["evidence_state"] = "derived", "대화 안내", []string{first.String()}, "summary", "local", "observation"
	summary["navigation_refs"] = []string{previous.String()}
	entry := f.id(t, "node_create", summary, "node", "context_id")
	graph, err := f.database.Graph(t.Context(), f.graph)
	if err != nil {
		t.Fatal(err)
	}
	update := f.args("graph_update")
	update["expected_version"], update["entry_context_id"] = graph.Version, entry.String()
	f.call(t, "graph_update", update, "")
	graph, err = f.database.Graph(t.Context(), f.graph)
	if err != nil || graph.EntryContextID == nil || *graph.EntryContextID != entry {
		t.Fatalf("대표 발견=%+v / %v", graph, err)
	}
	hops, err := f.database.HopContexts(t.Context(), f.graph, entry, 3, "out", []string{"navigation_refs", "precedes", "has_member"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[model.ID]bool{}
	for _, node := range hops.Contexts {
		seen[node.ID] = true
		if node.ID == second && (node.Source == nil || node.Source.TurnIndex == nil || *node.Source.TurnIndex != 1) {
			t.Fatal("흐름의 원천 턴 순서를 잃었다")
		}
	}
	for _, id := range []model.ID{entry, previous, current, first, second} {
		if !seen[id] {
			t.Fatalf("안내→사건 흐름→원천 경로에서 %s 누락", id)
		}
	}
}

func TestMCPGraphEntryOmissionPreservesHiddenDesignationIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	summary := f.args("node_create")
	summary["layer"], summary["body"], summary["derived_from"], summary["derivation_kind"], summary["summary_scope"], summary["evidence_state"] = "derived", "대표 요약", []string{f.source.String()}, "summary", "global", "observation"
	summary["navigation_refs"] = []string{f.firstEvent.String()}
	entry := f.id(t, "node_create", summary, "node", "context_id")
	graph, err := f.database.Graph(t.Context(), f.graph)
	if err != nil {
		t.Fatal(err)
	}
	update := f.args("graph_update")
	update["expected_version"], update["entry_context_id"] = graph.Version, entry.String()
	f.call(t, "graph_update", update, "")
	discard := f.args("node_discard")
	discard["context_id"] = entry.String()
	f.call(t, "node_discard", discard, "")
	graph, err = f.database.Graph(t.Context(), f.graph)
	if err != nil || graph.EntryContextID != nil {
		t.Fatalf("invalid entry must be hidden: %+v %v", graph, err)
	}
	update = f.args("graph_update")
	update["expected_version"], update["name"] = graph.Version, "대표를 보존하는 이름 변경"
	result := f.call(t, "graph_update", update, "")
	if _, exposed := result["graph"].(map[string]any)["entry_context_id"]; exposed {
		t.Fatal("name-only update exposed hidden invalid entry")
	}
	var stored *string
	if err := f.pool.QueryRow(t.Context(), "SELECT entry_context_id::text FROM public.context_graph WHERE graph_id=$1", f.graph.String()).Scan(&stored); err != nil || stored == nil || *stored != entry.String() {
		t.Fatalf("omitted entry changed raw designation: %v %v", stored, err)
	}
	graph, err = f.database.Graph(t.Context(), f.graph)
	if err != nil {
		t.Fatal(err)
	}
	update["expected_version"], update["entry_context_id"] = graph.Version, nil
	f.call(t, "graph_update", update, "")
	if err := f.pool.QueryRow(t.Context(), "SELECT entry_context_id::text FROM public.context_graph WHERE graph_id=$1", f.graph.String()).Scan(&stored); err != nil || stored != nil {
		t.Fatalf("explicit null did not clear: %v %v", stored, err)
	}
}
