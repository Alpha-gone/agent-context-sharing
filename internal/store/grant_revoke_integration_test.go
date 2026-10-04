package store

import (
	"errors"
	"testing"

	"agent_context_sharing/internal/model"
)

// TestGrantRevokeAvailabilityMatchesMutationIntegration은 행 수나 계정 수의 근사 대신
// 해당 부여 전체를 제외한 실제 회수 결과와 화면 안내를 대조한다.
func TestGrantRevokeAvailabilityMatchesMutationIntegration(t *testing.T) {
	for _, test := range []struct {
		name                                string
		directOwner, teamOwner, teamDeleted bool
		members                             int
		targetTeam, want                    bool
	}{
		{name: "구성원 없는 팀은 마지막 계정을 대신하지 않는다", directOwner: true, teamOwner: true},
		{name: "삭제된 팀은 마지막 계정을 대신하지 않는다", directOwner: true, teamOwner: true, teamDeleted: true, members: 2},
		{name: "같은 계정의 팀 부여도 직접 부여 회수 뒤 남는다", directOwner: true, teamOwner: true, members: 1, want: true},
		{name: "활성 팀 소유자가 직접 부여 회수 뒤 남는다", directOwner: true, teamOwner: true, members: 2, want: true},
		{name: "여러 구성원의 유일한 팀 부여도 회수할 수 없다", teamOwner: true, members: 2, targetTeam: true},
		{name: "직접 소유자가 있으면 팀 부여를 회수할 수 있다", directOwner: true, teamOwner: true, members: 2, targetTeam: true, want: true},
		{name: "빈 팀 부여 자체는 직접 소유자가 있으면 회수할 수 있다", directOwner: true, teamOwner: true, targetTeam: true, want: true},
		{name: "소유자 없는 그래프의 열람자 회수도 거부한다"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := newIntegrationStore(t)
			actorID, memberID := newTestID(t), newTestID(t)
			createTestAccount(t, database, actorID)
			createTestAccount(t, database, memberID)
			graphID := createTestGraph(t, database, actorID)
			grade := model.GraphGradeViewer
			if test.directOwner {
				grade = model.GraphGradeOwner
			}
			// 소유자 없는 상태는 팀 구성원 소멸 뒤 생길 수 있는 기존 상태다.
			grantAccount(t, database, graphID, actorID, grade)
			teamID := newTestID(t)
			if test.teamOwner {
				createTestTeam(t, database, teamID, actorID, test.teamDeleted)
				grantTeam(t, database, graphID, teamID, model.GraphGradeOwner)
				if test.members >= 1 {
					addTestTeamMember(t, database, teamID, actorID)
				}
				if test.members >= 2 {
					addTestTeamMember(t, database, teamID, memberID)
				}
			}
			targetID, targetType := actorID, GrantSubjectAccount
			if test.targetTeam {
				targetID, targetType = teamID, GrantSubjectTeam
			}
			grants, err := database.ListGraphGrants(t.Context(), graphID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, grant := range grants {
				if grant.Inherited && grant.CanRevoke {
					t.Fatalf("상속 등급에 회수 수단: %#v", grant)
				}
				if !grant.Inherited && grant.ID == targetID && grant.Type == string(targetType) {
					found = true
					if grant.CanRevoke != test.want {
						t.Fatalf("회수 안내=%t, 기대=%t", grant.CanRevoke, test.want)
					}
				}
			}
			if !found {
				t.Fatal("회수 대상 부여가 없다")
			}
			err = database.RevokeGraphGrantWithAudit(t.Context(), graphID, actorID, targetID, targetType)
			if test.want && err != nil {
				t.Fatalf("허용한 회수 실패: %v", err)
			}
			if !test.want && !errors.Is(err, ErrLastOwner) {
				t.Fatalf("차단한 회수=%v, 기대 ErrLastOwner", err)
			}
		})
	}
}
