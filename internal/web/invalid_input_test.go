package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type invalidInputStore struct {
	*fakeGraphStore
	calls int
}

func (s *invalidInputStore) AccountByLoginID(context.Context, string) (store.Account, error) {
	s.calls++
	return store.Account{}, store.ErrNotFound
}
func (s *invalidInputStore) CreateTeam(context.Context, model.ID, string) (model.Team, error) {
	s.calls++
	return model.Team{}, nil
}
func (s *invalidInputStore) ListGraphs(context.Context, model.ID, model.GraphListFilter, string, int) ([]model.GraphListItem, string, error) {
	s.calls++
	return nil, "", nil
}

func TestInvalidAccessInputRejectedBeforeStore(t *testing.T) {
	for _, action := range []string{"grant_account", "add_member", "remove_member", "create_team"} {
		values := []string{"", "a\x00b", "a\xffb"}
		if action != "create_team" {
			values = append(values, "ab", "Upper", "a-b", strings.Repeat("a", 33), " member ")
		}
		for _, value := range values {
			t.Run(action+"/"+url.QueryEscape(value), func(t *testing.T) {
				actor, graph := testID(t), testID(t)
				graphs := &invalidInputStore{fakeGraphStore: &fakeGraphStore{graphID: graph, grade: model.GraphGradeOwner}}
				server := newTestServerWith(t, actor, graphs.fakeGraphStore)
				form := url.Values{"action": {action}, "login_id": {value}, "team_name": {value}, "team_id": {testID(t).String()}, "grade": {"viewer"}}
				server.graphs = graphs
				response := httptest.NewRecorder()
				server.ServeHTTP(response, sessionRequest(http.MethodPost, "/graphs/"+graph.String()+"/access", form))
				if response.Code != http.StatusBadRequest || graphs.calls != 0 || graphs.granted {
					t.Fatalf("status=%d calls=%d granted=%t", response.Code, graphs.calls, graphs.granted)
				}
			})
		}
	}
}

func TestInvalidGraphNameFilterRejectedBeforeStore(t *testing.T) {
	for _, name := range []string{"\x00", "\xff"} {
		t.Run(url.QueryEscape(name), func(t *testing.T) {
			graphs := &invalidInputStore{fakeGraphStore: &fakeGraphStore{}}
			server := newTestServerWith(t, testID(t), graphs.fakeGraphStore)
			server.graphs = graphs
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs?name="+url.QueryEscape(name), nil))
			if response.Code != http.StatusBadRequest || graphs.calls != 0 {
				t.Fatalf("status=%d calls=%d", response.Code, graphs.calls)
			}
		})
	}
}
