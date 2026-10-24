package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

type web04AccessStore struct {
	*fakeGraphStore
	stage  string
	cancel context.CancelFunc
	calls  []string
}

func (graphs *web04AccessStore) step(stage string) error {
	graphs.calls = append(graphs.calls, stage)
	if graphs.stage == stage {
		if graphs.cancel != nil {
			graphs.cancel()
			return context.Canceled
		}
		return errors.New("private access storage detail")
	}
	return nil
}
func (graphs *web04AccessStore) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	return model.GraphGradeOwner, true, graphs.step("grade")
}
func (graphs *web04AccessStore) ListGraphGrants(context.Context, model.ID) ([]model.GrantSubject, error) {
	return nil, graphs.step("grants")
}
func (graphs *web04AccessStore) ListGrantableTeams(context.Context) ([]model.Team, error) {
	return nil, graphs.step("choices")
}
func (graphs *web04AccessStore) ListManagedTeams(context.Context, model.ID) ([]model.Team, error) {
	return nil, graphs.step("managed")
}
func TestWEB04AccessReadBoundaries(t *testing.T) {
	stages := []string{"grade", "grants", "choices", "managed"}
	for index, stage := range stages {
		for _, cancelled := range []bool{false, true} {
			name := stage + "/failure"
			if cancelled {
				name = stage + "/cancelled"
			}
			t.Run(name, func(t *testing.T) {
				actor, graph := testID(t), testID(t)
				graphs := &web04AccessStore{fakeGraphStore: &fakeGraphStore{accountID: actor, graphID: graph}, stage: stage}
				request := sessionRequest(http.MethodGet, "/graphs/"+graph.String()+"/access", nil)
				if cancelled {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					graphs.cancel = cancel
					request = request.WithContext(ctx)
				}
				response := httptest.NewRecorder()
				server, err := New(fakeAuthentication{accountID: actor}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				server.ServeHTTP(response, request)
				if !slices.Equal(graphs.calls, stages[:index+1]) {
					t.Fatalf("조회 중단 = %v", graphs.calls)
				}
				if cancelled {
					if response.Body.Len() != 0 {
						t.Fatal("취소 뒤 화면을 렌더링했다")
					}
				} else if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private access") {
					t.Fatalf("오류 사상 = %d", response.Code)
				}
			})
		}
	}
}
