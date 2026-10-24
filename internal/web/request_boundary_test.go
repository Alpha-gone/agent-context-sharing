package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func TestWEB02SafeMethodsRejectBeforeSessionAndBody(t *testing.T) {
	graph := testID(t).String()
	for _, path := range []string{
		"/login", "/register", "/logout", "/graphs", "/access", "/operator/restores", "/connections", "/authorize",
		"/graphs/" + graph, "/graphs/" + graph + "/access", "/graphs/" + graph + "/deletion", "/graphs/" + graph + "/audit",
	} {
		for _, method := range []string{http.MethodHead, http.MethodOptions} {
			t.Run(method+" "+path, func(t *testing.T) {
				// 호출을 허용하지 않는 경계로 인증·데이터·변경의 잘못된 실행도 검출한다.
				server, err := New(&web02Authentication{}, &web02Store{}, Config{SecureCookie: func(*http.Request) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(method, path+"?action=delete", nil)
				request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
				request.Body = unreadableCSRFBody{t: t}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") == "" || response.Header().Get("Cache-Control") != "no-store" || len(response.Result().Cookies()) != 0 {
					t.Fatalf("안전 메서드의 변경/조회 거부 실패: %d, %v", response.Code, response.Header())
				}
			})
		}
	}
}

func TestWEB02PublicGETDoesNotAuthenticateOrRevoke(t *testing.T) {
	for _, path := range []string{"/login", "/register", "/logout"} {
		t.Run(path, func(t *testing.T) {
			server, err := New(&web02Authentication{}, &web02Store{}, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, path+"?login_id=ignored&password=ignored&action=delete", nil)
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "ignored"})
			request.Body = unreadableCSRFBody{t: t}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK || len(response.Result().Cookies()) != 0 || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("공개 GET이 자격·폐기·쿠키를 변경했다: %d", response.Code)
			}
		})
	}
}

func TestWEB02ProtectedRequestOrderAndIsolation(t *testing.T) {
	for _, mode := range []string{
		"valid", "missing cookie", "invalid account", "session unavailable", "grade unavailable", "grade missing",
		"graph unavailable", "graph missing", "wrong graph", "wrong node graph", "outside source", "outside target", "deleted node",
		"canceled before", "canceled grade", "canceled graph", "canceled visualization",
	} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var events []string
			graphID, accountID, first, second := testID(t), testID(t), testID(t), testID(t)
			auth := &web02Authentication{account: accountID, events: &events}
			graphs := &web02Store{
				events: &events, graph: model.Graph{ID: graphID, Name: "안전한 표본"}, found: true,
				hops:   store.HopResult{Contexts: []model.Context{{ID: first, GraphID: graphID}, {ID: second, GraphID: graphID}}, Edges: []store.HopEdge{{FromID: first, ToID: second, Kind: "derived_from"}}},
				cancel: cancel,
			}
			wantStatus, wantEvents := http.StatusOK, []string{"session", "grade", "graph", "visualization"}
			switch mode {
			case "missing cookie":
				wantStatus, wantEvents = 303, nil
			case "invalid account":
				auth.account = model.ID{}
				wantStatus, wantEvents = 303, []string{"session"}
			case "session unavailable":
				auth.failure = fmt.Errorf("%w: internal-web02-marker", ErrAuthenticationUnavailable)
				wantStatus, wantEvents = 500, []string{"session"}
			case "grade unavailable":
				graphs.gradeError = errors.New("internal-web02-marker")
				wantStatus, wantEvents = 500, []string{"session", "grade"}
			case "grade missing":
				graphs.found = false
				wantStatus, wantEvents = 404, []string{"session", "grade"}
			case "graph unavailable":
				graphs.graphError = errors.New("internal-web02-marker")
				wantStatus, wantEvents = 500, []string{"session", "grade", "graph"}
			case "graph missing":
				graphs.graphError = fmt.Errorf("wrapped: %w", store.ErrNotFound)
				wantStatus, wantEvents = 404, []string{"session", "grade", "graph"}
			case "wrong graph":
				graphs.graph.ID = testID(t)
				graphs.graph.Name = "foreign-web02-marker"
				wantStatus, wantEvents = 500, []string{"session", "grade", "graph"}
			case "wrong node graph":
				graphs.hops.Contexts[0].GraphID = testID(t)
				graphs.hops.Contexts[0].Body = "foreign-web02-marker"
				wantStatus = 500
			case "outside source":
				graphs.hops.Edges[0].FromID = testID(t)
				wantStatus = 500
			case "outside target":
				graphs.hops.Edges[0].ToID = testID(t)
				wantStatus = 500
			case "deleted node":
				graphs.hops.Contexts[0].DeletedAt = new(time.Now())
				graphs.hops.Contexts[0].Body = "foreign-web02-marker"
				wantStatus = 500
			case "canceled before":
				cancel()
				wantStatus, wantEvents = 200, nil
			case "canceled grade":
				graphs.cancelAt = "grade"
				wantStatus, wantEvents = 200, []string{"session", "grade"}
			case "canceled graph":
				graphs.cancelAt = "graph"
				wantStatus, wantEvents = 200, []string{"session", "grade", "graph"}
			case "canceled visualization":
				graphs.cancelAt = "visualization"
				wantStatus = 200
			}
			server, err := New(auth, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/graphs/"+graphID.String()+"?account_id="+accountID.String()+"&token=ignored", nil).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer ignored")
			if mode != "missing cookie" {
				request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if !reflect.DeepEqual(events, wantEvents) || response.Code != wantStatus || response.Header().Get("Cache-Control") != "no-store" || len(response.Result().Cookies()) != 0 {
				t.Fatalf("요청 순서 = %v, want %v; 상태 = %d, want %d", events, wantEvents, response.Code, wantStatus)
			}
			if mode != "valid" && strings.Contains(response.Body.String(), "foreign-web02-marker") || strings.Contains(response.Body.String(), "internal-web02-marker") {
				t.Fatal("다른 그래프·내부 원문 노출")
			}
			if strings.HasPrefix(mode, "canceled") && (response.Body.Len() != 0 || response.Header().Get("Location") != "") {
				t.Fatal("취소 뒤 성공·오류 응답 생성")
			}
			if wantStatus == 303 && response.Header().Get("Location") != "/login" {
				t.Fatal("잘못된 재로그인 경로")
			}
		})
	}
}

func TestWEB02DeletionReadBoundary(t *testing.T) {
	for _, mode := range []string{"valid", "graph unavailable", "graph missing", "wrong graph", "active cross graph", "deleted cross graph", "context cross graph", "wrong context id", "context unavailable", "context missing", "context deleted"} {
		t.Run(mode, func(t *testing.T) {
			var events []string
			account, graph, node := testID(t), testID(t), testID(t)
			graphs := &web02Store{events: &events, found: true, graph: model.Graph{ID: graph}, selected: model.Context{ID: node, GraphID: graph, Body: "선택 본문"}}
			want := http.StatusOK
			switch mode {
			case "graph unavailable":
				graphs.graphError = errors.New("internal-web02-marker")
				want = 500
			case "graph missing":
				graphs.graphError = fmt.Errorf("wrapped: %w", store.ErrNotFound)
				want = 404
			case "wrong graph":
				graphs.graph.ID = testID(t)
				want = 500
			case "active cross graph":
				graphs.active = []model.Context{{ID: node, GraphID: testID(t), Body: "foreign-web02-marker"}}
				want = 500
			case "deleted cross graph":
				graphs.deleted = []model.Context{{ID: node, GraphID: testID(t), Body: "foreign-web02-marker"}}
				want = 500
			case "context cross graph":
				graphs.selected.GraphID = testID(t)
				graphs.selected.Body = "foreign-web02-marker"
				want = 500
			case "wrong context id":
				graphs.selected.ID = testID(t)
				graphs.selected.Body = "foreign-web02-marker"
				want = 500
			case "context unavailable":
				graphs.contextError = errors.New("internal-web02-marker")
				want = 500
			case "context missing":
				graphs.contextError = fmt.Errorf("wrapped: %w", store.ErrNotFound)
				want = 404
			case "context deleted":
				graphs.selected.DeletedAt = new(time.Now())
				want = 404
			}
			server, err := New(&web02Authentication{account: account, events: &events}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			target := "/graphs/" + graph.String() + "/deletion?context_id=" + node.String()
			if mode == "deleted cross graph" {
				// 삭제된 목록은 복구 대상 보기만 읽는다.
				target = "/graphs/" + graph.String() + "/deletion?tab=restore"
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodGet, target, nil))
			body := response.Body.String()
			if response.Code != want || strings.Contains(body, "internal-web02-marker") || strings.Contains(body, "foreign-web02-marker") {
				t.Fatalf("조회 경계 = %d, want %d", response.Code, want)
			}
			if want != 200 && strings.Contains(body, "선택 본문") {
				t.Fatal("거부한 데이터의 일부 본문 노출")
			}
		})
	}
}

type web02Authentication struct {
	Authentication
	account model.ID
	failure error
	events  *[]string
}

func (auth *web02Authentication) VerifyWebSession(_ context.Context, raw, audience string) (model.ID, Session, error) {
	if auth.events == nil {
		panic("unexpected authentication call")
	}
	*auth.events = append(*auth.events, "session")
	if raw != "web-session" || audience != SessionAudience {
		panic("unexpected credentials")
	}
	return auth.account, Session{}, auth.failure
}

type web02Store struct {
	GraphStore
	events                               *[]string
	found                                bool
	gradeError, graphError, contextError error
	graph                                model.Graph
	hops                                 store.HopResult
	active, deleted                      []model.Context
	selected                             model.Context
	cancel                               context.CancelFunc
	cancelAt                             string
}

func (graphs *web02Store) event(name string) {
	*graphs.events = append(*graphs.events, name)
	if graphs.cancelAt == name {
		graphs.cancel()
	}
}
func (graphs *web02Store) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	graphs.event("grade")
	return model.GraphGradeOwner, graphs.found, graphs.gradeError
}
func (graphs *web02Store) Graph(context.Context, model.ID) (model.Graph, error) {
	graphs.event("graph")
	return graphs.graph, graphs.graphError
}
func (graphs *web02Store) GraphVisualization(context.Context, model.ID, int, int) (store.HopResult, error) {
	graphs.event("visualization")
	return graphs.hops, nil
}
func (graphs *web02Store) DeletionImpact(context.Context, model.ID, model.ID) (model.DeletionImpact, error) {
	graphs.event("impact")
	return model.DeletionImpact{}, nil
}
func (graphs *web02Store) ListActiveContexts(context.Context, model.ID, int) ([]model.Context, bool, error) {
	graphs.event("active")
	return graphs.active, false, nil
}
func (graphs *web02Store) ListDeletedContexts(context.Context, model.ID, int) ([]model.Context, bool, error) {
	graphs.event("deleted")
	return graphs.deleted, false, nil
}
func (graphs *web02Store) Context(context.Context, model.ID, model.ID) (model.Context, error) {
	graphs.event("context")
	return graphs.selected, graphs.contextError
}
