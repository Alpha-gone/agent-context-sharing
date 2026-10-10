package store

import (
	"errors"
	"slices"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestTeamGrantInheritanceAndAuditIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	ownerID, managerID, memberID := newTestID(t), newTestID(t), newTestID(t)
	for _, id := range []model.ID{ownerID, managerID, memberID} {
		createTestAccount(t, database, id)
	}
	graphID := createTestGraph(t, database, ownerID)
	grantAccount(t, database, graphID, ownerID, model.GraphGradeOwner)
	team, err := database.CreateTeam(t.Context(), managerID, "상속 시험 팀")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), team.ID, managerID, memberID); err != nil {
		t.Fatal(err)
	}
	assertAccess := func(want bool) {
		t.Helper()
		grade, found, err := database.EffectiveGrade(t.Context(), graphID, memberID)
		if err != nil || found != want || (want && grade != model.GraphGradeEditor) {
			t.Fatalf("구성원 등급 = %s %t %v, 기대 접근 %t", grade, found, err, want)
		}
		graphs, _, err := database.ListGraphs(t.Context(), memberID, model.GraphListFilter{}, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(graphs, func(graph model.GraphListItem) bool { return graph.ID == graphID })
		if (index >= 0) != want || (index >= 0 && graphs[index].Grade != model.GraphGradeEditor) {
			t.Fatalf("구성원 그래프 목록 = %#v, 기대 접근 %t", graphs, want)
		}
	}
	assertAccess(false)
	// 그래프 소유자와 팀 관리자가 달라도 부여할 수 있다.
	if err := database.GrantGraph(t.Context(), graphID, ownerID, team.ID, GrantSubjectTeam, model.GraphGradeEditor); err != nil {
		t.Fatal(err)
	}
	assertAccess(true)
	grants, err := database.ListGraphGrants(t.Context(), graphID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(grants, func(grant model.GrantSubject) bool {
		return grant.ID == team.ID && grant.Type == "team" && !grant.Inherited && grant.CanRevoke
	}) || !slices.ContainsFunc(grants, func(grant model.GrantSubject) bool {
		return grant.ID == memberID && grant.Inherited && grant.Grade == model.GraphGradeEditor
	}) {
		t.Fatalf("직접 팀 부여와 구성원 상속 표시 = %#v", grants)
	}
	if err := database.RemoveTeamMemberWithAudit(t.Context(), team.ID, managerID, memberID); err != nil {
		t.Fatal(err)
	}
	assertAccess(false)
	if err := database.AddTeamMember(t.Context(), team.ID, managerID, memberID); err != nil {
		t.Fatal(err)
	}
	assertAccess(true)
	if err := database.SetTeamDeleted(t.Context(), team.ID, managerID, true); err != nil {
		t.Fatal(err)
	}
	assertAccess(false)
	if err := database.SetTeamDeleted(t.Context(), team.ID, managerID, false); err != nil {
		t.Fatal(err)
	}
	assertAccess(true)
	if err := database.RevokeGraphGrantWithAudit(t.Context(), graphID, ownerID, team.ID, GrantSubjectTeam); err != nil {
		t.Fatal(err)
	}
	assertAccess(false)
	for _, action := range []string{"grant", "revoke"} {
		var count int
		if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log
			WHERE graph_id = $1 AND actor_account_id = $2 AND subject_type = 'team' AND subject_id = $3 AND action = $4`, graphID.String(), ownerID.String(), team.ID.String(), action).Scan(&count); err != nil || count != 1 {
			t.Fatalf("팀 %s 감사 기록 = %d, 오류 %v", action, count, err)
		}
	}
}

func TestTeamGrantRejectsInactiveTargetsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	ownerID := newTestID(t)
	createTestAccount(t, database, ownerID)
	graphID := createTestGraph(t, database, ownerID)
	grantAccount(t, database, graphID, ownerID, model.GraphGradeOwner)
	deletedID := newTestID(t)
	createTestTeam(t, database, deletedID, ownerID, true)
	// 거부된 대상이 유예와 감사·등급 행을 바꾸지 않는지 함께 확인한다.
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at = now(), grace_expires_at = now() + interval '30 days' WHERE graph_id = $1`, graphID.String()); err != nil {
		t.Fatal(err)
	}
	for _, teamID := range []model.ID{newTestID(t), deletedID} {
		if err := database.GrantGraph(t.Context(), graphID, ownerID, teamID, GrantSubjectTeam, model.GraphGradeViewer); !errors.Is(err, ErrNotFound) {
			t.Fatalf("없는 팀·삭제된 팀 부여 = %v", err)
		}
	}
	var grants, audits int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.graph_grant WHERE graph_id = $1 AND subject_type = 'team'`, graphID.String()).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log WHERE graph_id = $1`, graphID.String()).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	started, expires := graceTimes(t, database, graphID)
	if grants != 0 || audits != 0 || started == nil || expires == nil {
		t.Fatalf("거부 뒤 등급 %d, 감사 %d, 유예 %v %v", grants, audits, started, expires)
	}
}

func TestManagedTeamMembersAndGrantChoicesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	managerID, otherID, firstID, secondID := newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	for _, id := range []model.ID{managerID, otherID, firstID, secondID} {
		createTestAccount(t, database, id)
	}
	activeID, emptyID, deletedID, foreignID := newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	for _, id := range []model.ID{activeID, emptyID, deletedID} {
		createTestTeam(t, database, id, managerID, id == deletedID)
	}
	createTestTeam(t, database, foreignID, otherID, false)
	addTestTeamMember(t, database, activeID, secondID)
	addTestTeamMember(t, database, activeID, firstID)
	addTestTeamMember(t, database, deletedID, firstID)
	addTestTeamMember(t, database, foreignID, otherID)
	teams, err := database.ListManagedTeams(t.Context(), managerID)
	if err != nil || len(teams) != 3 {
		t.Fatalf("관리 팀 목록 = %#v, 오류 %v", teams, err)
	}
	for _, team := range teams {
		wantCount := 0
		if team.ID == activeID {
			wantCount = 2
		} else if team.ID == deletedID {
			wantCount = 1
		}
		if team.ManagerAccountID != managerID || len(team.MemberLoginIDs) != wantCount || !slices.IsSorted(team.MemberLoginIDs) {
			t.Fatalf("관리자 범위·구성원 정렬 = %#v", team)
		}
	}
	choices, err := database.ListGrantableTeams(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, team := range choices {
		if team.DeletedAt != nil || len(team.MemberLoginIDs) != 0 {
			t.Fatalf("부여 목록에 삭제된 팀 또는 구성원 정보가 포함됐다: %#v", team)
		}
	}
	for _, id := range []model.ID{activeID, emptyID, foreignID} {
		if !slices.ContainsFunc(choices, func(team model.Team) bool { return team.ID == id }) {
			t.Fatalf("활성 팀 선택 누락: %s", id)
		}
	}
}
