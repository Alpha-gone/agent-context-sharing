package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type navigationWebStore struct {
	*web06Store
	selected       model.ID
	selectedResult store.HopResult
	selectedError  error
	calls          int
}

func (s *navigationWebStore) HopContexts(_ context.Context, graph, start model.ID, hops int, direction string, filter []string, maximum int) (store.HopResult, error) {
	s.calls++
	if graph != s.graphID || start != s.selected || direction != "both" || hops < 1 || maximum < 1 || len(filter) != 0 {
		return store.HopResult{}, errors.New("unexpected traversal")
	}
	return s.selectedResult, s.selectedError
}

func TestConversationNavigationWebLinksAndOrder(t *testing.T) {
	account, graph := testID(t), testID(t)
	source := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerSource, Body: "원문", Source: &model.SourceAttributes{ConversationID: "<chat>", TurnIndex: new(int64(0))}}
	event := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerEvent, Body: "사건", Event: &model.EventAttributes{ConversationID: "<chat>", MemberIDs: []model.ID{source.ID}}}
	summary := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerDerived, Body: "대표", Derived: &model.DerivedAttributes{Kind: model.DerivationKindSummary, NavigationRefs: []model.ID{event.ID}}}
	base := &web06Store{fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph}, graph: model.Graph{Name: "안내", EntryContextID: new(summary.ID)}, result: store.HopResult{Contexts: []model.Context{summary, event, source}, Edges: []store.HopEdge{{FromID: summary.ID, ToID: event.ID, Kind: "navigation_refs"}, {FromID: event.ID, ToID: source.ID, Kind: "member_refs"}}}}
	html := web06Detail(t, base)
	for _, want := range []string{"대표 요약", "안내 참조", "파생 근거와 구분", "사건 구성원", "&lt;chat&gt;", "턴 0", "?context_id=" + event.ID.String(), "?context_id=" + source.ID.String()} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q", want)
		}
	}
	elements := web06Elements(t, html)
	if elements.Nodes[2].Data.TurnIndex == nil || *elements.Nodes[2].Data.TurnIndex != 0 || elements.Nodes[2].Data.ConversationID != "<chat>" {
		t.Fatal("selected detail loses turn zero")
	}
	backend := &navigationWebStore{web06Store: base, selected: event.ID, selectedResult: store.HopResult{Contexts: []model.Context{event, source}, Edges: base.result.Edges[1:]}}
	server, err := New(fakeAuthentication{accountID: account}, backend, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name, query string
		err         error
		status      int
	}{{"selected", event.ID.String(), nil, 200}, {"invalid", "bad", nil, 400}, {"missing", event.ID.String(), model.ErrNotFound, 404}, {"failure", event.ID.String(), errors.New("private storage"), 500}} {
		t.Run(sample.name, func(t *testing.T) {
			backend.selectedError = sample.err
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graph.String()+"?context_id="+sample.query, nil))
			if response.Code != sample.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private storage") {
				t.Fatal("storage error exposed")
			}
		})
	}
}

func TestEventMemberLinksUseConversationTurnOrder(t *testing.T) {
	graph := testID(t)
	later := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerSource, Source: &model.SourceAttributes{ConversationID: "chat", TurnIndex: new(int64(8))}}
	earlier := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerSource, Source: &model.SourceAttributes{ConversationID: "chat", TurnIndex: new(int64(0))}}
	legacy := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerSource, Source: &model.SourceAttributes{}}
	missing := testID(t)
	event := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerEvent, Event: &model.EventAttributes{ConversationID: "chat", MemberIDs: []model.ID{missing, legacy.ID, later.ID, earlier.ID}}}
	data := pageData{Contexts: []model.Context{legacy, later, earlier}}
	links := data.EventMemberLinks(event)
	if len(links) != 4 || links[0].ID != earlier.ID || links[1].ID != later.ID || links[2].TurnIndex != nil || links[3].TurnIndex != nil {
		t.Fatalf("member turn order: %+v", links)
	}
	backend := &web06Store{fakeGraphStore: &fakeGraphStore{accountID: testID(t), graphID: graph}, graph: model.Graph{Name: "순서"}, result: store.HopResult{Contexts: append(data.Contexts, event)}}
	html := web06Detail(t, backend)
	_, members, _ := strings.Cut(html, "<h3>사건 구성원</h3>")
	if strings.Index(members, "?context_id="+earlier.ID.String()) > strings.Index(members, "?context_id="+later.ID.String()) || !strings.Contains(members, "대화 순서 정보 없음") || !strings.Contains(members, "조회 범위 밖: 순서 확인 불가") {
		t.Fatal("ordered source links or legacy fallback missing")
	}
}
