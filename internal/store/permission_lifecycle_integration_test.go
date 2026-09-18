package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestRevokeGraphGrantKeepsOneOwnerIntegration은 실제 행 잠금으로 동시에 두 소유자
// 등급을 회수해도 한 등급은 남는지 확인한다.
func TestRevokeGraphGrantKeepsOneOwnerIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	firstAccountID, secondAccountID := newTestID(t), newTestID(t)
	createTestAccount(t, database, firstAccountID)
	createTestAccount(t, database, secondAccountID)
	graphID := createTestGraph(t, database, firstAccountID)
	grantAccount(t, database, graphID, firstAccountID, model.GraphGradeOwner)
	grantAccount(t, database, graphID, secondAccountID, model.GraphGradeOwner)

	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for _, accountID := range []model.ID{firstAccountID, secondAccountID} {
		waitGroup.Go(func() {
			results <- database.RevokeGraphGrant(t.Context(), graphID, GrantSubjectAccount, accountID)
		})
	}
	waitGroup.Wait()
	close(results)

	succeeded, retained := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrLastOwner):
			retained++
		default:
			t.Fatalf("동시 소유자 등급 회수: %v", err)
		}
	}
	if succeeded != 1 || retained != 1 {
		t.Fatalf("동시 소유자 등급 회수 결과 = 성공 %d, 보존 %d; want 각각 1", succeeded, retained)
	}
	var owners int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.graph_grant WHERE graph_id = $1 AND grade = 'owner'`, graphID.String()).Scan(&owners); err != nil {
		t.Fatalf("남은 소유자 수 조회: %v", err)
	}
	if owners != 1 {
		t.Fatalf("동시 회수 뒤 소유자 수 = %d, want 1", owners)
	}
}

// TestRemoveTeamMemberStartsGraceOnceIntegration은 두 팀이 같은 두 그래프에 접근을
// 제공할 때 모든 그래프 행을 같은 순서로 잠그고 마지막 제거에서만 유예를 시작하며,
// 이후 수명주기 판정이 기존 유예 시각을 덮어쓰지 않는지 확인한다.
func TestRemoveTeamMemberStartsGraceOnceIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	firstTeamID, secondTeamID := newTestID(t), newTestID(t)
	createTestTeam(t, database, firstTeamID, accountID, false)
	createTestTeam(t, database, secondTeamID, accountID, false)
	addTestTeamMember(t, database, firstTeamID, accountID)
	addTestTeamMember(t, database, secondTeamID, accountID)
	firstGraphID, secondGraphID := createTestGraph(t, database, accountID), createTestGraph(t, database, accountID)
	for _, graphID := range []model.ID{firstGraphID, secondGraphID} {
		grantTeam(t, database, graphID, firstTeamID, model.GraphGradeOwner)
		grantTeam(t, database, graphID, secondTeamID, model.GraphGradeOwner)
	}

	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for _, teamID := range []model.ID{firstTeamID, secondTeamID} {
		waitGroup.Go(func() {
			results <- database.RemoveTeamMember(t.Context(), teamID, accountID)
		})
	}
	waitGroup.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("동시 팀 구성원 제거: %v", err)
		}
	}
	graceStartedAt := make(map[model.ID]time.Time, 2)
	for _, graphID := range []model.ID{firstGraphID, secondGraphID} {
		graph, err := database.Graph(t.Context(), graphID)
		if err != nil {
			t.Fatalf("유예 대상 그래프 조회: %v", err)
		}
		if graph.GraceStartedAt == nil {
			t.Fatalf("마지막 접근 가능 계정이 사라진 그래프의 유예가 시작되지 않았다: %s", graphID)
		}
		graceStartedAt[graphID] = *graph.GraceStartedAt
	}

	// 첫 팀은 이미 구성원이 없지만 등급은 남아 있다. 삭제는 수명주기 판정을 다시
	// 실행하므로, grace_started_at IS NULL 보호가 없으면 기존 유예 시각을 덮어쓴다.
	time.Sleep(10 * time.Millisecond)
	if err := database.DeleteTeam(t.Context(), firstTeamID); err != nil {
		t.Fatalf("유예 뒤 팀 삭제: %v", err)
	}
	for _, graphID := range []model.ID{firstGraphID, secondGraphID} {
		graph, err := database.Graph(t.Context(), graphID)
		if err != nil {
			t.Fatalf("유예 보존 그래프 조회: %v", err)
		}
		if graph.GraceStartedAt == nil || !graph.GraceStartedAt.Equal(graceStartedAt[graphID]) {
			t.Fatalf("기존 유예 시각이 바뀌었다: graph_id=%s, got=%v, want=%s", graphID, graph.GraceStartedAt, graceStartedAt[graphID])
		}
	}
}

// TestDeleteTeamStartsGraceIntegration은 팀 소프트 삭제도 접근 가능한 계정을 줄이는
// 경로로 취급해 유예 시작을 같은 트랜잭션에 기록하는지 확인한다.
func TestDeleteTeamStartsGraceIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	teamID := newTestID(t)
	createTestTeam(t, database, teamID, accountID, false)
	addTestTeamMember(t, database, teamID, accountID)
	graphID := createTestGraph(t, database, accountID)
	grantTeam(t, database, graphID, teamID, model.GraphGradeOwner)

	if err := database.DeleteTeam(t.Context(), teamID); err != nil {
		t.Fatalf("팀 삭제: %v", err)
	}
	graph, err := database.Graph(t.Context(), graphID)
	if err != nil {
		t.Fatalf("팀 삭제 뒤 그래프 조회: %v", err)
	}
	if graph.GraceStartedAt == nil {
		t.Fatal("팀 삭제 뒤 마지막 접근 가능 계정 유예가 시작되지 않았다")
	}
}

// TestGrantGraphKeepsOneOwnerIntegration은 유일한 소유자가 자기 등급을 낮추는 부여도
// 마지막 소유자 판정을 지나는지 확인한다. upsert가 기존 등급을 덮어쓰므로 부여 경로도
// 소유자를 없앨 수 있으며, FR-AGENT_CONTEXT-042는 소유자 계정을 최소 하나 요구한다.
func TestGrantGraphKeepsOneOwnerIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)
	grantAccount(t, database, graphID, accountID, model.GraphGradeOwner)

	err := database.GrantGraph(t.Context(), graphID, accountID, accountID, GrantSubjectAccount, model.GraphGradeEditor)
	if !errors.Is(err, ErrLastOwner) {
		t.Fatalf("마지막 소유자 강등 = %v, want ErrLastOwner", err)
	}
	var owners int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.graph_grant WHERE graph_id = $1 AND grade = 'owner'`, graphID.String()).Scan(&owners); err != nil {
		t.Fatalf("소유자 수 조회: %v", err)
	}
	if owners != 1 {
		t.Fatalf("거부 뒤 소유자 수 = %d, want 1", owners)
	}
}

// TestGrantGraphAllowsDemotionWithAnotherOwnerIntegration은 다른 소유자가 남아 있으면
// 강등이 정상 처리되는지 확인한다. 판정이 부여 자체를 막아서는 안 된다.
func TestGrantGraphAllowsDemotionWithAnotherOwnerIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	firstAccountID, secondAccountID := newTestID(t), newTestID(t)
	createTestAccount(t, database, firstAccountID)
	createTestAccount(t, database, secondAccountID)
	graphID := createTestGraph(t, database, firstAccountID)
	grantAccount(t, database, graphID, firstAccountID, model.GraphGradeOwner)
	grantAccount(t, database, graphID, secondAccountID, model.GraphGradeOwner)

	if err := database.GrantGraph(t.Context(), graphID, firstAccountID, secondAccountID, GrantSubjectAccount, model.GraphGradeEditor); err != nil {
		t.Fatalf("다른 소유자가 있는 강등: %v", err)
	}
	grade, found, err := database.EffectiveGrade(t.Context(), graphID, secondAccountID)
	if err != nil || !found || grade != model.GraphGradeEditor {
		t.Fatalf("강등 뒤 등급 = %q, found %t, %v; want editor", grade, found, err)
	}
}

// TestWebRestoreRejectsAutoDeletedGraphIntegration은 유예 만료로 자동 삭제된 그래프를
// 웹 복구가 되살리지 못하는지 확인한다. FR-AGENT_CONTEXT-108이 자동 삭제의 복구를
// 요청과 운영자 경로로만 한정했다.
func TestWebRestoreRejectsAutoDeletedGraphIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)
	grantAccount(t, database, graphID, accountID, model.GraphGradeOwner)

	now := time.Now().UTC()
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at = $2, grace_expires_at = $3 WHERE graph_id = $1`, graphID.String(), now.AddDate(0, 0, -30), now); err != nil {
		t.Fatalf("유예 시작 시각 준비: %v", err)
	}
	if _, err := database.ExpireGrace(t.Context(), now); err != nil {
		t.Fatalf("유예 만료: %v", err)
	}

	err := database.SetGraphDeleted(t.Context(), graphID, accountID, false)
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("자동 삭제 그래프의 웹 복구 = %v, want ErrInvalidState", err)
	}
	graph, err := database.Graph(t.Context(), graphID)
	if err != nil || graph.DeletedAt == nil {
		t.Fatalf("거부 뒤 그래프 상태 = %#v, %v; 삭제가 유지되어야 한다", graph, err)
	}

	// 운영자 경로는 여전히 복구할 수 있어야 한다.
	if err := database.RequestGraphRestore(t.Context(), graphID, accountID); err != nil {
		t.Fatalf("복구 요청: %v", err)
	}
	if err := database.OperatorRestoreGraph(t.Context(), graphID, accountID); err != nil {
		t.Fatalf("운영자 복구: %v", err)
	}
	restored, err := database.Graph(t.Context(), graphID)
	if err != nil || restored.DeletedAt != nil {
		t.Fatalf("운영자 복구 뒤 상태 = %#v, %v", restored, err)
	}
}

// TestWebRestoreAcceptsWebDeletedGraphIntegration은 웹에서 직접 삭제한 그래프의 복구가
// 조건 추가 뒤에도 그대로 동작하는지 확인한다.
func TestWebRestoreAcceptsWebDeletedGraphIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)
	grantAccount(t, database, graphID, accountID, model.GraphGradeOwner)

	if err := database.SetGraphDeleted(t.Context(), graphID, accountID, true); err != nil {
		t.Fatalf("웹 직접 삭제: %v", err)
	}
	if err := database.SetGraphDeleted(t.Context(), graphID, accountID, false); err != nil {
		t.Fatalf("웹 복구: %v", err)
	}
	graph, err := database.Graph(t.Context(), graphID)
	if err != nil || graph.DeletedAt != nil {
		t.Fatalf("웹 복구 뒤 상태 = %#v, %v", graph, err)
	}
}
