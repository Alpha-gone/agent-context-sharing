package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

type web03ListStore struct {
	*fakeGraphStore
	events []string
	failAt string
	err    error
	cancel context.CancelFunc
	limit  int
}

func (s *web03ListStore) step(name string) error {
	s.events = append(s.events, name)
	if name == s.failAt {
		if s.cancel != nil {
			s.cancel()
		}
		return s.err
	}
	return nil
}

func (s *web03ListStore) ListGraphs(_ context.Context, account model.ID, filter model.GraphListFilter, _ string, limit int) ([]model.GraphListItem, string, error) {
	s.limit = limit
	if account != s.accountID || !reflect.DeepEqual(filter.Grades, []model.GraphGrade{model.GraphGradeOwner, model.GraphGradeViewer}) {
		return nil, "", errors.New("조회 계정·정규화한 등급이 다르다")
	}
	return nil, "", s.step("active")
}
func (s *web03ListStore) ListRestoreEligibleGraphs(context.Context, model.ID) ([]model.Graph, error) {
	return nil, s.step("automatic")
}
func (s *web03ListStore) ListOwnedDeletedGraphs(context.Context, model.ID) ([]model.Graph, error) {
	return nil, s.step("direct")
}

func TestWEB03ListBoundaries(t *testing.T) {
	for _, phase := range []string{"active", "automatic", "direct"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%t", phase, canceled), func(t *testing.T) {
				account := testID(t)
				backend := &web03ListStore{fakeGraphStore: &fakeGraphStore{accountID: account}, failAt: phase, err: errors.New("private database details")}
				plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"graph_page":{"default":2,"maximum":3}}}`, account.String()))
				if err != nil {
					t.Fatal(err)
				}
				server, err := New(fakeAuthentication{accountID: account}, backend, Config{Plans: plans, SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if canceled {
					backend.cancel = cancel
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, sessionRequest("GET", "/graphs?grade=viewer&grade=owner&grade=viewer", nil).WithContext(ctx))
				if backend.limit != 2 || backend.events[len(backend.events)-1] != phase || strings.Contains(response.Body.String(), "private database") {
					t.Fatal("페이지 한도·단계별 중단·안전한 오류 안내가 다르다")
				}
				if canceled && response.Body.Len() != 0 || !canceled && response.Code != 500 {
					t.Fatal("취소 또는 저장 장애 응답이 다르다", response.Code)
				}
			})
		}
	}
}

func TestWEB03InvalidCursorKeepsFilter(t *testing.T) {
	account := testID(t)
	backend := &web03ListStore{fakeGraphStore: &fakeGraphStore{accountID: account}, failAt: "active", err: fmt.Errorf("private cursor details: %w", store.ErrInvalidCursor)}
	server, err := New(fakeAuthentication{accountID: account}, backend, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, sessionRequest("GET", "/graphs?name=keep&grade=viewer&grade=owner&cursor=bad", nil))
	form := graphFilterForm(t, response.Body.String())
	if response.Code != 400 || form.Get("name") != "keep" || !reflect.DeepEqual(form["grade"], []string{"owner", "viewer"}) || strings.Contains(response.Body.String(), "private cursor") || len(backend.events) != 1 {
		t.Fatal("커서 오류가 입력 오류·필터 복구·후속 조회 중단을 유지하지 않는다")
	}
}

func TestWEB03NextPageNormalizesGradeSet(t *testing.T) {
	account := testID(t)
	backend := &filterGraphStore{fakeGraphStore: &fakeGraphStore{accountID: account, cursor: "next"}}
	server, err := New(fakeAuthentication{accountID: account}, backend, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	request := sessionRequest("GET", "/graphs?name=++name++&grade=viewer&grade=&grade=owner&grade=owner", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	filter, err := graphFilter(request)
	if err != nil {
		t.Fatal(err)
	}
	page, err := url.Parse(graphListPage(filter, backend.cursor))
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || backend.filter.Name != "name" || !reflect.DeepEqual(page.Query()["grade"], []string{"owner", "viewer"}) || page.Query().Get("cursor") != "next" {
		t.Fatal("조회와 다음 페이지의 선택 집합이 다르다")
	}
}
