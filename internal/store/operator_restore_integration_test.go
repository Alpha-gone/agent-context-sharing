package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// operatorRestoreFixture는 마지막 팀 구성원 제거와 유예 만료로 실제 접근을 잃은
// 그래프를 만든다. formerOwner면 생성자가 아닌 과거 소유자가 복구를 요청한다.
func operatorRestoreFixture(t *testing.T, database *Store, formerOwner bool) (model.ID, model.ID, model.ID) {
	t.Helper()
	creatorID, operatorID := newTestID(t), newTestID(t)
	createTestAccount(t, database, creatorID)
	createTestAccount(t, database, operatorID)
	requesterID := creatorID
	if formerOwner {
		requesterID = newTestID(t)
		createTestAccount(t, database, requesterID)
	}
	graphID := createTestGraph(t, database, creatorID)
	teamID := newTestID(t)
	createTestTeam(t, database, teamID, creatorID, false)
	addTestTeamMember(t, database, teamID, requesterID)
	grantTeam(t, database, graphID, teamID, model.GraphGradeOwner)
	if formerOwner {
		if err := database.GrantGraph(t.Context(), graphID, requesterID, requesterID, GrantSubjectAccount, model.GraphGradeOwner); err != nil {
			t.Fatalf("소유자 이력 준비: %v", err)
		}
		if err := database.RevokeGraphGrantWithAudit(t.Context(), graphID, requesterID, requesterID, GrantSubjectAccount); err != nil {
			t.Fatalf("직접 소유자 회수: %v", err)
		}
	}
	if err := database.RemoveTeamMember(t.Context(), teamID, requesterID); err != nil {
		t.Fatalf("마지막 접근 계정 제거: %v", err)
	}
	// 이 표본의 유예만 과거로 옮기고 현재 시각으로 만료시킨다.
	now := time.Now().UTC()
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at = $2, grace_expires_at = $3 WHERE graph_id = $1 AND grace_started_at IS NOT NULL`, graphID.String(), now.AddDate(0, 0, -30), now); err != nil {
		t.Fatalf("유예 만료 준비: %v", err)
	}
	if _, err := database.ExpireGrace(t.Context(), now); err != nil {
		t.Fatalf("자동 소프트 삭제: %v", err)
	}
	if err := database.RequestGraphRestore(t.Context(), graphID, requesterID); err != nil {
		t.Fatalf("복구 요청: %v", err)
	}
	return graphID, requesterID, operatorID
}

func TestOperatorRestoreOwnerIntegration(t *testing.T) {
	for _, formerOwner := range []bool{false, true} {
		name := "creator"
		if formerOwner {
			name = "former_owner"
		}
		t.Run(name, func(t *testing.T) {
			database := newIntegrationStore(t)
			graphID, requesterID, operatorID := operatorRestoreFixture(t, database, formerOwner)
			if _, found, err := database.EffectiveGrade(t.Context(), graphID, requesterID); err != nil || found {
				t.Fatalf("복구 전 등급: found %t, 오류 %v", found, err)
			}
			if err := database.OperatorRestoreGraph(t.Context(), graphID, operatorID); err != nil {
				t.Fatalf("운영자 복구: %v", err)
			}
			grade, found, err := database.EffectiveGrade(t.Context(), graphID, requesterID)
			if err != nil || !found || grade != model.GraphGradeOwner {
				t.Fatalf("복구 요청자 등급 = %q, found %t, 오류 %v; want owner", grade, found, err)
			}
			if _, found, err := database.EffectiveGrade(t.Context(), graphID, operatorID); err != nil || found {
				t.Fatalf("운영자에게 등급이 생겼다: found %t, 오류 %v", found, err)
			}
			var deletedAt, graceStartedAt, graceExpiresAt *time.Time
			if err := database.pool.QueryRow(t.Context(), `SELECT deleted_at, grace_started_at, grace_expires_at FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&deletedAt, &graceStartedAt, &graceExpiresAt); err != nil || deletedAt != nil || graceStartedAt != nil || graceExpiresAt != nil {
				t.Fatalf("복구 뒤 삭제·유예 상태 = %v, %v, %v, 오류 %v", deletedAt, graceStartedAt, graceExpiresAt, err)
			}
			graphs, _, err := database.ListGraphs(t.Context(), requesterID, model.GraphListFilter{}, "", 50)
			if err != nil || len(graphs) != 1 || graphs[0].ID != graphID || graphs[0].Grade != model.GraphGradeOwner {
				t.Fatalf("요청자의 활성 그래프 목록 = %#v, 오류 %v", graphs, err)
			}
			var before *string
			var after, subject, actor, requester string
			if err := database.pool.QueryRow(t.Context(), `SELECT before_grade, after_grade, subject_id, actor_account_id, requester_account_id FROM public.web_audit_log WHERE graph_id = $1 AND target_kind = 'operator_restore'`, graphID.String()).Scan(&before, &after, &subject, &actor, &requester); err != nil || before != nil || after != "owner" || subject != requesterID.String() || actor != operatorID.String() || requester != requesterID.String() {
				t.Fatalf("복구 감사의 등급·주체·요청자 = %v, %q, %q, %q, %q, 오류 %v", before, after, subject, actor, requester, err)
			}
			var members int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.team_member WHERE account_id = $1`, requesterID.String()).Scan(&members); err != nil || members != 0 {
				t.Fatalf("복구가 팀 구성원을 되살렸다: %d, 오류 %v", members, err)
			}
		})
	}
}

func TestOperatorRestoreAuditFailureRollsBackIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graphID, requesterID, operatorID := operatorRestoreFixture(t, database, false)
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("감사 실패 트랜잭션 준비: %v", err)
	}
	defer tx.Rollback(t.Context())
	failure := errors.New("시험용 웹 감사 기록 실패")
	wrapped := &operatorRestoreAuditFailureTx{Tx: tx, failure: failure}
	if err := database.OperatorRestoreGraph(context.WithValue(t.Context(), writeTransactionContextKey{}, pgx.Tx(wrapped)), graphID, operatorID); !errors.Is(err, failure) {
		t.Fatalf("감사 실패 결과 = %v, want %v", err, failure)
	}
	graph, err := database.Graph(t.Context(), graphID)
	if err != nil || graph.DeletedAt == nil || graph.GraceStartedAt == nil {
		t.Fatalf("감사 실패 뒤 삭제 상태가 사라졌다: %#v, 오류 %v", graph, err)
	}
	if _, found, err := database.EffectiveGrade(t.Context(), graphID, requesterID); err != nil || found {
		t.Fatalf("실패한 복구의 소유자 등급이 남았다: found %t, 오류 %v", found, err)
	}
	var grants int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM public.graph_grant WHERE graph_id = $1 AND subject_type = 'account' AND subject_id = $2`, graphID.String(), requesterID.String()).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("저장점 롤백 뒤 직접 등급 = %d, 오류 %v", grants, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("시험 트랜잭션 정리: %v", err)
	}
	if err := database.OperatorRestoreGraph(t.Context(), graphID, operatorID); err != nil {
		t.Fatalf("감사 실패 뒤 재시도: %v", err)
	}
}

// operatorRestoreAuditFailureTx는 실제 그래프·등급 쓰기 뒤 감사 쓰기만 실패시킨다.
type operatorRestoreAuditFailureTx struct {
	pgx.Tx
	failure error
}

func (tx *operatorRestoreAuditFailureTx) Begin(ctx context.Context) (pgx.Tx, error) {
	child, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &operatorRestoreAuditFailureTx{Tx: child, failure: tx.failure}, nil
}

func (tx *operatorRestoreAuditFailureTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO public.web_audit_log") {
		return pgconn.CommandTag{}, tx.failure
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestOperatorRestoreConcurrentAppliesOnceIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graphID, _, operatorID := operatorRestoreFixture(t, database, false)
	var waitGroup sync.WaitGroup
	results := make(chan error, 4)
	start := make(chan struct{})
	for range 4 {
		waitGroup.Go(func() {
			<-start
			results <- database.OperatorRestoreGraph(t.Context(), graphID, operatorID)
		})
	}
	close(start)
	waitGroup.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalidState) {
			t.Fatalf("동시 운영자 복구: %v", err)
		}
	}
	var records int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log WHERE graph_id = $1 AND target_kind = 'operator_restore'`, graphID.String()).Scan(&records); err != nil || succeeded != 1 || records != 1 {
		t.Fatalf("동시 복구 성공·기록 = %d, %d, 오류 %v; want 각각 1", succeeded, records, err)
	}
}

func TestOperatorRestoreKeepsOtherGrantsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graphID, requesterID, operatorID := operatorRestoreFixture(t, database, false)
	otherID := newTestID(t)
	createTestAccount(t, database, otherID)
	// 삭제 후 등급이 다시 연결된 표본에서도 요청자의 직접 등급만 복원한다.
	for accountID, grade := range map[model.ID]model.GraphGrade{requesterID: model.GraphGradeViewer, otherID: model.GraphGradeEditor} {
		grantAccount(t, database, graphID, accountID, grade)
	}
	if err := database.OperatorRestoreGraph(t.Context(), graphID, operatorID); err != nil {
		t.Fatalf("기존 등급이 있는 복구: %v", err)
	}
	for accountID, want := range map[model.ID]model.GraphGrade{requesterID: model.GraphGradeOwner, otherID: model.GraphGradeEditor} {
		grade, found, err := database.EffectiveGrade(t.Context(), graphID, accountID)
		if err != nil || !found || grade != want {
			t.Fatalf("복구 뒤 등급 = %q, found %t, 오류 %v; want %q", grade, found, err, want)
		}
	}
	var before, after string
	if err := database.pool.QueryRow(t.Context(), `SELECT before_grade, after_grade FROM public.web_audit_log WHERE graph_id = $1 AND target_kind = 'operator_restore'`, graphID.String()).Scan(&before, &after); err != nil || before != "viewer" || after != "owner" {
		t.Fatalf("복원한 등급의 감사 = %q→%q, 오류 %v", before, after, err)
	}
}

func TestOperatorRestoreRequiresPendingAutoDeletionIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	for _, state := range []string{"active", "web_deleted", "auto_deleted_without_request"} {
		t.Run(state, func(t *testing.T) {
			graphID := createTestGraph(t, database, actorID)
			if state != "active" {
				if err := database.SetGraphDeleted(t.Context(), graphID, actorID, true); err != nil {
					t.Fatalf("삭제 준비: %v", err)
				}
			}
			if state == "auto_deleted_without_request" {
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at = clock_timestamp() WHERE graph_id = $1`, graphID.String()); err != nil {
					t.Fatalf("자동 삭제 표본 준비: %v", err)
				}
			} else {
				// 처리되지 않은 과거 요청이 있어도 활성·웹 삭제 그래프는 복구하지 않는다.
				tx, err := database.pool.Begin(t.Context())
				if err != nil {
					t.Fatalf("과거 요청 준비: %v", err)
				}
				defer tx.Rollback(t.Context())
				if err := database.commitWebAudit(t.Context(), tx, webAuditRecord{TargetKind: "restore_request", Action: "request", ActorID: actorID, GraphID: graphID, RequesterAccountID: actorID}); err != nil {
					t.Fatalf("과거 요청 기록: %v", err)
				}
			}
			if err := database.OperatorRestoreGraph(t.Context(), graphID, actorID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("복구 대상 밖의 결과 = %v, want ErrNotFound", err)
			}
			if _, found, err := database.EffectiveGrade(t.Context(), graphID, actorID); err != nil || found {
				t.Fatalf("거부한 복구가 등급을 부여했다: found %t, 오류 %v", found, err)
			}
		})
	}
}
