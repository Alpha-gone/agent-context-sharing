package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestNavigationRevisionRoundTripIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	for _, refs := range [][]model.ID{nil, {}, {source.ID}} {
		entry := testDerivedContext(t, graph, actor)
		entry.Derived.Kind, entry.Derived.SummaryScope = model.DerivationKindSummary, model.SummaryScopeLocal
		entry.Derived.NavigationRefs = refs
		stored, err := database.CreateContext(t.Context(), graph, entry, []model.ID{source.ID})
		if err != nil {
			t.Fatal(err)
		}
		revision, err := database.ContextRevision(t.Context(), graph, stored.ID, stored.Version)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored, revision.Context) {
			t.Fatalf("탐색 참조 스냅숏 왕복: before=%#v after=%#v", stored, revision.Context)
		}
	}
}

func TestNavigationGraphEntryIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graphID := createTestGraph(t, database, actor)
	source := testSourceContext(t, graphID, actor, "api://navigation/"+newTestID(t).String())
	if _, err := database.CreateContext(t.Context(), graphID, source, nil); err != nil {
		t.Fatal(err)
	}
	event := testEventContext(t, graphID, actor, source.ID)
	event.Event.Start = source.Source.OccurredAt
	if _, err := database.CreateContext(t.Context(), graphID, event, nil); err != nil {
		t.Fatal(err)
	}
	entry := testDerivedContext(t, graphID, actor)
	entry.Derived.Kind, entry.Derived.SummaryScope = model.DerivationKindSummary, model.SummaryScopeLocal
	entry.Derived.NavigationRefs = []model.ID{source.ID, event.ID}
	stored, err := database.CreateContext(t.Context(), graphID, entry, []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []model.Context{stored, navigationReadContext(t, database, graphID, entry.ID)} {
		if !sameEventMembers(read.Derived.NavigationRefs, entry.Derived.NavigationRefs) {
			t.Fatalf("탐색 간선 조립=%v", read.Derived.NavigationRefs)
		}
	}
	for _, sample := range []struct {
		start     model.ID
		direction string
		count     int
	}{{entry.ID, "out", 3}, {event.ID, "in", 2}, {event.ID, "both", 2}, {event.ID, "out", 1}} {
		hop, err := database.HopContexts(t.Context(), graphID, sample.start, 1, sample.direction, []string{"navigation_refs"}, 10)
		if err != nil || len(hop.Contexts) != sample.count {
			t.Fatalf("탐색 방향 %s: %d/%v", sample.direction, len(hop.Contexts), err)
		}
	}
	graph, err := database.Graph(t.Context(), graphID)
	if err != nil {
		t.Fatal(err)
	}
	graph.EntryContextID = new(entry.ID)
	graph, err = database.UpdateGraphMetadata(t.Context(), graph, graph.Version)
	if err != nil || graph.EntryContextID == nil || *graph.EntryContextID != entry.ID {
		t.Fatalf("대표 지정=%+v/%v", graph, err)
	}
	if _, err := database.UpdateGraphMetadata(t.Context(), graph, graph.Version-1); err == nil {
		t.Fatal("오래된 기대 판을 허용했다")
	}
	visual, err := database.GraphVisualization(t.Context(), graphID, 0, 10)
	if err != nil || len(visual.Contexts) != 1 || visual.Contexts[0].ID != entry.ID {
		t.Fatalf("대표 시각화 시작점=%+v/%v", visual.Contexts, err)
	}
	graph, err = database.UpdateGraph(t.Context(), graphID, graph.Version, "대표 유지", "")
	if err != nil || graph.EntryContextID == nil || *graph.EntryContextID != entry.ID {
		t.Fatalf("기존 메타데이터 API 대표 보존=%+v/%v", graph, err)
	}
	graph.EntryContextID = nil
	graph, err = database.UpdateGraphMetadata(t.Context(), graph, graph.Version)
	if err != nil || graph.EntryContextID != nil {
		t.Fatalf("대표 해제=%+v/%v", graph, err)
	}
	graph.EntryContextID = new(entry.ID)
	graph, err = database.UpdateGraphMetadata(t.Context(), graph, graph.Version)
	if err != nil {
		t.Fatal(err)
	}
	// 탐색 대상의 폐기는 근거 무효 신호를 확장하지 않는다.
	if _, err := database.DiscardContext(t.Context(), graphID, event.ID, nil, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	if navigationReadContext(t, database, graphID, entry.ID).Derived.EvidenceInvalidated {
		t.Fatal("탐색 대상 폐기가 근거 무효 표시를 바꿨다")
	}
	next := testDerivedContext(t, graphID, actor)
	next.Derived.Kind, next.Derived.SummaryScope = model.DerivationKindSummary, model.SummaryScopeLocal
	if _, err := database.CreateSupersedingContextWithOperation(t.Context(), graphID, next, []model.ID{source.ID}, entry.ID, nil, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	read, err := database.Graph(t.Context(), graphID)
	if err != nil || read.EntryContextID != nil {
		t.Fatalf("대체된 대표 숨김=%+v/%v", read, err)
	}
	var raw string
	if err := database.pool.QueryRow(t.Context(), `SELECT entry_context_id::text FROM public.context_graph WHERE graph_id=$1`, graphID.String()).Scan(&raw); err != nil || raw != entry.ID.String() {
		t.Fatalf("조회가 저장 지정을 바꿨다: %s/%v", raw, err)
	}
	violations, err := database.AuditGraph(t.Context(), graphID, EmbeddingExpectation{})
	if err != nil || len(violations) > 0 {
		t.Fatalf("탐색 참조 감사=%+v/%v", violations, err)
	}
}

func TestNavigationIsolationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph, foreign := createTestGraph(t, database, actor), createTestGraph(t, database, actor)
	source := testSourceContext(t, graph, actor, "api://navigation/"+newTestID(t).String())
	other := testSourceContext(t, foreign, actor, "api://navigation/"+newTestID(t).String())
	for _, value := range []model.Context{source, other} {
		if _, err := database.CreateContext(t.Context(), value.GraphID, value, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []model.ID{other.ID, newTestID(t)} {
		entry := testDerivedContext(t, graph, actor)
		entry.Derived.Kind, entry.Derived.SummaryScope = model.DerivationKindSummary, model.SummaryScopeLocal
		entry.Derived.NavigationRefs = []model.ID{target}
		if _, err := database.CreateContext(t.Context(), graph, entry, []model.ID{source.ID}); err == nil {
			t.Fatal("다른 그래프·없는 탐색 대상을 허용했다")
		}
		if _, err := database.Context(t.Context(), graph, entry.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("실패한 요약이 남았다: %v", err)
		}
	}
	metadata, err := database.Graph(t.Context(), graph)
	if err != nil {
		t.Fatal(err)
	}
	entry := testDerivedContext(t, graph, actor)
	entry.Derived.Kind, entry.Derived.SummaryScope = model.DerivationKindSummary, model.SummaryScopeLocal
	entry.Derived.ValidTo = new(time.Now().UTC().Add(-time.Second))
	if _, err := database.CreateContext(t.Context(), graph, entry, []model.ID{source.ID}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []model.ID{other.ID, source.ID, entry.ID, newTestID(t)} {
		metadata.EntryContextID = new(target)
		if _, err := database.UpdateGraphMetadata(t.Context(), metadata, metadata.Version); err == nil {
			t.Fatal("잘못된 대표를 지정했다")
		}
	}
	// 근거 필터는 탐색 링크를 따라 사건으로 확장하지 않는다.
	hops, err := database.HopContexts(t.Context(), graph, entry.ID, 1, "out", []string{"derived_from"}, 10)
	if err != nil || !slices.ContainsFunc(hops.Contexts, func(value model.Context) bool { return value.ID == source.ID }) {
		t.Fatalf("기존 근거 탐색 호환=%+v/%v", hops, err)
	}
}

func navigationReadContext(t *testing.T, database *Store, graph, context model.ID) model.Context {
	t.Helper()
	value, err := database.Context(t.Context(), graph, context)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
