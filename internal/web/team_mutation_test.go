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
	"agent_context_sharing/internal/store"
)

type teamMutationStore struct {
	*fakeGraphStore
	err, lookupErr error
	called         bool
	actor, member  model.ID
}

func (fake *teamMutationStore) AccountByLoginID(context.Context, string) (store.Account, error) {
	return store.Account{ID: fake.member}, fake.lookupErr
}
func (fake *teamMutationStore) CreateTeam(_ context.Context, actor model.ID, _ string) (model.Team, error) {
	fake.called, fake.actor = true, actor
	return model.Team{}, fake.err
}
func (fake *teamMutationStore) AddTeamMember(_ context.Context, _, actor, _ model.ID) error {
	fake.called, fake.actor = true, actor
	return fake.err
}
func (fake *teamMutationStore) RemoveTeamMemberWithAudit(_ context.Context, _, actor, _ model.ID) error {
	fake.called, fake.actor = true, actor
	return fake.err
}
func (fake *teamMutationStore) SetTeamDeleted(_ context.Context, _, actor model.ID, _ bool) error {
	fake.called, fake.actor = true, actor
	return fake.err
}

func TestTeamMutationHTTPBoundary(t *testing.T) {
	for _, action := range []string{"create_team", "add_member", "remove_member", "delete_team", "restore_team"} {
		for _, sample := range []struct {
			name   string
			err    error
			status int
		}{
			{"not_found", store.ErrNotFound, http.StatusNotFound},
			{"state", store.ErrInvalidState, http.StatusConflict},
			{"input", model.FieldError{Field: "name", Message: "synthetic input"}, http.StatusBadRequest},
			{"internal", errors.New("secret team storage detail"), http.StatusInternalServerError},
			{"success", nil, http.StatusSeeOther},
		} {
			t.Run(action+"/"+sample.name, func(t *testing.T) {
				actor := testID(t)
				graphs := &teamMutationStore{fakeGraphStore: &fakeGraphStore{}, member: testID(t)}
				if sample.err != nil {
					graphs.err = fmt.Errorf("storage context: %w", sample.err)
				}
				server, err := New(fakeAuthentication{accountID: actor}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, sessionRequest(http.MethodPost, "/access", url.Values{"action": {action}, "team_name": {"공유 팀"}, "team_id": {testID(t).String()}, "login_id": {"member"}}))
				if !graphs.called || graphs.actor != actor || response.Code != sample.status {
					t.Fatalf("팀 관리 경계=%t/%s/%d", graphs.called, graphs.actor, response.Code)
				}
				if strings.Contains(response.Body.String(), "secret team") || strings.Contains(response.Body.String(), "storage context") {
					t.Fatal("내부 오류 원문이 노출됐다")
				}
				if sample.err == nil && response.Header().Get("Location") != "/access" {
					t.Fatal("그래프 없는 팀 관리의 이동이 바뀌었다")
				}
			})
		}
	}
}

func TestTeamMemberLookupFailureBoundary(t *testing.T) {
	for _, action := range []string{"add_member", "remove_member"} {
		for _, sample := range []struct {
			name   string
			err    error
			status int
		}{
			{"missing", store.ErrNotFound, http.StatusBadRequest},
			{"internal", errors.New("secret member lookup detail"), http.StatusInternalServerError},
		} {
			t.Run(action+"/"+sample.name, func(t *testing.T) {
				actor := testID(t)
				graphs := &teamMutationStore{fakeGraphStore: &fakeGraphStore{}, lookupErr: fmt.Errorf("lookup: %w", sample.err)}
				server, err := New(fakeAuthentication{accountID: actor}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, sessionRequest(http.MethodPost, "/access", url.Values{"action": {action}, "team_id": {testID(t).String()}, "login_id": {"member"}}))
				if graphs.called || response.Code != sample.status || strings.Contains(response.Body.String(), "secret member") || strings.Contains(response.Body.String(), "lookup:") {
					t.Fatalf("팀 계정 조회 경계=%t/%d", graphs.called, response.Code)
				}
			})
		}
	}
}
