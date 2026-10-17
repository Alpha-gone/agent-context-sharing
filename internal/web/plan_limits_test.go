package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

type planRestoreStore struct {
	fakeGraphStore
	called bool
	limit  int64
	actor  model.ID
	err    error
}

func (s *planRestoreStore) SetContextDeleted(_ context.Context, _, _ model.ID, actor model.ID, deleted bool, limit int64) (model.Context, error) {
	s.called, s.limit, s.actor = !deleted, limit, actor
	return model.Context{}, s.err
}

func TestWebRestoreLimitMessage(t *testing.T) {
	for _, sample := range []struct {
		name string
		err  error
		want string
	}{
		{"storage", plan.LimitError{Name: "stored_characters_per_graph", Current: 13, Allowed: 10}, "현재 13자, 허용 10자"},
		{"internal", errors.New("sensitive database details"), "현재 상태에서는 처리할 수 없습니다."},
		{"unknown_limit", plan.LimitError{Name: "sensitive database details", Current: 13, Allowed: 10}, "현재 상태에서는 처리할 수 없습니다."},
	} {
		t.Run(sample.name, func(t *testing.T) {
			account, graph := testID(t), testID(t)
			graphs := &planRestoreStore{fakeGraphStore: fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner}, err: sample.err}
			server, err := New(fakeAuthentication{accountID: account}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodPost, "/graphs/"+graph.String()+"/deletion", url.Values{"action": {"restore_context"}, "context_id": {testID(t).String()}}))
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), sample.want) || strings.Contains(response.Body.String(), "sensitive database details") {
				t.Fatalf("복구 오류 안내=%d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestWebRestoreUsesAccountPlan(t *testing.T) {
	for _, limit := range []int64{0, 13} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			account, graph := testID(t), testID(t)
			graphs := &planRestoreStore{fakeGraphStore: fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner}}
			plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"stored_characters_per_graph":%d}}`, account, limit))
			if err != nil {
				t.Fatal(err)
			}
			server, err := New(fakeAuthentication{accountID: account}, graphs, Config{
				SecureCookie: func(*http.Request) bool { return true }, Plans: plans,
			})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			form := url.Values{"action": {"restore_context"}, "context_id": {testID(t).String()}}
			server.ServeHTTP(response, sessionRequest(http.MethodPost, "/graphs/"+graph.String()+"/deletion", form))
			if response.Code != http.StatusSeeOther || !graphs.called || graphs.limit != limit || graphs.actor != account {
				t.Fatalf("웹 복구=%d, 호출=%t, 한도=%d, 계정=%s", response.Code, graphs.called, graphs.limit, graphs.actor)
			}
		})
	}
}
