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

// matrixStore는 화면 행렬에서 지정한 조회 하나를 내부 장애로 만든다.
type matrixStore struct {
	*fakeGraphStore
	fail string
}

var errMatrix = errors.New("private matrix storage detail")

func (s *matrixStore) failing(name string) error {
	if s.fail == name {
		return errMatrix
	}
	return nil
}
func (s *matrixStore) ListGraphs(ctx context.Context, account model.ID, filter model.GraphListFilter, cursor string, limit int) ([]model.GraphListItem, string, error) {
	items, next, _ := s.fakeGraphStore.ListGraphs(ctx, account, filter, cursor, limit)
	return items, next, s.failing("list")
}
func (s *matrixStore) GraphVisualization(ctx context.Context, graph model.ID, hops, limit int) (store.HopResult, error) {
	if err := s.failing("detail"); err != nil {
		return store.HopResult{}, err
	}
	return store.HopResult{}, nil
}
func (s *matrixStore) ListGraphGrants(ctx context.Context, graph, actor model.ID) ([]model.GrantSubject, error) {
	grants, _ := s.fakeGraphStore.ListGraphGrants(ctx, graph, actor)
	return grants, s.failing("access")
}
func (s *matrixStore) DeletionImpact(ctx context.Context, graph, contextID model.ID) (model.DeletionImpact, error) {
	return model.DeletionImpact{}, s.failing("deletion")
}
func (s *matrixStore) ListAuditEntries(context.Context, model.ID, model.ID, model.AuditFilter, string, int) ([]model.AuditEntry, string, error) {
	return nil, "", s.failing("audit")
}
func (s *matrixStore) PendingRestoreRequests(context.Context) ([]model.RestoreRequest, error) {
	return nil, s.failing("operator")
}

type matrixCase struct {
	name, method, path string
	form               url.Values
	grade              model.GraphGrade
	fail               string
	credential         error
	connection         error
	noSession          bool
	status             int
	location, contains string
}

// TestWEB09ScreenMatrix는 여섯 업무 화면·계정 화면·연결 관리의 정상·빈 목록·입력 실패·
// 등급 부족·내부 장애·성공 이동과 세션 없는 접근을 한 행렬로 HTTP 검사한다.
func TestWEB09ScreenMatrix(t *testing.T) {
	account, graph := testID(t), testID(t)
	g := "/graphs/" + graph.String()
	owner, viewer := model.GraphGradeOwner, model.GraphGradeViewer
	cases := []matrixCase{
		{name: "목록/정상·빈 목록", method: "GET", path: "/graphs", status: 200, contains: "접근 가능한 그래프가 없습니다."},
		{name: "목록/입력 실패", method: "GET", path: "/graphs?grade=admin", status: 400, contains: "등급 필터 값이 올바르지 않습니다."},
		{name: "목록/내부 장애", method: "GET", path: "/graphs", fail: "list", status: 500, contains: "그래프 목록을 읽을 수 없습니다."},
		{name: "목록/성공 이동", method: "POST", path: "/graphs", form: url.Values{"restore_graph_id": {graph.String()}}, status: 303, location: "/graphs"},
		{name: "목록/세션 없음", method: "GET", path: "/graphs", noSession: true, status: 303, location: "/login"},
		{name: "상세/정상·빈 그래프", method: "GET", path: g, grade: viewer, status: 200, contains: "표시할 활성 컨텍스트가 없습니다."},
		{name: "상세/등급 없음", method: "GET", path: g, grade: "none", status: 404, contains: "그래프를 찾을 수 없습니다."},
		{name: "상세/내부 장애", method: "GET", path: g, grade: viewer, fail: "detail", status: 500, contains: "시각화 데이터를 읽을 수 없습니다."},
		{name: "권한/정상·빈 팀", method: "GET", path: g + "/access", grade: owner, status: 200, contains: "관리하는 팀이 없습니다."},
		{name: "권한/입력 실패", method: "POST", path: g + "/access", grade: owner, form: url.Values{"action": {"grant_team"}, "team_id": {"bad"}, "grade": {"viewer"}}, status: 400, contains: "팀 식별자가 올바르지 않습니다."},
		{name: "권한/등급 부족", method: "POST", path: g + "/access", grade: viewer, form: url.Values{"action": {"grant_account"}, "login_id": {"someone"}, "grade": {"viewer"}}, status: 403, contains: "등급을 바꿀 권한이 없습니다."},
		{name: "권한/내부 장애", method: "GET", path: g + "/access", grade: owner, fail: "access", status: 500},
		{name: "권한/성공 이동", method: "POST", path: g + "/access", grade: owner, form: url.Values{"action": {"create_team"}, "team_name": {"팀"}}, status: 303, location: g + "/access"},
		{name: "삭제/정상·빈 목록", method: "GET", path: g + "/deletion", grade: owner, status: 200, contains: "삭제할 활성 컨텍스트가 없습니다."},
		{name: "삭제/입력 실패", method: "POST", path: g + "/deletion", grade: owner, form: url.Values{"action": {"delete_context"}, "context_id": {"bad"}}, status: 400, contains: "컨텍스트 식별자가 올바르지 않습니다."},
		{name: "삭제/등급 부족", method: "GET", path: g + "/deletion", grade: viewer, status: 403, contains: "이 화면에 접근할 권한이 없습니다."},
		{name: "삭제/내부 장애", method: "GET", path: g + "/deletion", grade: owner, fail: "deletion", status: 500, contains: "삭제 영향을 읽을 수 없습니다."},
		{name: "삭제/성공 이동", method: "POST", path: g + "/deletion", grade: owner, form: url.Values{"action": {"delete_context"}, "context_id": {graph.String()}}, status: 303, location: g + "/deletion"},
		{name: "감사/정상·빈 목록", method: "GET", path: g + "/audit", grade: owner, status: 200, contains: "조건에 맞는 감사 기록이 없습니다."},
		{name: "감사/입력 실패", method: "GET", path: g + "/audit?from=bad", grade: owner, status: 400, contains: "시작일은 YYYY-MM-DD 형식의 날짜여야 합니다."},
		{name: "감사/등급 부족", method: "GET", path: g + "/audit", grade: viewer, status: 403},
		{name: "감사/내부 장애", method: "GET", path: g + "/audit", grade: owner, fail: "audit", status: 500, contains: "감사 기록을 읽을 수 없습니다."},
		{name: "운영자/정상·빈 목록", method: "GET", path: "/operator/restores", status: 200, contains: "대기 중인 요청이 없습니다."},
		{name: "운영자/입력 실패", method: "GET", path: "/operator/restores?graph_id=bad", status: 400, contains: "그래프 식별자가 올바르지 않습니다."},
		{name: "운영자/내부 장애", method: "GET", path: "/operator/restores", fail: "operator", status: 500, contains: "대기 요청을 읽을 수 없습니다."},
		{name: "운영자/성공 이동", method: "POST", path: "/operator/restores", form: url.Values{"graph_id": {graph.String()}}, status: 303, location: "/operator/restores"},
		{name: "운영자/세션 없음", method: "GET", path: "/operator/restores", noSession: true, status: 303, location: "/login"},
		{name: "로그인/정상", method: "GET", path: "/login", noSession: true, status: 200, contains: "로그인 아이디 (필수)"},
		{name: "로그인/자격 불일치", method: "POST", path: "/login", noSession: true, form: url.Values{"login_id": {"someone"}, "password": {"wrong-password"}}, credential: errors.New("private credential detail"), status: 401, contains: "로그인 아이디 또는 비밀번호가 올바르지 않습니다."},
		{name: "로그인/내부 장애", method: "POST", path: "/login", noSession: true, form: url.Values{"login_id": {"someone"}, "password": {"password1"}}, credential: ErrAuthenticationUnavailable, status: 500},
		{name: "로그인/성공 이동", method: "POST", path: "/login", noSession: true, form: url.Values{"login_id": {"someone"}, "password": {"password1"}}, status: 303, location: "/graphs"},
		{name: "가입/입력 실패", method: "POST", path: "/register", noSession: true, form: url.Values{"login_id": {"A"}, "password": {"short"}}, credential: model.FieldError{Field: "login_id", Message: "private"}, status: 400, contains: "형식이 올바르지 않거나 이미 사용 중입니다."},
		{name: "가입/성공 이동", method: "POST", path: "/register", noSession: true, form: url.Values{"login_id": {"someone"}, "password": {"password1"}}, status: 303, location: "/login?registered=1"},
		{name: "연결/정상·빈 목록", method: "GET", path: "/connections", status: 200, contains: "표시할 연결이 없습니다."},
		{name: "연결/입력 실패", method: "POST", path: "/connections", form: url.Values{"action": {"unknown"}, "target_id": {graph.String()}}, status: 400, contains: "연결 요청의 입력을 확인해 주세요."},
		{name: "연결/다른 계정 대상", method: "POST", path: "/connections", form: url.Values{"action": {"revoke_grant"}, "target_id": {graph.String()}}, connection: model.ErrNotFound, status: 404, contains: "대상 연결을 찾을 수 없습니다."},
		{name: "연결/내부 장애", method: "GET", path: "/connections", connection: errMatrix, status: 500},
		{name: "연결/성공 이동", method: "POST", path: "/connections", form: url.Values{"action": {"revoke_agent"}, "target_id": {graph.String()}}, status: 303, location: "/connections"},
		{name: "연결/세션 없음", method: "GET", path: "/connections", noSession: true, status: 303, location: "/login"},
	}
	for _, sample := range cases {
		t.Run(sample.name, func(t *testing.T) {
			graphs := &matrixStore{fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph, grade: sample.grade}, fail: sample.fail}
			if sample.grade == "none" {
				graphs.graphID = model.ID{}
			}
			auth := &credentialBoundaryAuthentication{fakeAuthentication: fakeAuthentication{accountID: account}, account: account, failure: sample.credential}
			server, err := New(auth, graphs, Config{Connections: &connectionStub{err: sample.connection}, SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			var request *http.Request
			if sample.noSession {
				request = httptest.NewRequest(sample.method, sample.path, strings.NewReader(sample.form.Encode()))
				if sample.form != nil {
					request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
			} else {
				request = sessionRequest(sample.method, sample.path, sample.form)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != sample.status || response.Header().Get("Location") != sample.location || !strings.Contains(body, sample.contains) {
				t.Fatalf("응답 = %d %q, want %d %q / %q", response.Code, response.Header().Get("Location"), sample.status, sample.location, sample.contains)
			}
			if strings.Contains(body, "private") || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("내부 원문 노출 또는 동적 화면 캐시 경계 위반")
			}
			if response.Code >= 400 && strings.Contains(response.Header().Get("Content-Type"), "text/html") && !strings.Contains(body, `<html lang="ko">`) {
				t.Fatal("오류 화면이 한국어 문서가 아니다")
			}
		})
	}
}
