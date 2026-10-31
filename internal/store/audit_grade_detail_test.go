package store

import (
	"testing"

	"agent_context_sharing/internal/model"
)

func TestAuditGradeDetailsIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, owner, _ := boundaryGraph(t, database)
	editor, successor := newTestID(t), newTestID(t)
	createTestAccount(t, database, editor)
	createTestAccount(t, database, successor)
	if err := database.GrantGraph(t.Context(), graph, owner, editor, GrantSubjectAccount, model.GraphGradeEditor); err != nil {
		t.Fatal(err)
	}
	if err := database.GrantGraph(t.Context(), graph, owner, editor, GrantSubjectAccount, model.GraphGradeViewer); err != nil {
		t.Fatal(err)
	}
	if err := database.GrantGraph(t.Context(), graph, owner, successor, GrantSubjectAccount, model.GraphGradeOwner); err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeGraphGrantWithAudit(t.Context(), graph, owner, owner, GrantSubjectAccount); err != nil {
		t.Fatal(err)
	}
	team, err := database.CreateTeam(t.Context(), successor, "audit inherited owners")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), team.ID, successor, editor); err != nil {
		t.Fatal(err)
	}
	if err := database.GrantGraph(t.Context(), graph, successor, team.ID, GrantSubjectTeam, model.GraphGradeOwner); err != nil {
		t.Fatal(err)
	}
	if err := database.SetGraphDeleted(t.Context(), graph, successor, true); err != nil {
		t.Fatal(err)
	}
	if err := database.SetGraphDeleted(t.Context(), graph, successor, false); err != nil {
		t.Fatal(err)
	}
	entries, next, err := database.ListAuditEntries(t.Context(), graph, successor, model.AuditFilter{}, "", 50)
	if err != nil || next != "" {
		t.Fatalf("audit list: %s/%v", next, err)
	}
	for _, want := range []struct{ kind, action, target, detail string }{
		{"grant", "grant", editor.String(), "- → editor"},
		{"grant", "grant", editor.String(), "editor → viewer"},
		{"ownership_transfer", "transfer", successor.String(), "- → owner"},
		{"grant", "revoke", owner.String(), "owner → -"},
		{"grant", "grant", team.ID.String(), "- → owner / 구성원: " + editor.String()},
		{"web_delete", "delete", graph.String(), ""},
		{"web_delete", "restore", graph.String(), ""},
	} {
		found := false
		for _, entry := range entries {
			if entry.Kind == want.kind && entry.Action == want.action && entry.Target == want.target && entry.Detail == want.detail {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing audit %+v; got %+v", want, entries)
		}
	}
	// 구성원 필터도 팀 대상과 해당 구성원의 상속 상세를 함께 유지해야 한다.
	members, _, err := database.ListAuditEntries(t.Context(), graph, successor, model.AuditFilter{Target: editor}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	inherited := false
	for _, entry := range members {
		if entry.Target == team.ID.String() && entry.Detail == "- → owner / 구성원: "+editor.String() {
			inherited = true
		}
	}
	if !inherited {
		t.Fatal("member filter lost inherited owner details")
	}
}

func TestAuditOperatorRestoreGradeDetailIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, requester, operator := operatorRestoreFixture(t, database, false)
	if err := database.OperatorRestoreGraph(t.Context(), graph, operator); err != nil {
		t.Fatal(err)
	}
	entries, next, err := database.ListAuditEntries(t.Context(), graph, requester, model.AuditFilter{}, "", 50)
	if err != nil || next != "" {
		t.Fatalf("restore audit list: %s/%v", next, err)
	}
	for _, entry := range entries {
		if entry.Kind == "operator_restore" && entry.Action == "restore" {
			if entry.ActorID != operator || entry.Target != requester.String() || entry.Detail != "- → owner" {
				t.Fatalf("restore audit = %+v", entry)
			}
			return
		}
	}
	t.Fatal("operator restore audit missing")
}
