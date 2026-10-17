package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type lifecycleDeletionStore struct {
	fakeGraphStore
	err    error
	called bool
}

func (s *lifecycleDeletionStore) SetGraphDeleted(context.Context, model.ID, model.ID, bool) error {
	s.called = true
	return s.err
}
func (s *lifecycleDeletionStore) SetContextDeleted(context.Context, model.ID, model.ID, model.ID, bool, int64) (model.Context, error) {
	s.called = true
	return model.Context{}, s.err
}

func TestLifeWebDeletionErrorStatus(t *testing.T) {
	for _, action := range []string{"restore", "restore_context"} {
		for _, sample := range []struct {
			name   string
			err    error
			status int
		}{
			{"lost_access", store.ErrNotFound, http.StatusNotFound},
			{"invalid_state", store.ErrInvalidState, http.StatusConflict},
			{"internal", errors.New("sensitive internal fixture"), http.StatusInternalServerError},
		} {
			t.Run(action+"/"+sample.name, func(t *testing.T) {
				actor, graph := testID(t), testID(t)
				graphs := &lifecycleDeletionStore{fakeGraphStore: fakeGraphStore{accountID: actor, graphID: graph, grade: model.GraphGradeOwner}, err: sample.err}
				server, err := New(fakeAuthentication{accountID: actor}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, sessionRequest(http.MethodPost, "/graphs/"+graph.String()+"/deletion", url.Values{"action": {action}, "context_id": {testID(t).String()}}))
				if !graphs.called || response.Code != sample.status || strings.Contains(response.Body.String(), "sensitive internal fixture") {
					t.Fatalf("삭제 처리 경계=%t/%d %s", graphs.called, response.Code, response.Body.String())
				}
			})
		}
	}
}
