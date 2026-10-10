package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func TestTeamGrantRequestBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, teamID, grade, origin string
		owner                       bool
		grantErr                    error
		status                      int
		forwarded                   bool
	}{
		{name: "소유자 부여", owner: true, grade: "editor", status: http.StatusSeeOther, forwarded: true},
		{name: "비소유자", grade: "editor", status: http.StatusForbidden},
		{name: "다른 출처", owner: true, grade: "editor", origin: "https://other.test", status: http.StatusForbidden},
		{name: "잘못된 식별자", owner: true, teamID: "invalid", grade: "editor", status: http.StatusBadRequest},
		{name: "잘못된 등급", owner: true, grade: "invalid", status: http.StatusBadRequest},
		{name: "없는 팀 또는 삭제된 팀", owner: true, grade: "editor", grantErr: fmt.Errorf("조회: %w", store.ErrNotFound), status: http.StatusNotFound, forwarded: true},
		{name: "마지막 소유자", owner: true, grade: "editor", grantErr: store.ErrLastOwner, status: http.StatusConflict, forwarded: true},
		{name: "내부 장애", owner: true, grade: "editor", grantErr: errors.New("secret database detail"), status: http.StatusInternalServerError, forwarded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			accountID, graphID, teamID := testID(t), testID(t), testID(t)
			graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, grade: model.GraphGradeViewer, grantErr: test.grantErr}
			if test.owner {
				graphs.grade = model.GraphGradeOwner
			}
			rawID := teamID.String()
			if test.teamID != "" {
				rawID = test.teamID
			}
			target := "https://service.test/graphs/" + graphID.String() + "/access"
			request := sessionRequest(http.MethodPost, target, url.Values{"action": {"grant_team"}, "team_id": {rawID}, "grade": {test.grade}})
			request.Header.Set("Origin", "https://service.test")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			newTestServerWith(t, accountID, graphs).ServeHTTP(response, request)
			if response.Code != test.status || graphs.granted != test.forwarded {
				t.Fatalf("응답 = %d, 전달 = %t, 기대 %d %t", response.Code, graphs.granted, test.status, test.forwarded)
			}
			if graphs.granted && (graphs.grantSubjectID != teamID || graphs.grantSubjectType != store.GrantSubjectTeam || graphs.grantGrade != model.GraphGradeEditor) {
				t.Fatalf("팀 부여 입력 = %s %s %s", graphs.grantSubjectID, graphs.grantSubjectType, graphs.grantGrade)
			}
			if strings.Contains(response.Body.String(), "secret database") {
				t.Fatal("내부 오류가 응답에 노출됐다")
			}
		})
	}
}

func TestAccessShowsTeamChoicesAndManagedMembers(t *testing.T) {
	accountID, graphID, teamID := testID(t), testID(t), testID(t)
	graphs := &fakeGraphStore{
		accountID: accountID, graphID: graphID, grade: model.GraphGradeOwner,
		grantableTeams: []model.Team{{ID: teamID, Name: "공유 팀"}},
		managedTeams: []model.Team{
			{ID: teamID, Name: "공유 팀", MemberLoginIDs: []string{"alice", "bob"}},
			{ID: testID(t), Name: "빈 팀"},
			{ID: testID(t), Name: "삭제된 팀", DeletedAt: new(time.Now()), MemberLoginIDs: []string{"<script>unsafe()</script>"}},
		},
		grants: []model.GrantSubject{{ID: teamID, Name: "공유 팀", Type: "team", Grade: model.GraphGradeEditor, CanRevoke: true}},
	}
	server := newTestServerWith(t, accountID, graphs)
	for _, target := range []string{"/graphs/" + graphID.String() + "/access", "/access"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, sessionRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("화면 응답 = %d", response.Code)
		}
		body := response.Body.String()
		for _, expected := range []string{"현재 구성원", "<li>alice</li>", "<li>bob</li>", "구성원이 없습니다.", "(삭제됨)", "&lt;script&gt;"} {
			if !strings.Contains(body, expected) {
				t.Errorf("구성원 표시 누락: %s", expected)
			}
		}
		if strings.Contains(body, "<script>unsafe()") {
			t.Fatal("구성원 입력을 HTML로 삽입했다")
		}
		if target != "/access" {
			for _, expected := range []string{`value="grant_team"`, `name="team_id"`, `공유 팀 · ` + teamID.String(), `value="revoke"`, `name="subject_type" value="team"`} {
				if !strings.Contains(body, expected) {
					t.Errorf("팀 부여·회수 양식 누락: %s", expected)
				}
			}
		} else if strings.Contains(body, `value="grant_team"`) {
			t.Fatal("그래프 없는 진입에 부여 양식이 보인다")
		}
	}
}

func TestGrantableTeamsAreQueriedOnlyForOwners(t *testing.T) {
	for _, grade := range []model.GraphGrade{model.GraphGradeViewer, model.GraphGradeOwner} {
		t.Run(string(grade), func(t *testing.T) {
			accountID, graphID := testID(t), testID(t)
			graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, grade: grade, grantableTeamsErr: errors.New("secret database detail")}
			response := httptest.NewRecorder()
			newTestServerWith(t, accountID, graphs).ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graphID.String()+"/access", nil))
			wantOwner := grade == model.GraphGradeOwner
			if graphs.grantableQueried != wantOwner || (wantOwner && response.Code != http.StatusInternalServerError) || (!wantOwner && response.Code != http.StatusOK) {
				t.Fatalf("활성 팀 조회 = %t, 응답 = %d", graphs.grantableQueried, response.Code)
			}
			if strings.Contains(response.Body.String(), "secret database") {
				t.Fatal("목록 조회 오류가 응답에 노출됐다")
			}
		})
	}
}
