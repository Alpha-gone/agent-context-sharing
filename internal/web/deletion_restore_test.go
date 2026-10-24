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
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

// web05Store는 삭제와 복구·운영자 화면이 읽는 단계와 상태 변경 인자를 기록한다.
type web05Store struct {
	*fakeGraphStore
	graph    model.Graph
	impacts  map[model.ID]model.DeletionImpact
	requests []model.RestoreRequest
	events   []string
	failAt   string
	err      error
	cancel   context.CancelFunc
	mutated  []string
}

func (s *web05Store) step(name string) error {
	s.events = append(s.events, name)
	if name == s.failAt {
		if s.cancel != nil {
			s.cancel()
		}
		return s.err
	}
	return nil
}

func (s *web05Store) called(name string) bool {
	for _, event := range s.events {
		if event == name {
			return true
		}
	}
	return false
}

func (s *web05Store) Graph(_ context.Context, graphID model.ID) (model.Graph, error) {
	graph := s.graph
	graph.ID = graphID
	if graph.Name == "" {
		graph.Name = "테스트 그래프"
	}
	return graph, s.step("graph")
}
func (s *web05Store) DeletionImpact(_ context.Context, _ model.ID, contextID model.ID) (model.DeletionImpact, error) {
	name := "graph_impact"
	if !contextID.IsZero() {
		name = "context_impact"
	}
	return s.impacts[contextID], s.step(name)
}
func (s *web05Store) ListActiveContexts(ctx context.Context, graphID model.ID, limit int) ([]model.Context, bool, error) {
	active, truncated, _ := s.fakeGraphStore.ListActiveContexts(ctx, graphID, limit)
	return active, truncated, s.step("active")
}
func (s *web05Store) ListDeletedContexts(ctx context.Context, graphID model.ID, limit int) ([]model.Context, bool, error) {
	deleted, truncated, _ := s.fakeGraphStore.ListDeletedContexts(ctx, graphID, limit)
	return deleted, truncated, s.step("deleted")
}
func (s *web05Store) Context(ctx context.Context, graphID, contextID model.ID) (model.Context, error) {
	value, err := s.fakeGraphStore.Context(ctx, graphID, contextID)
	if stepErr := s.step("context"); stepErr != nil {
		return model.Context{}, stepErr
	}
	return value, err
}
func (s *web05Store) GraphVisualization(ctx context.Context, graphID model.ID, hops, limit int) (store.HopResult, error) {
	s.events = append(s.events, "visualization")
	return s.fakeGraphStore.GraphVisualization(ctx, graphID, hops, limit)
}
func (s *web05Store) SetGraphDeleted(_ context.Context, _, _ model.ID, deleted bool) error {
	s.mutated = append(s.mutated, fmt.Sprintf("graph:%t", deleted))
	return s.err
}
func (s *web05Store) SetContextDeleted(_ context.Context, _, contextID, _ model.ID, deleted bool, _ int64) (model.Context, error) {
	s.mutated = append(s.mutated, fmt.Sprintf("context:%s:%t", contextID, deleted))
	return model.Context{}, s.err
}
func (s *web05Store) PendingRestoreRequests(context.Context) ([]model.RestoreRequest, error) {
	return s.requests, s.step("pending")
}
func (s *web05Store) OperatorRestoreGraph(context.Context, model.ID, model.ID) error {
	s.mutated = append(s.mutated, "operator")
	return s.err
}

func newWEB05Server(t *testing.T, backend *web05Store) *Server {
	t.Helper()
	server, err := New(fakeAuthentication{accountID: backend.accountID}, backend, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func web05Get(t *testing.T, server *Server, target string, status int) string {
	t.Helper()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, sessionRequest(http.MethodGet, target, nil))
	if response.Code != status {
		t.Fatalf("GET %s = %d, want %d", target, response.Code, status)
	}
	return response.Body.String()
}

func web05Contexts(t *testing.T, graphID model.ID) (model.Context, model.Context) {
	active := model.Context{ID: testID(t), GraphID: graphID, Layer: model.LayerSource, Body: "활성 본문"}
	now := time.Now()
	removed := model.Context{ID: testID(t), GraphID: graphID, Layer: model.LayerDerived, Body: "삭제된 본문", DeletedAt: &now}
	return active, removed
}

// TestWEB05DeletionViews는 삭제 대상과 복구 대상을 별도 보기로 나누고 각 보기가 자기
// 목록과 수단만 싣는지 확인한다.
func TestWEB05DeletionViews(t *testing.T) {
	account, graph := testID(t), testID(t)
	active, removed := web05Contexts(t, graph)
	backend := &web05Store{fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner, active: []model.Context{active}, deleted: []model.Context{removed}}}
	server := newWEB05Server(t, backend)
	base := "/graphs/" + graph.String() + "/deletion"

	body := web05Get(t, server, base, http.StatusOK)
	for _, want := range []string{`href="` + base + `" aria-current="page">삭제`, active.ID.String(), `name="graph_name"`, `aria-describedby="graph-name-help"`, `autocomplete="off"`, `<label for="graph-name-confirm">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("삭제 보기에 %q가 없다", want)
		}
	}
	if strings.Contains(body, removed.ID.String()) || strings.Contains(body, "restore_context") || backend.called("deleted") {
		t.Fatal("삭제 보기가 복구 대상을 읽거나 표시했다")
	}

	backend.events = nil
	body = web05Get(t, server, base+"?tab=restore", http.StatusOK)
	for _, want := range []string{`href="` + base + `?tab=restore" aria-current="page">복구 대상`, removed.ID.String(), "restore_context", "에이전트가 폐기한 노드"} {
		if !strings.Contains(body, want) {
			t.Fatalf("복구 보기에 %q가 없다", want)
		}
	}
	if strings.Contains(body, active.ID.String()) || strings.Contains(body, `name="graph_name"`) || backend.called("active") || backend.called("graph_impact") {
		t.Fatal("복구 보기가 삭제 대상을 읽거나 표시했다")
	}

	web05Get(t, server, base+"?tab=unknown", http.StatusBadRequest)
	if body := web05Get(t, server, base+"?tab=restore&context_id="+active.ID.String(), http.StatusNotFound); !strings.Contains(body, "삭제된 컨텍스트를 찾을 수 없습니다.") {
		t.Fatal("복구 보기가 활성 컨텍스트를 선택했다")
	}
	if body := web05Get(t, server, base+"?context_id="+removed.ID.String(), http.StatusNotFound); !strings.Contains(body, "활성 컨텍스트를 찾을 수 없습니다.") {
		t.Fatal("삭제 보기가 삭제된 컨텍스트를 선택했다")
	}
	if body := web05Get(t, server, base+"?tab=restore&context_id="+removed.ID.String(), http.StatusOK); !strings.Contains(body, "이 노드 복구") {
		t.Fatal("목록 밖 복구 대상을 직접 열 수 없다")
	}
}

// TestWEB05DeletedGraphViews는 삭제된 그래프에서 노드 변경 수단을 감추고, 직접 삭제만
// 웹 복구를 제공하며 자동 삭제는 요청 경로로 안내하는지 확인한다.
func TestWEB05DeletedGraphViews(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%t", automatic), func(t *testing.T) {
			account, graph := testID(t), testID(t)
			active, removed := web05Contexts(t, graph)
			deletedAt := time.Now()
			value := model.Graph{Name: "삭제된 그래프", DeletedAt: &deletedAt}
			if automatic {
				value.GraceStartedAt = &deletedAt
			}
			backend := &web05Store{graph: value, fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner, active: []model.Context{active}, deleted: []model.Context{removed}}}
			server := newWEB05Server(t, backend)
			base := "/graphs/" + graph.String() + "/deletion"

			body := web05Get(t, server, base+"?context_id="+active.ID.String(), http.StatusOK)
			if !strings.Contains(body, "삭제된 그래프입니다.") || strings.Contains(body, `name="graph_name"`) || strings.Contains(body, "delete_context") || backend.called("active") || backend.called("graph_impact") || backend.called("context") {
				t.Fatal("삭제된 그래프의 삭제 보기가 변경 수단이나 대상을 실었다")
			}
			body = web05Get(t, server, base+"?tab=restore", http.StatusOK)
			if !strings.Contains(body, removed.ID.String()) || strings.Contains(body, "restore_context") || !strings.Contains(body, "그래프를 먼저 복구해야") {
				t.Fatal("삭제된 그래프의 노드 복구 안내가 다르다")
			}
			graphRestore := strings.Contains(body, `name="action" value="restore"`)
			if graphRestore == automatic || automatic != strings.Contains(body, "운영자가 처리합니다") {
				t.Fatalf("그래프 복구 수단 = %t, 자동 삭제 = %t", graphRestore, automatic)
			}
		})
	}
}

// TestWEB05SelectedImpactByLayer는 SRS 「연쇄 영향 안내」의 계층별 항목만 보여 주는지 확인한다.
func TestWEB05SelectedImpactByLayer(t *testing.T) {
	account, graph := testID(t), testID(t)
	source := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerSource, Body: "원천"}
	derived := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerDerived, Body: "파생"}
	event := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerEvent, Body: "사건"}
	backend := &web05Store{
		fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner, active: []model.Context{source, derived, event}},
		impacts: map[model.ID]model.DeletionImpact{
			{}:        {Contexts: 3, Relations: 4, Accounts: 2},
			source.ID: {Derived: 5, Events: 6}, derived.ID: {Derived: 7}, event.ID: {Relations: 8},
		},
	}
	server := newWEB05Server(t, backend)
	base := "/graphs/" + graph.String() + "/deletion"
	if body := web05Get(t, server, base, http.StatusOK); !strings.Contains(body, "활성 컨텍스트 3개, 확정 관계 4개, 접근이 차단될 계정 2개") {
		t.Fatal("그래프 삭제 영향이 다르다")
	}
	for _, sample := range []struct {
		value model.Context
		want  []string
		not   string
	}{
		{source, []string{"활성 파생 5개", "활성 사건 6개", "근거 무효"}, "확정 관계 0개"},
		{derived, []string{"활성 파생 7개", "근거 무효"}, "활성 사건"},
		{event, []string{"확정 관계 8개", "함께 폐기"}, "활성 파생"},
	} {
		body := web05Get(t, server, base+"?context_id="+sample.value.ID.String(), http.StatusOK)
		selected := body[strings.Index(body, "<h2>노드 삭제</h2>"):]
		for _, want := range sample.want {
			if !strings.Contains(selected, want) {
				t.Fatalf("%s 영향에 %q가 없다", sample.value.Layer, want)
			}
		}
		if strings.Contains(selected, sample.not) {
			t.Fatalf("%s 영향에 다른 계층 항목 %q가 있다", sample.value.Layer, sample.not)
		}
	}
}

// TestWEB05DeletionMutationResponses는 래핑된 저장 오류도 같은 상태·고정 안내로 바꾸고
// 성공은 조작한 보기로 303 이동하는지 확인한다.
func TestWEB05DeletionMutationResponses(t *testing.T) {
	internal := errors.New("secret database detail")
	samples := []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"success", nil, http.StatusSeeOther, ""},
		{"not_found", store.ErrNotFound, http.StatusNotFound, "대상을 찾을 수 없거나 접근 권한이 변경되었습니다."},
		{"last_owner", store.ErrLastOwner, http.StatusConflict, "마지막 소유자 등급이 사라지는 처리는 할 수 없습니다."},
		{"invalid_state", store.ErrInvalidState, http.StatusConflict, "현재 상태에서는 처리할 수 없습니다."},
		{"storage", plan.LimitError{Name: "stored_characters_per_graph", Current: 11, Allowed: 10}, http.StatusConflict, "현재 11자, 허용 10자"},
		{"internal", internal, http.StatusInternalServerError, "삭제·복구 처리 중 오류가 발생했습니다."},
	}
	for _, action := range []string{"delete", "restore", "delete_context", "restore_context"} {
		for _, sample := range samples {
			for _, wrapped := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/wrapped=%t", action, sample.name, wrapped), func(t *testing.T) {
					account, graph, contextID := testID(t), testID(t), testID(t)
					err := sample.err
					if wrapped && err != nil {
						err = fmt.Errorf("저장 문맥: %w", err)
					}
					backend := &web05Store{err: err, fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner}}
					form := url.Values{"action": {action}, "graph_name": {"테스트 그래프"}, "context_id": {contextID.String()}}
					response := httptest.NewRecorder()
					newWEB05Server(t, backend).ServeHTTP(response, sessionRequest(http.MethodPost, "/graphs/"+graph.String()+"/deletion", form))
					body := response.Body.String()
					if response.Code != sample.status || !strings.Contains(body, sample.message) || strings.Contains(body, "secret database") || strings.Contains(body, "저장 문맥") {
						t.Fatalf("응답 = %d %s", response.Code, body)
					}
					want := map[string]string{
						"delete": "graph:true", "restore": "graph:false",
						"delete_context": "context:" + contextID.String() + ":true", "restore_context": "context:" + contextID.String() + ":false",
					}[action]
					if len(backend.mutated) != 1 || backend.mutated[0] != want {
						t.Fatalf("상태 변경 = %v, want %s", backend.mutated, want)
					}
					if sample.err != nil {
						return
					}
					location := map[string]string{
						"delete": "/graphs", "restore": "/graphs",
						"delete_context": "/graphs/" + graph.String() + "/deletion", "restore_context": "/graphs/" + graph.String() + "/deletion?tab=restore",
					}[action]
					if response.Header().Get("Location") != location {
						t.Fatalf("성공 이동 = %q, want %q", response.Header().Get("Location"), location)
					}
				})
			}
		}
	}
}

// TestWEB05GraphNameConfirmation은 이름이 정확히 같을 때만 그래프 삭제를 호출하는지 확인한다.
func TestWEB05GraphNameConfirmation(t *testing.T) {
	for _, name := range []string{"", "테스트", "테스트 그래프 ", "테스트 그래프"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			account, graph := testID(t), testID(t)
			backend := &web05Store{fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner}}
			response := httptest.NewRecorder()
			newWEB05Server(t, backend).ServeHTTP(response, sessionRequest(http.MethodPost, "/graphs/"+graph.String()+"/deletion", url.Values{"action": {"delete"}, "graph_name": {name}}))
			matched := name == "테스트 그래프"
			if matched && (response.Code != http.StatusSeeOther || len(backend.mutated) != 1) {
				t.Fatalf("일치한 이름의 삭제 = %d %v", response.Code, backend.mutated)
			}
			if !matched && (response.Code != http.StatusBadRequest || len(backend.mutated) != 0) {
				t.Fatalf("다른 이름의 삭제 = %d %v", response.Code, backend.mutated)
			}
		})
	}
}

// TestWEB05DeletionReadBoundaries는 조회 단계마다 저장 장애를 고정 500으로, 취소를 무응답으로
// 끝내고 이후 조회·렌더링을 진행하지 않는지 확인한다.
func TestWEB05DeletionReadBoundaries(t *testing.T) {
	for _, sample := range []struct{ query, phase string }{
		{"", "graph_impact"}, {"", "active"}, {"?context_id=%s", "context"}, {"?context_id=%s", "context_impact"},
		{"?tab=restore", "deleted"}, {"?tab=restore&context_id=%s", "context"},
	} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/canceled=%t", sample.query, sample.phase, canceled), func(t *testing.T) {
				account, graph := testID(t), testID(t)
				active, removed := web05Contexts(t, graph)
				backend := &web05Store{failAt: sample.phase, err: errors.New("private database details"), fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner, active: []model.Context{active}, deleted: []model.Context{removed}}}
				target := active.ID
				if strings.Contains(sample.query, "restore") {
					target = removed.ID
				}
				query := sample.query
				if strings.Contains(query, "%s") {
					query = fmt.Sprintf(query, target)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if canceled {
					backend.cancel = cancel
				}
				response := httptest.NewRecorder()
				newWEB05Server(t, backend).ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graph.String()+"/deletion"+query, nil).WithContext(ctx))
				if backend.events[len(backend.events)-1] != sample.phase || strings.Contains(response.Body.String(), "private database") {
					t.Fatalf("단계 = %v", backend.events)
				}
				if canceled && response.Body.Len() != 0 || !canceled && response.Code != http.StatusInternalServerError {
					t.Fatalf("취소 또는 저장 장애 응답 = %d", response.Code)
				}
			})
		}
	}
}

// TestWEB05OperatorConfirmation은 운영자 화면이 요청·그래프 메타데이터만 읽고, 선택한
// 그래프의 가장 오래된 요청을 처리 확인으로 보여 주는지 확인한다.
func TestWEB05OperatorConfirmation(t *testing.T) {
	operator, graph, other := testID(t), testID(t), testID(t)
	first, second, third := testID(t), testID(t), testID(t)
	at := time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)
	backend := &web05Store{fakeGraphStore: &fakeGraphStore{accountID: operator}, requests: []model.RestoreRequest{
		{GraphID: graph, GraphName: "<같은 이름>", RequestedBy: first, RequestedAt: at},
		{GraphID: other, GraphName: "<같은 이름>", RequestedBy: third, RequestedAt: at.Add(time.Minute)},
		{GraphID: graph, GraphName: "<같은 이름>", RequestedBy: second, RequestedAt: at.Add(2 * time.Minute)},
	}}
	server := newWEB05Server(t, backend)

	body := web05Get(t, server, "/operator/restores", http.StatusOK)
	for _, want := range []string{"<caption>대기 중인 복구 요청</caption>", `<th scope="col">요청 계정</th>`, graph.String(), other.String(), second.String(), "/operator/restores?graph_id=" + graph.String(), "&lt;같은 이름&gt;"} {
		if !strings.Contains(body, want) {
			t.Fatalf("대기 목록에 %q가 없다", want)
		}
	}
	if strings.Contains(body, `name="graph_id"`) || strings.Contains(body, "복구 처리 확인") {
		t.Fatal("선택 전 목록에 처리 양식이 있다")
	}

	body = web05Get(t, server, "/operator/restores?graph_id="+graph.String(), http.StatusOK)
	confirm := body[strings.Index(body, "복구 처리 확인"):strings.Index(body, "</section>")]
	if !strings.Contains(confirm, first.String()) || strings.Contains(confirm, second.String()) || !strings.Contains(confirm, `name="graph_id" value="`+graph.String()+`"`) || !strings.Contains(confirm, formatTimeForTest(at)) {
		t.Fatal("처리 확인이 가장 오래된 요청을 가리키지 않는다")
	}
	if body := web05Get(t, server, "/operator/restores?graph_id="+testID(t).String(), http.StatusNotFound); !strings.Contains(body, "처리할 복구 요청을 찾을 수 없습니다.") || !strings.Contains(body, other.String()) {
		t.Fatal("요청 없는 그래프의 처리 확인 응답이 다르다")
	}
	web05Get(t, server, "/operator/restores?graph_id=not-an-id", http.StatusBadRequest)
	for _, name := range []string{"graph", "context", "active", "deleted", "visualization"} {
		if backend.called(name) {
			t.Fatalf("운영자 화면이 %s를 읽었다", name)
		}
	}
	if len(backend.mutated) != 0 {
		t.Fatal("처리 확인 조회가 상태를 바꿨다")
	}
}

func formatTimeForTest(value time.Time) string { return value.UTC().Format(time.RFC3339) }

// TestWEB05OperatorRestoreResponses는 래핑된 처리 오류의 상태와 성공 이동을 확인한다.
func TestWEB05OperatorRestoreResponses(t *testing.T) {
	for _, sample := range []struct {
		err    error
		status int
	}{
		{nil, http.StatusSeeOther}, {store.ErrNotFound, http.StatusNotFound}, {store.ErrInvalidState, http.StatusConflict},
		{store.ErrLastOwner, http.StatusConflict}, {errors.New("secret database detail"), http.StatusInternalServerError},
	} {
		t.Run(fmt.Sprint(sample.err), func(t *testing.T) {
			operator, graph := testID(t), testID(t)
			err := sample.err
			if err != nil {
				err = fmt.Errorf("운영자 문맥: %w", err)
			}
			backend := &web05Store{err: err, fakeGraphStore: &fakeGraphStore{accountID: operator}}
			response := httptest.NewRecorder()
			newWEB05Server(t, backend).ServeHTTP(response, sessionRequest(http.MethodPost, "/operator/restores", url.Values{"graph_id": {graph.String()}}))
			if response.Code != sample.status || strings.Contains(response.Body.String(), "secret database") || strings.Contains(response.Body.String(), "운영자 문맥") {
				t.Fatalf("운영자 처리 응답 = %d %s", response.Code, response.Body.String())
			}
			if sample.err == nil && response.Header().Get("Location") != "/operator/restores" {
				t.Fatal("성공 이동이 다르다")
			}
		})
	}
}
