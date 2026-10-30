package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

type web04Client struct {
	t      *testing.T
	base   string
	client *http.Client
	token  string
}

func web04Server(t *testing.T, database *store.Store, graphs web.GraphStore, actor model.ID) web04Client {
	t.Helper()
	service, err := authz.New(database, authz.Config{Issuer: "https://permission.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(t.Context(), actor, web.SessionAudience)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := web.New(permissionAuthentication{service}, graphs, web.Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return web04Client{t: t, base: server.URL, client: client, token: token.Raw}
}

func (client web04Client) request(path string, form url.Values, status int) string {
	client.t.Helper()
	method := http.MethodGet
	if form != nil {
		method = http.MethodPost
	}
	request, err := http.NewRequestWithContext(client.t.Context(), method, client.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		client.t.Fatal(err)
	}
	request.Header.Set("Origin", client.base)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "agent_context_session", Value: client.token})
	response, err := client.client.Do(request)
	if err != nil {
		client.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		client.t.Fatal(err)
	}
	if response.StatusCode != status {
		client.t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, status, body)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		client.t.Fatal("보호 화면의 캐시 경계")
	}
	if status == http.StatusSeeOther && response.Header.Get("Location") != path {
		client.t.Fatalf("이동 대상 = %q", response.Header.Get("Location"))
	}
	return string(body)
}

func TestWEB04OwnerRevocationIntegration(t *testing.T) {
	database := permissionDatabase(t)
	actor, other := permissionAccount(t, database), permissionAccount(t, database)
	client := web04Server(t, database, database, actor)
	for _, sample := range []struct {
		name                                                                    string
		teamTarget, emptyAlternative, deletedAlternative, duplicate, additional bool
		allowed                                                                 bool
	}{
		{name: "direct_last_owner"},
		{name: "sole_team_multiple_members", teamTarget: true},
		{name: "empty_team_is_not_owner", emptyAlternative: true},
		{name: "deleted_team_is_not_owner", deletedAlternative: true},
		{name: "same_account_direct_and_inherited", duplicate: true, allowed: true},
		{name: "other_direct_owner", additional: true, allowed: true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := permissionGraph(t, database, actor)
			grant := func(subject model.ID, kind store.GrantSubjectType, grade model.GraphGrade) {
				t.Helper()
				if err := database.GrantGraph(t.Context(), graph, actor, subject, kind, grade); err != nil {
					t.Fatal(err)
				}
			}
			subject, kind := actor, store.GrantSubjectAccount
			if sample.teamTarget || sample.emptyAlternative || sample.deletedAlternative || sample.duplicate {
				team, err := database.CreateTeam(t.Context(), other, "owner team")
				if err != nil {
					t.Fatal(err)
				}
				if !sample.emptyAlternative {
					for _, member := range []model.ID{actor, other} {
						if err := database.AddTeamMember(t.Context(), team.ID, other, member); err != nil {
							t.Fatal(err)
						}
					}
				}
				grant(team.ID, store.GrantSubjectTeam, model.GraphGradeOwner)
				if sample.deletedAlternative {
					if err := database.SetTeamDeleted(t.Context(), team.ID, other, true); err != nil {
						t.Fatal(err)
					}
				}
				if sample.teamTarget {
					if err := database.RevokeGraphGrantWithAudit(t.Context(), graph, actor, actor, store.GrantSubjectAccount); err != nil {
						t.Fatal(err)
					}
					subject, kind = team.ID, store.GrantSubjectTeam
				}
			}
			if sample.additional {
				grant(other, store.GrantSubjectAccount, model.GraphGradeOwner)
			}
			rows, err := database.ListGraphGrants(t.Context(), graph, actor)
			if err != nil {
				t.Fatal(err)
			}
			path := "/graphs/" + graph.String() + "/access"
			body := client.request(path, nil, http.StatusOK)
			found := false
			for _, row := range rows {
				var rendered string
				for _, fragment := range strings.Split(body, "<tr>") {
					if strings.Contains(fragment, "<code>"+row.ID.String()+"</code>") && strings.Contains(fragment, ">"+row.Type+"</td>") && strings.Contains(fragment, "팀 상속</td>") == row.Inherited {
						rendered = strings.Split(fragment, "</tr>")[0]
						break
					}
				}
				if rendered == "" {
					t.Fatalf("등급 행 누락: %+v", row)
				}
				if row.Inherited {
					if strings.Contains(rendered, "<form") {
						t.Fatal("상속 행에 직접 회수 양식")
					}
					continue
				}
				if row.ID != subject || row.Type != string(kind) {
					continue
				}
				found = true
				if row.CanRevoke != sample.allowed || strings.Contains(rendered, `value="revoke"`) != sample.allowed {
					t.Fatalf("회수 안내 불일치: %+v", row)
				}
				if !sample.allowed && (!strings.Contains(rendered, "disabled aria-describedby=") || !strings.Contains(rendered, "소유자 등급을 쓸 계정이 남지 않습니다.")) {
					t.Fatal("본문의 회수 불가 이유 누락")
				}
			}
			if !found {
				t.Fatal("회수 대상 행 누락")
			}
			status := http.StatusConflict
			if sample.allowed {
				status = http.StatusSeeOther
			}
			response := client.request(path, url.Values{"action": {"revoke"}, "subject_id": {subject.String()}, "subject_type": {string(kind)}}, status)
			if !sample.allowed && !strings.Contains(response, "마지막 소유자 등급은 회수할 수 없습니다.") {
				t.Fatal("마지막 소유자 고정 안내 누락")
			}
			if !sample.allowed {
				form := url.Values{"action": {"grant_account"}, "login_id": {permissionLogin(actor)}, "grade": {"editor"}}
				if sample.teamTarget {
					form = url.Values{"action": {"grant_team"}, "team_id": {subject.String()}, "grade": {"editor"}}
				}
				client.request(path, form, http.StatusConflict)
			}
		})
	}
}

func TestWEB04ManagedTeamsIntegration(t *testing.T) {
	database := permissionDatabase(t)
	manager, owner, first, second, secret := permissionAccount(t, database), permissionAccount(t, database), permissionAccount(t, database), permissionAccount(t, database), permissionAccount(t, database)
	client := web04Server(t, database, database, manager)
	ownerClient := web04Server(t, database, database, owner)
	client.request("/access", url.Values{"action": {"create_team"}, "team_name": {"managed <team>"}}, http.StatusSeeOther)
	teams, err := database.ListManagedTeams(t.Context(), manager)
	if err != nil || len(teams) != 1 || len(teams[0].MemberLoginIDs) != 0 {
		t.Fatalf("팀 생성 = %+v, %v", teams, err)
	}
	team := teams[0]
	emptyBody := client.request("/access", nil, http.StatusOK)
	if !strings.Contains(emptyBody, "구성원이 없습니다.") || !strings.Contains(emptyBody, "부여된 그래프 등급이 없습니다.") {
		t.Fatal("빈 팀의 구성원·등급 안내 누락")
	}
	foreign, err := database.CreateTeam(t.Context(), owner, "managed <team>")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), foreign.ID, owner, secret); err != nil {
		t.Fatal(err)
	}
	for _, id := range []model.ID{second, first} {
		client.request("/access", url.Values{"action": {"add_member"}, "team_id": {team.ID.String()}, "login_id": {permissionLogin(id)}}, http.StatusSeeOther)
	}
	graph, another := permissionGraph(t, database, owner), permissionGraph(t, database, owner)
	for _, id := range []model.ID{graph, another} {
		path := "/graphs/" + id.String() + "/access"
		body := ownerClient.request(path, nil, http.StatusOK)
		for _, choice := range []model.ID{team.ID, foreign.ID} {
			if !strings.Contains(body, `value="`+choice.String()+`">managed &lt;team&gt; · `+choice.String()) {
				t.Fatal("동명 활성 팀을 식별자로 구분할 수 없다")
			}
		}
		ownerClient.request(path, url.Values{"action": {"grant_team"}, "team_id": {team.ID.String()}, "grade": {"editor"}}, http.StatusSeeOther)
	}
	for i, deleted := range []bool{false, true, false} {
		if i > 0 {
			if err := database.SetTeamDeleted(t.Context(), team.ID, manager, deleted); err != nil {
				t.Fatal(err)
			}
		}
		body := client.request("/access", nil, http.StatusOK)
		members := []string{permissionLogin(first), permissionLogin(second)}
		slices.Sort(members)
		if strings.Index(body, "<li>"+members[0]+"</li>") >= strings.Index(body, "<li>"+members[1]+"</li>") || !strings.Contains(body, "<li>"+members[0]+"</li>") {
			t.Fatal("구성원 정렬/표시")
		}
		for _, id := range []model.ID{graph, another} {
			if !strings.Contains(body, "<code>"+id.String()+"</code> · editor") {
				t.Fatal("팀 그래프 등급 누락")
			}
		}
		if strings.Contains(body, permissionLogin(secret)) || strings.Contains(body, foreign.ID.String()) || strings.Contains(body, "permission acceptance") || strings.Contains(body, `value="grant_team"`) || strings.Contains(body, "managed <team>") {
			t.Fatal("관리자 범위 또는 이스케이프 실패")
		}
		if strings.Contains(body, "삭제된 팀의 상속 등급은 무효입니다.") != deleted {
			t.Fatal("삭제 팀 상속 안내")
		}
		if _, found, err := database.EffectiveGrade(t.Context(), graph, first); err != nil || found == deleted {
			t.Fatalf("상속 유효성 = %t, %v", found, err)
		}
	}
	client.request("/graphs/"+graph.String(), nil, http.StatusNotFound)
	client.request("/access", url.Values{"action": {"remove_member"}, "team_id": {foreign.ID.String()}, "login_id": {permissionLogin(secret)}}, http.StatusNotFound)
	client.request("/access", url.Values{"action": {"delete_team"}, "team_id": {foreign.ID.String()}}, http.StatusNotFound)
	for _, action := range []string{"remove_member", "delete_team", "restore_team"} {
		client.request("/access", url.Values{"action": {action}, "team_id": {team.ID.String()}, "login_id": {permissionLogin(first)}}, http.StatusSeeOther)
	}
	teams, err = database.ListManagedTeams(t.Context(), manager)
	if err != nil || len(teams) != 1 || len(teams[0].GraphGrants) != 2 || len(teams[0].MemberLoginIDs) != 1 {
		t.Fatalf("관리 팀 묶음 = %+v, %v", teams, err)
	}
}

type web04ChangedBeforeGrant struct {
	web.GraphStore
	before func(context.Context) error
}

func (graphs web04ChangedBeforeGrant) GrantGraph(ctx context.Context, graph, actor, subject model.ID, kind store.GrantSubjectType, grade model.GraphGrade) error {
	if err := graphs.before(ctx); err != nil {
		return fmt.Errorf("변경 준비: %w", err)
	}
	return graphs.GraphStore.GrantGraph(ctx, graph, actor, subject, kind, grade)
}

func TestWEB04StaleOwnerAndTeamIntegration(t *testing.T) {
	for _, change := range []string{"owner_lost", "team_deleted"} {
		t.Run(change, func(t *testing.T) {
			database := permissionDatabase(t)
			actor, other, member := permissionAccount(t, database), permissionAccount(t, database), permissionAccount(t, database)
			graph := permissionGraph(t, database, actor)
			if err := database.GrantGraph(t.Context(), graph, actor, other, store.GrantSubjectAccount, model.GraphGradeOwner); err != nil {
				t.Fatal(err)
			}
			team, err := database.CreateTeam(t.Context(), other, "target")
			if err != nil {
				t.Fatal(err)
			}
			if err := database.AddTeamMember(t.Context(), team.ID, other, member); err != nil {
				t.Fatal(err)
			}
			graphs := web04ChangedBeforeGrant{GraphStore: database, before: func(ctx context.Context) error {
				if change == "owner_lost" {
					return database.RevokeGraphGrantWithAudit(ctx, graph, other, actor, store.GrantSubjectAccount)
				}
				return database.SetTeamDeleted(ctx, team.ID, other, true)
			}}
			client := web04Server(t, database, graphs, actor)
			path := "/graphs/" + graph.String() + "/access"
			client.request(path, nil, http.StatusOK)
			body := client.request(path, url.Values{"action": {"grant_team"}, "team_id": {team.ID.String()}, "grade": {"editor"}}, http.StatusNotFound)
			if !strings.Contains(body, "등급을 바꿀 대상을 찾을 수 없습니다.") {
				t.Fatal("고정된 대상/접근 상실 안내 누락")
			}
			if _, found, err := database.EffectiveGrade(t.Context(), graph, member); err != nil || found {
				t.Fatalf("거부한 부여가 반영됐다: %v", err)
			}
		})
	}
}

func TestTeamInheritanceMemberVisibilityIntegration(t *testing.T) {
	database := permissionDatabase(t)
	owner, manager := permissionAccount(t, database), permissionAccount(t, database)
	localMember, foreignMember := permissionAccount(t, database), permissionAccount(t, database)
	graph := permissionGraph(t, database, owner)
	local, err := database.CreateTeam(t.Context(), owner, "local visibility team")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := database.CreateTeam(t.Context(), manager, "foreign visibility team")
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct{ team, manager, member model.ID }{
		{local.ID, owner, localMember}, {foreign.ID, manager, foreignMember},
	} {
		if err := database.AddTeamMember(t.Context(), sample.team, sample.manager, sample.member); err != nil {
			t.Fatal(err)
		}
		if err := database.GrantGraph(t.Context(), graph, owner, sample.team, store.GrantSubjectTeam, model.GraphGradeViewer); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.GrantGraph(t.Context(), graph, owner, manager, store.GrantSubjectAccount, model.GraphGradeOwner); err != nil {
		t.Fatal(err)
	}
	ownerClient := web04Server(t, database, database, owner)
	managerClient := web04Server(t, database, database, manager)
	path := "/graphs/" + graph.String() + "/access"
	for i, deleted := range []bool{false, true, false} {
		if i > 0 {
			if err := database.SetTeamDeleted(t.Context(), foreign.ID, manager, deleted); err != nil {
				t.Fatal(err)
			}
		}
		for _, sample := range []struct {
			actor           model.ID
			client          web04Client
			visible, hidden model.ID
			show            bool
		}{
			{owner, ownerClient, localMember, foreignMember, true},
			{manager, managerClient, foreignMember, localMember, !deleted},
		} {
			rows, err := database.ListGraphGrants(t.Context(), graph, sample.actor)
			if err != nil {
				t.Fatal(err)
			}
			body := sample.client.request(path, nil, http.StatusOK)
			for _, forbidden := range []string{sample.hidden.String(), permissionLogin(sample.hidden)} {
				if strings.Contains(body, forbidden) {
					t.Fatal("다른 관리자 팀의 구성원 정보가 노출됐다")
				}
			}
			if slices.ContainsFunc(rows, func(row model.GrantSubject) bool { return row.ID == sample.hidden }) {
				t.Fatal("저장소의 관리자 조회 경계 위반")
			}
			_, table, foundTable := strings.Cut(body, "<tbody>")
			table, _, closedTable := strings.Cut(table, "</tbody>")
			if !foundTable || !closedTable {
				t.Fatal("등급 표 누락")
			}
			for _, visible := range []string{sample.visible.String(), permissionLogin(sample.visible)} {
				if strings.Contains(table, visible) != sample.show {
					t.Fatal("관리 팀 구성원의 표시 범위 불일치")
				}
			}
			if !strings.Contains(body, foreign.ID.String()) || !strings.Contains(body, "foreign visibility team") || !strings.Contains(body, "구성원별 상속 행은 관리하는 활성 팀에 한해 표시합니다.") {
				t.Fatal("팀 요약·상속 안내 누락")
			}
		}
		grade, found, err := database.EffectiveGrade(t.Context(), graph, foreignMember)
		if err != nil || found == deleted || found && grade != model.GraphGradeViewer {
			t.Fatal("표시 범위 변경이 실제 상속을 바꿨다", grade, found, err)
		}
	}
	if err := database.GrantGraph(t.Context(), graph, owner, foreignMember, store.GrantSubjectAccount, model.GraphGradeEditor); err != nil {
		t.Fatal(err)
	}
	body := ownerClient.request(path, nil, http.StatusOK)
	if !strings.Contains(body, foreignMember.String()) || !strings.Contains(body, permissionLogin(foreignMember)) {
		t.Fatal("명시적 계정 직접 부여를 숨겼다")
	}
}
