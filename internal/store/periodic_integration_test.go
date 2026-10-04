package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestWithTryAdvisoryLockSkipsAndReleasesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	other := newIntegrationStore(t)
	const key int64 = 4182026199

	runs := 0
	_, acquired, err := database.WithTryAdvisoryLock(t.Context(), key, func(ctx context.Context) (int, error) {
		runs++
		_, nested, err := other.WithTryAdvisoryLock(ctx, key, func(context.Context) (int, error) {
			return 0, nil
		})
		if err != nil {
			return 0, err
		}
		if nested {
			t.Fatal("같은 자문 잠금을 두 번째 회차가 얻었다")
		}
		return 1, nil
	})
	if err != nil || !acquired || runs != 1 {
		t.Fatalf("첫 자문 잠금 실행 = acquired:%t runs:%d err:%v", acquired, runs, err)
	}
	_, acquired, err = other.WithTryAdvisoryLock(t.Context(), key, func(context.Context) (int, error) {
		return 1, nil
	})
	if err != nil || !acquired {
		t.Fatalf("해제 뒤 자문 잠금 재획득 = acquired:%t err:%v", acquired, err)
	}

	locked, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, _, err := database.WithTryAdvisoryLock(locked, key, func(ctx context.Context) (int, error) {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		})
		finished <- err
	}()
	<-started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("취소된 자문 잠금 작업 결과 = %v", err)
	}
	_, acquired, err = other.WithTryAdvisoryLock(t.Context(), key, func(context.Context) (int, error) {
		return 1, nil
	})
	if err != nil || !acquired {
		t.Fatalf("취소 뒤 자문 잠금 재획득 = acquired:%t err:%v", acquired, err)
	}
}

func TestPeriodicCleanupExpiresGraceAndOldRowsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at = $2, grace_expires_at = $3 WHERE graph_id = $1`, graphID.String(), now.AddDate(0, 0, -30), now); err != nil {
		t.Fatalf("유예 시작 시각 준비: %v", err)
	}
	expired, err := database.ExpireGrace(t.Context(), now)
	if err != nil || expired < 1 {
		t.Fatalf("유예 만료 = %d, %v", expired, err)
	}
	graph, err := database.Graph(t.Context(), graphID)
	if err != nil || graph.DeletedAt == nil || !graph.DeletedAt.Equal(now) {
		t.Fatalf("자동 소프트 삭제 상태 = %#v, %v", graph, err)
	}

	oldToken := "old-" + accountID.String()
	currentToken := "current-" + accountID.String()
	if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.revoked_token (token_id, expires_at) VALUES ($1, $2), ($3, $4)`, oldToken, now.Add(-time.Minute), currentToken, now.Add(time.Minute)); err != nil {
		t.Fatalf("폐기 목록 준비: %v", err)
	}
	if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.request_rate (account_id, window_started_at, count) VALUES ($1, $2, 1), ($1, $3, 1)`, accountID.String(), now.Add(-time.Minute), now); err != nil {
		t.Fatalf("요청 빈도 창 준비: %v", err)
	}
	if deleted, err := database.CleanupExpiredRevocations(t.Context(), now); err != nil || deleted < 1 {
		t.Fatalf("폐기 목록 정리 = %d, %v", deleted, err)
	}
	if deleted, err := database.CleanupRequestRateWindows(t.Context(), now); err != nil || deleted < 1 {
		t.Fatalf("요청 빈도 창 정리 = %d, %v", deleted, err)
	}
	oldCode := AuthorizationCode{Hash: "old-" + accountID.String(), ClientID: "test-client", AccountID: accountID, RedirectURI: "https://test.invalid/callback", CodeChallenge: "challenge", Resource: "https://test.invalid/mcp", IssuedAt: now.Add(-2 * time.Minute), AuthenticatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute)}
	currentCode := AuthorizationCode{Hash: "current-" + accountID.String(), ClientID: "test-client", AccountID: accountID, RedirectURI: "https://test.invalid/callback", CodeChallenge: "challenge", Resource: "https://test.invalid/mcp", IssuedAt: now, AuthenticatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := database.CreateAuthorizationCode(t.Context(), oldCode); err != nil {
		t.Fatalf("만료 인가 코드 준비: %v", err)
	}
	if err := database.CreateAuthorizationCode(t.Context(), currentCode); err != nil {
		t.Fatalf("현재 인가 코드 준비: %v", err)
	}
	if deleted, err := database.CleanupExpiredAuthorizationCodes(t.Context(), now); err != nil || deleted < 1 {
		t.Fatalf("인가 코드 정리 = %d, %v", deleted, err)
	}
	var oldRevoked, currentRevoked, rates bool
	if err := database.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM public.revoked_token WHERE token_id = $1), EXISTS (SELECT 1 FROM public.revoked_token WHERE token_id = $2)`, oldToken, currentToken).Scan(&oldRevoked, &currentRevoked); err != nil || oldRevoked || !currentRevoked {
		t.Fatalf("폐기 목록 대상 보존 = old:%t current:%t err:%v", oldRevoked, currentRevoked, err)
	}
	if err := database.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM public.request_rate WHERE account_id = $1 AND window_started_at = $2)`, accountID.String(), now).Scan(&rates); err != nil || !rates {
		t.Fatalf("현재 요청 빈도 창 보존 = %t, %v", rates, err)
	}
	var oldAuthorizationCode, currentAuthorizationCode bool
	if err := database.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM public.authorization_code WHERE code_hash = $1), EXISTS (SELECT 1 FROM public.authorization_code WHERE code_hash = $2)`, oldCode.Hash, currentCode.Hash).Scan(&oldAuthorizationCode, &currentAuthorizationCode); err != nil || oldAuthorizationCode || !currentAuthorizationCode {
		t.Fatalf("인가 코드 대상 보존 = old:%t current:%t err:%v", oldAuthorizationCode, currentAuthorizationCode, err)
	}

	oldAuditID := newTestID(t)
	currentAuditID := newTestID(t)
	teamID := newTestID(t)
	if _, err := database.pool.Exec(t.Context(), `
		INSERT INTO public.web_audit_log (audit_id, target_kind, action, actor_account_id, occurred_at, team_id)
		VALUES ($1, 'team', 'add', $2, $3, $4), ($5, 'team', 'add', $2, $6, $4)
	`, oldAuditID.String(), accountID.String(), now.AddDate(0, 0, -91), teamID.String(), currentAuditID.String(), now); err != nil {
		t.Fatalf("팀 감사 로그 준비: %v", err)
	}
	if deleted, err := database.CleanupAuditRecords(t.Context(), now, func(id model.ID) int {
		if id != accountID {
			return 0
		}
		return 90
	}, func(model.ID) int { return 0 }); err != nil || deleted < 1 {
		t.Fatalf("팀 감사 로그 정리 = %d, %v", deleted, err)
	}
	var oldAudit, currentAudit bool
	if err := database.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM public.web_audit_log WHERE audit_id = $1), EXISTS (SELECT 1 FROM public.web_audit_log WHERE audit_id = $2)`, oldAuditID.String(), currentAuditID.String()).Scan(&oldAudit, &currentAudit); err != nil || oldAudit || !currentAudit {
		t.Fatalf("팀 감사 로그 대상 보존 = old:%t current:%t err:%v", oldAudit, currentAudit, err)
	}
}

func TestCleanupExpiredProposalsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, accountID, "https://test.invalid/periodic-proposal"), nil)
	if err != nil {
		t.Fatalf("관계 후보 원천 생성: %v", err)
	}
	first, err := database.CreateContext(t.Context(), graphID, testEventContext(t, graphID, accountID, source.ID), nil)
	if err != nil {
		t.Fatalf("첫 사건 생성: %v", err)
	}
	secondInput := testEventContext(t, graphID, accountID, source.ID)
	second, err := database.CreateContext(t.Context(), graphID, secondInput, nil)
	if err != nil {
		t.Fatalf("두 번째 사건 생성: %v", err)
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	if _, err := database.CreateRelation(t.Context(), graphID, model.Relation{
		ID: newTestID(t), GraphID: graphID, Type: model.RelationTypeRelatesTo, FromContextID: first.ID, ToContextID: second.ID,
		State: model.RelationStateProposed, ProposedBy: model.ProposalSourceSystem, ProposedAt: now.AddDate(0, 0, -91),
	}); err != nil {
		t.Fatalf("오래된 관계 후보 생성: %v", err)
	}
	deleted, err := database.CleanupExpiredProposals(t.Context(), now, func(id model.ID) int {
		if id == accountID {
			return 90
		}
		return 0
	})
	if err != nil || deleted != 1 {
		t.Fatalf("오래된 관계 후보 정리 = %d, %v", deleted, err)
	}
	relations, _, err := database.ListRelations(t.Context(), graphID, "", 10)
	if err != nil || len(relations) != 0 {
		t.Fatalf("정리 뒤 관계 후보 목록 = %#v, %v", relations, err)
	}
}

// TestGraceExpiryIsFixedAtGraceStartIntegration은 유예 시작 시점의 플랜 값으로 만료 시각을
// 고정해 이후 플랜 변경이 소급되지 않는지, 만료 시각이 없는 유예는 지우지 않는지 확인한다.
func TestGraceExpiryIsFixedAtGraceStartIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graceDays := 30
	database.graceDays = func(model.ID) int { return graceDays }
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	teamID := newTestID(t)
	createTestTeam(t, database, teamID, accountID, false)
	addTestTeamMember(t, database, teamID, accountID)
	graphID := createTestGraph(t, database, accountID)
	grantTeam(t, database, graphID, teamID, model.GraphGradeOwner)
	if err := database.RemoveTeamMember(t.Context(), teamID, accountID); err != nil {
		t.Fatalf("팀 구성원 제거: %v", err)
	}
	started, expires := graceTimes(t, database, graphID)
	if started == nil || expires == nil || !expires.Equal(started.AddDate(0, 0, 30)) {
		t.Fatalf("유예 만료 시각 = started:%v expires:%v", started, expires)
	}

	// 유예 중에 유예 일수가 줄어도 이미 고정한 만료 시각을 쓴다.
	graceDays = 1
	if expired, err := database.ExpireGrace(t.Context(), started.AddDate(0, 0, 2)); err != nil {
		t.Fatalf("만료 전 유예 만료 실행: %v", err)
	} else if graph, err := database.Graph(t.Context(), graphID); err != nil || graph.DeletedAt != nil {
		t.Fatalf("플랜 변경이 소급되어 그래프가 삭제됐다: expired=%d graph=%#v err=%v", expired, graph, err)
	}
	if _, err := database.ExpireGrace(t.Context(), *expires); err != nil {
		t.Fatalf("만료 시각 유예 만료 실행: %v", err)
	}
	if graph, err := database.Graph(t.Context(), graphID); err != nil || graph.DeletedAt == nil || graph.GraceStartedAt == nil {
		t.Fatalf("만료 뒤 운영자 복구 가능 상태 = %#v, %v", graph, err)
	}

	// 유예 일수가 0이면 만료 시각을 남기지 않아 만료하지 않는다.
	graceDays = 0
	unlimitedTeamID := newTestID(t)
	createTestTeam(t, database, unlimitedTeamID, accountID, false)
	addTestTeamMember(t, database, unlimitedTeamID, accountID)
	unlimitedGraphID := createTestGraph(t, database, accountID)
	grantTeam(t, database, unlimitedGraphID, unlimitedTeamID, model.GraphGradeOwner)
	if err := database.RemoveTeamMember(t.Context(), unlimitedTeamID, accountID); err != nil {
		t.Fatalf("무기한 팀 구성원 제거: %v", err)
	}
	if started, expires := graceTimes(t, database, unlimitedGraphID); started == nil || expires != nil {
		t.Fatalf("무기한 유예 시각 = started:%v expires:%v", started, expires)
	}
	if _, err := database.ExpireGrace(t.Context(), time.Now().UTC().AddDate(1, 0, 0)); err != nil {
		t.Fatalf("무기한 유예 만료 실행: %v", err)
	}
	if graph, err := database.Graph(t.Context(), unlimitedGraphID); err != nil || graph.DeletedAt != nil {
		t.Fatalf("만료 시각이 없는 유예가 삭제됐다: %#v, %v", graph, err)
	}
}

// TestGraceCancelRequiresAccessibleAccountIntegration은 구성원이 없는 팀에 등급을 부여해도
// 유예가 남고, 그 팀에 구성원이 생겨 접근 가능한 계정이 실제로 생길 때 취소되는지 확인한다.
func TestGraceCancelRequiresAccessibleAccountIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	database.graceDays = func(model.ID) int { return 30 }
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	teamID := newTestID(t)
	createTestTeam(t, database, teamID, accountID, false)
	addTestTeamMember(t, database, teamID, accountID)
	graphID := createTestGraph(t, database, accountID)
	grantTeam(t, database, graphID, teamID, model.GraphGradeOwner)
	if err := database.RemoveTeamMember(t.Context(), teamID, accountID); err != nil {
		t.Fatalf("팀 구성원 제거: %v", err)
	}

	emptyTeamID := newTestID(t)
	createTestTeam(t, database, emptyTeamID, accountID, false)
	if err := database.GrantGraph(t.Context(), graphID, accountID, emptyTeamID, GrantSubjectTeam, model.GraphGradeViewer); err != nil {
		t.Fatalf("빈 팀 등급 부여: %v", err)
	}
	if started, expires := graceTimes(t, database, graphID); started == nil || expires == nil {
		t.Fatalf("빈 팀 등급 부여가 유예를 취소했다: started:%v expires:%v", started, expires)
	}

	if err := database.AddTeamMember(t.Context(), emptyTeamID, accountID, accountID); err != nil {
		t.Fatalf("팀 구성원 추가: %v", err)
	}
	if started, expires := graceTimes(t, database, graphID); started != nil || expires != nil {
		t.Fatalf("접근 가능한 계정이 생긴 뒤 유예가 남았다: started:%v expires:%v", started, expires)
	}
}

func TestDeleteProposedRelationMissingIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)
	removed, err := database.deleteProposedRelation(t.Context(), graphID, newTestID(t))
	if err != nil || removed {
		t.Fatalf("이미 사라진 관계 후보 삭제 = removed:%t err:%v", removed, err)
	}
}

func graceTimes(t *testing.T, database *Store, graphID model.ID) (started, expires *time.Time) {
	t.Helper()
	if err := database.pool.QueryRow(t.Context(), `SELECT grace_started_at, grace_expires_at FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&started, &expires); err != nil {
		t.Fatalf("유예 시각 조회: %v", err)
	}
	return started, expires
}
