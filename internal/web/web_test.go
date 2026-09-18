package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func TestProtectedRoutesRedirectToLogin(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	request := httptest.NewRequest(http.MethodGet, "/graphs", nil)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/login" {
		t.Fatalf("보호된 목록 응답 = %d, Location %q", response.Code, response.Header().Get("Location"))
	}
}

func TestGraphDetailRendersOnlyAfterWebSessionAndGradeCheck(t *testing.T) {
	accountID := testID(t)
	graphID := testID(t)
	server := newTestServer(t, accountID, graphID)
	request := httptest.NewRequest(http.MethodGet, "/graphs/"+graphID.String(), nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("그래프 상세 응답 = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Body.String() == "" {
		t.Fatal("그래프 상세 본문이 비어 있다")
	}
	if strings.Contains(response.Body.String(), "unsafe()</script>") {
		t.Fatal("컨텍스트 본문이 시각화 스크립트 문맥을 벗어났다")
	}
}

func TestCytoscapeAssetIsServedLocally(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	request := httptest.NewRequest(http.MethodGet, "/assets/cytoscape.min.js", nil)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("Cytoscape 자산 응답 = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Body.Len() == 0 {
		t.Fatal("Cytoscape 자산 본문이 비어 있다")
	}
}

func newTestServer(t *testing.T, accountID, graphID model.ID) *Server {
	t.Helper()
	return newTestServerWith(t, accountID, &fakeGraphStore{accountID: accountID, graphID: graphID})
}

func newTestServerWith(t *testing.T, accountID model.ID, graphs *fakeGraphStore) *Server {
	t.Helper()
	server, err := New(fakeAuthentication{accountID: accountID}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatalf("웹 서버 생성: %v", err)
	}
	return server
}

// sessionRequest는 웹 세션 쿠키를 실은 요청을 만든다.
func sessionRequest(method, target string, form url.Values) *http.Request {
	var request *http.Request
	if form == nil {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
	return request
}

func testID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("식별자 생성: %v", err)
	}
	return id
}

type fakeAuthentication struct{ accountID model.ID }

func (fakeAuthentication) Authenticate(context.Context, string, string) (model.ID, error) {
	return model.ID{}, nil
}
func (fakeAuthentication) Register(context.Context, string, string) (model.ID, error) {
	return model.ID{}, nil
}
func (authentication fakeAuthentication) WebSession(_ context.Context, _ model.ID, _ string) (Session, error) {
	return Session{Raw: "web-session", ID: "session", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (authentication fakeAuthentication) VerifyWebSession(_ context.Context, raw, _ string) (model.ID, Session, error) {
	if raw != "web-session" || !authentication.accountID.IsV7() {
		return model.ID{}, Session{}, http.ErrNoCookie
	}
	return authentication.accountID, Session{Raw: raw, ID: "session", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (fakeAuthentication) Revoke(context.Context, Session) error { return nil }

type fakeGraphStore struct {
	accountID    model.ID
	graphID      model.ID
	grade        model.GraphGrade
	active       []model.Context
	deleted      []model.Context
	ownedDeleted []model.Graph
	boundary     *int
	granted      bool
	teamCreated  string
	contextSet   *model.ID
	contextState bool
}

func (fake fakeGraphStore) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	grade := fake.grade
	if grade == "" {
		grade = model.GraphGradeViewer
	}
	return grade, fake.graphID.IsV7(), nil
}
func (fakeGraphStore) ListGraphs(context.Context, model.ID, model.GraphListFilter, string, int) ([]model.GraphListItem, string, error) {
	return nil, "", nil
}
func (store fakeGraphStore) Graph(_ context.Context, graphID model.ID) (model.Graph, error) {
	return model.Graph{ID: graphID, Name: "테스트 그래프"}, nil
}
func (fake fakeGraphStore) GraphVisualization(_ context.Context, graphID model.ID, _, _ int) (store.HopResult, error) {
	if graphID != fake.graphID {
		return store.HopResult{}, nil
	}
	result := store.HopResult{
		Contexts:  []model.Context{{ID: graphID, GraphID: graphID, Layer: model.LayerSource, Body: "</script><script>unsafe()</script>"}},
		Distances: map[model.ID]int{graphID: 0},
	}
	if fake.boundary != nil {
		result.Truncated, result.Boundary = true, *fake.boundary
	}
	return result, nil
}
func (fake fakeGraphStore) ListActiveContexts(context.Context, model.ID, int) ([]model.Context, error) {
	return fake.active, nil
}
func (fake fakeGraphStore) ListDeletedContexts(context.Context, model.ID, int) ([]model.Context, error) {
	return fake.deleted, nil
}
func (fake fakeGraphStore) ListOwnedDeletedGraphs(context.Context, model.ID) ([]model.Graph, error) {
	return fake.ownedDeleted, nil
}
func (fakeGraphStore) ListGraphGrants(context.Context, model.ID) ([]model.GrantSubject, error) {
	return nil, nil
}
func (fakeGraphStore) ListManagedTeams(context.Context, model.ID) ([]model.Team, error) {
	return nil, nil
}
func (fakeGraphStore) AccountByLoginID(context.Context, string) (store.Account, error) {
	return store.Account{}, nil
}
func (fake *fakeGraphStore) GrantGraph(context.Context, model.ID, model.ID, model.ID, store.GrantSubjectType, model.GraphGrade) error {
	fake.granted = true
	return nil
}
func (fakeGraphStore) RevokeGraphGrantWithAudit(context.Context, model.ID, model.ID, model.ID, store.GrantSubjectType) error {
	return nil
}
func (fake *fakeGraphStore) CreateTeam(_ context.Context, _ model.ID, name string) (model.Team, error) {
	fake.teamCreated = name
	return model.Team{}, nil
}
func (fakeGraphStore) AddTeamMember(context.Context, model.ID, model.ID, model.ID) error { return nil }
func (fakeGraphStore) RemoveTeamMemberWithAudit(context.Context, model.ID, model.ID, model.ID) error {
	return nil
}
func (fakeGraphStore) SetTeamDeleted(context.Context, model.ID, model.ID, bool) error { return nil }
func (fakeGraphStore) DeletionImpact(context.Context, model.ID, model.ID) (model.DeletionImpact, error) {
	return model.DeletionImpact{}, nil
}
func (fakeGraphStore) SetGraphDeleted(context.Context, model.ID, model.ID, bool) error { return nil }
func (fake *fakeGraphStore) SetContextDeleted(_ context.Context, _, contextID, _ model.ID, deleted bool) (model.Context, error) {
	fake.contextSet, fake.contextState = &contextID, deleted
	return model.Context{}, nil
}
func (fakeGraphStore) ListRestoreEligibleGraphs(context.Context, model.ID) ([]model.Graph, error) {
	return nil, nil
}
func (fakeGraphStore) RequestGraphRestore(context.Context, model.ID, model.ID) error { return nil }
func (fakeGraphStore) PendingRestoreRequests(context.Context) ([]model.RestoreRequest, error) {
	return nil, nil
}
func (fakeGraphStore) OperatorRestoreGraph(context.Context, model.ID, model.ID) error { return nil }
func (fakeGraphStore) ListAuditEntries(context.Context, model.ID, int) ([]model.AuditEntry, error) {
	return nil, nil
}

// TestAccessScreenHidesGrantsFromNonOwner는 등급 영역이 소유자에게만 보이고 팀 영역은
// 인증된 계정에 열리는지 확인한다. FR-AGENT_CONTEXT-043이 팀 관리를 계정 전부에 열어
// 두었고 FR-AGENT_CONTEXT-041은 등급 부여·회수를 소유자로 좁혔다.
func TestAccessScreenHidesGrantsFromNonOwner(t *testing.T) {
	accountID, graphID := testID(t), testID(t)
	graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, grade: model.GraphGradeViewer}
	server := newTestServerWith(t, accountID, graphs)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graphID.String()+"/access", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("열람자의 권한 화면 응답 = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	if strings.Contains(body, "현재 등급") || strings.Contains(body, "계정 등급 부여") {
		t.Fatal("열람자에게 등급 영역이 보인다")
	}
	if !strings.Contains(body, "관리 팀") {
		t.Fatal("열람자에게 팀 영역이 보이지 않는다")
	}
}

// TestAccessScreenRejectsGrantChangeFromNonOwner는 소유자가 아닌 계정의 등급 변경이
// 저장소에 닿기 전에 막히는지 확인한다.
func TestAccessScreenRejectsGrantChangeFromNonOwner(t *testing.T) {
	accountID, graphID := testID(t), testID(t)
	graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, grade: model.GraphGradeEditor}
	server := newTestServerWith(t, accountID, graphs)
	form := url.Values{"action": {"grant_account"}, "login_id": {"someone"}, "grade": {"owner"}}
	response := httptest.NewRecorder()

	server.ServeHTTP(response, sessionRequest(http.MethodPost, "/graphs/"+graphID.String()+"/access", form))

	if response.Code != http.StatusForbidden {
		t.Fatalf("편집자의 등급 부여 응답 = %d, want %d", response.Code, http.StatusForbidden)
	}
	if graphs.granted {
		t.Fatal("등급 부여가 저장소까지 전달됐다")
	}
}

// TestAccessScreenAllowsTeamManagementWithoutGraph는 그래프가 없는 계정도 팀을 만들 수
// 있는지 확인한다. 팀 동작만 하는 계정에는 URL에 넣을 그래프가 없다.
func TestAccessScreenAllowsTeamManagementWithoutGraph(t *testing.T) {
	accountID := testID(t)
	graphs := &fakeGraphStore{accountID: accountID}
	server := newTestServerWith(t, accountID, graphs)
	form := url.Values{"action": {"create_team"}, "team_name": {"플랫폼"}}
	response := httptest.NewRecorder()

	server.ServeHTTP(response, sessionRequest(http.MethodPost, "/access", form))

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/access" {
		t.Fatalf("팀 생성 응답 = %d, Location %q", response.Code, response.Header().Get("Location"))
	}
	if graphs.teamCreated != "플랫폼" {
		t.Fatalf("만들어진 팀 이름 = %q", graphs.teamCreated)
	}
}

// TestDeletionScreenRestoresAndDeletesNodes는 노드의 삭제와 복구가 복구 화면에 있는지
// 확인한다. 「운영자 복구」가 사용자 직접 삭제의 복구를 소유자의 웹 화면에 맡겼다.
func TestDeletionScreenRestoresAndDeletesNodes(t *testing.T) {
	accountID, graphID := testID(t), testID(t)
	active := model.Context{ID: testID(t), GraphID: graphID, Layer: model.LayerSource, Body: "활성 본문", Source: &model.SourceAttributes{}}
	removed := model.Context{ID: testID(t), GraphID: graphID, Layer: model.LayerSource, Body: "삭제된 본문", Source: &model.SourceAttributes{}}
	graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, grade: model.GraphGradeOwner, active: []model.Context{active}, deleted: []model.Context{removed}}
	server := newTestServerWith(t, accountID, graphs)

	response := httptest.NewRecorder()
	server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graphID.String()+"/deletion", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("삭제 화면 응답 = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	// 삭제된 컨텍스트가 보이는 자리는 이 복구 화면뿐이다.
	if !strings.Contains(body, removed.ID.String()) || !strings.Contains(body, "restore_context") {
		t.Fatal("삭제된 컨텍스트의 복구 수단이 없다")
	}
	if !strings.Contains(body, active.ID.String()) {
		t.Fatal("삭제 대상을 고를 활성 컨텍스트 목록이 없다")
	}

	// 삭제 전 안내는 고른 노드의 연쇄 영향을 보여준다.
	selected := httptest.NewRecorder()
	server.ServeHTTP(selected, sessionRequest(http.MethodGet, "/graphs/"+graphID.String()+"/deletion?context_id="+active.ID.String(), nil))
	if selected.Code != http.StatusOK || !strings.Contains(selected.Body.String(), "delete_context") {
		t.Fatalf("고른 노드의 삭제 안내 = %d", selected.Code)
	}

	form := url.Values{"action": {"delete_context"}, "context_id": {active.ID.String()}}
	deleted := httptest.NewRecorder()
	server.ServeHTTP(deleted, sessionRequest(http.MethodPost, "/graphs/"+graphID.String()+"/deletion", form))
	if deleted.Code != http.StatusSeeOther {
		t.Fatalf("노드 삭제 응답 = %d, want %d", deleted.Code, http.StatusSeeOther)
	}
	if graphs.contextSet == nil || *graphs.contextSet != active.ID || !graphs.contextState {
		t.Fatalf("노드 삭제 호출 = %v, 상태 %v", graphs.contextSet, graphs.contextState)
	}
}

// TestGraphDetailShowsHopTruncationBoundary는 결과 상한이 자른 경계를 화면이 알리는지
// 확인한다. 표시를 빼면 잘린 범위가 그래프 전체처럼 보인다.
func TestGraphDetailShowsHopTruncationBoundary(t *testing.T) {
	accountID, graphID := testID(t), testID(t)
	boundary := 2
	graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, boundary: &boundary}
	server := newTestServerWith(t, accountID, graphs)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graphID.String(), nil))

	if response.Code != http.StatusOK {
		t.Fatalf("그래프 상세 응답 = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "2홉 경계에서 잘렸습니다") {
		t.Fatal("절단 경계 표시가 화면에 없다")
	}
}

// TestGraphListLinksRestoreForOwnDeletedGraphs는 직접 삭제한 그래프의 복구 경로가
// 목록 화면에 있는지 확인한다. 목록 조회에서 빠지므로 다른 진입점이 없다.
func TestGraphListLinksRestoreForOwnDeletedGraphs(t *testing.T) {
	accountID, graphID := testID(t), testID(t)
	deletedAt := time.Now().UTC()
	graphs := &fakeGraphStore{accountID: accountID, graphID: graphID,
		ownedDeleted: []model.Graph{{ID: graphID, Name: "지운 그래프", DeletedAt: &deletedAt}}}
	server := newTestServerWith(t, accountID, graphs)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("그래프 목록 응답 = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "/graphs/"+graphID.String()+"/deletion") {
		t.Fatal("직접 삭제한 그래프의 복구 링크가 없다")
	}
}

// TestLoginReturnsToAuthorizeRequest는 로그인 뒤 원래의 인가 요청으로 돌아가는지
// 확인한다. 「인가 코드 흐름」 5단계가 인가 요청으로 되돌아오는 유일한 통로다.
func TestLoginReturnsToAuthorizeRequest(t *testing.T) {
	server := newTestServer(t, model.ID{1}, model.ID{2})
	next := "/authorize?client_id=test-client&resource=https%3A%2F%2Fservice.test%2Fmcp"
	form := url.Values{"login_id": {"tester"}, "password": {"correct horse battery staple"}, "next": {next}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("로그인 응답 상태 = %d, want %d", recorder.Code, http.StatusSeeOther)
	}
	if location := recorder.Header().Get("Location"); location != next {
		t.Fatalf("로그인 뒤 이동 = %q, want %q", location, next)
	}
}

// TestLoginRejectsExternalReturnTarget은 복귀 주소가 이 서버 밖을 가리키면 버리는지
// 확인한다. 값이 사용자 입력이므로 그대로 쓰면 열린 리다이렉션이 된다.
func TestLoginRejectsExternalReturnTarget(t *testing.T) {
	server := newTestServer(t, model.ID{1}, model.ID{2})
	for _, target := range []string{"https://evil.test/steal", "//evil.test/steal", "javascript:alert(1)"} {
		form := url.Values{"login_id": {"tester"}, "password": {"correct horse battery staple"}, "next": {target}}
		request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if location := recorder.Header().Get("Location"); location != "/graphs" {
			t.Fatalf("%s 복귀 주소 = %q, want /graphs", target, location)
		}
	}
}
