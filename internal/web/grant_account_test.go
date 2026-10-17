package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type grantAccountLookup struct {
	*fakeGraphStore
	subject model.ID
	err     error
}

func (fake grantAccountLookup) AccountByLoginID(context.Context, string) (store.Account, error) {
	return store.Account{ID: fake.subject}, fake.err
}

func TestGrantAccountLookupBoundary(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"부재", fmt.Errorf("lookup: %w", store.ErrNotFound), http.StatusBadRequest, "대상 계정을 찾을 수 없습니다."},
		{"장애", fmt.Errorf("secret lookup detail"), http.StatusInternalServerError, "대상 계정을 조회할 수 없습니다."},
		{"성공", nil, http.StatusSeeOther, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor, graph, subject := testID(t), testID(t), testID(t)
			graphs := &fakeGraphStore{accountID: actor, graphID: graph, grade: model.GraphGradeOwner}
			lookup := grantAccountLookup{fakeGraphStore: graphs, subject: subject, err: test.err}
			server, err := New(fakeAuthentication{accountID: actor}, lookup, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			target := "/graphs/" + graph.String() + "/access"
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodPost, target,
				url.Values{"action": {"grant_account"}, "login_id": {"successor"}, "grade": {"owner"}}))
			if response.Code != test.status || graphs.granted != (test.err == nil) || !strings.Contains(response.Body.String(), test.message) {
				t.Fatalf("계정 조회 경계=%d/%t/%s", response.Code, graphs.granted, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "secret lookup") || strings.Contains(response.Body.String(), "lookup:") {
				t.Fatal("조회 오류 원문이 노출됐다")
			}
			if test.err == nil && (graphs.grantSubjectID != subject || graphs.grantSubjectType != store.GrantSubjectAccount || graphs.grantGrade != model.GraphGradeOwner || response.Header().Get("Location") != target) {
				t.Fatal("계정 인계 인자·성공 이동이 달라졌다")
			}
		})
	}
}
