package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

type obs06AuditStore struct {
	fakeGraphStore
	t     *testing.T
	calls int
	limit int
}

func (s *obs06AuditStore) ListAuditEntries(_ context.Context, graph, actor model.ID, _ model.AuditFilter, _ string, limit int) ([]model.AuditEntry, string, error) {
	s.calls++
	if graph != s.graphID || actor != s.accountID || limit != s.limit {
		s.t.Errorf("감사 범위·계정·플랜=%s/%s/%d", graph, actor, limit)
	}
	return nil, "", nil
}

func TestOBS06AuditWebOwnerAndPlan(t *testing.T) {
	actor, graph := testID(t), testID(t)
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"graph_page":{"default":3,"maximum":7}}}`, actor))
	if err != nil {
		t.Fatal(err)
	}
	for _, grade := range []model.GraphGrade{model.GraphGradeOwner, model.GraphGradeViewer} {
		t.Run(string(grade), func(t *testing.T) {
			backend := &obs06AuditStore{accountID: actor, graphID: graph, grade: grade, t: t, limit: 3}
			server, err := New(fakeAuthentication{accountID: actor}, backend, Config{Plans: plans, SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/graphs/"+graph.String()+"/audit", nil)
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if grade == model.GraphGradeOwner {
				if response.Code != http.StatusOK || backend.calls != 1 {
					t.Fatalf("소유자 감사=%d/%d", response.Code, backend.calls)
				}
			} else if response.Code != http.StatusForbidden || backend.calls != 0 {
				t.Fatalf("일반 등급 감사=%d/%d", response.Code, backend.calls)
			}
		})
	}
}
