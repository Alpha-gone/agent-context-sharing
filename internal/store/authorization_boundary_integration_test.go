package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestGradeMutationRechecksActorAfterGraphLockIntegration(t *testing.T) {
	for _, action := range []string{"grant", "grant_team", "revoke"} {
		for _, change := range []string{"직접 등급 하향", "팀 구성원 제거", "팀 삭제"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				database := newIntegrationStore(t)
				actorID, remainingID, targetID := newTestID(t), newTestID(t), newTestID(t)
				for _, id := range []model.ID{actorID, remainingID, targetID} {
					createTestAccount(t, database, id)
				}
				graphID := createTestGraph(t, database, actorID)
				grantAccount(t, database, graphID, remainingID, model.GraphGradeOwner)
				grantAccount(t, database, graphID, targetID, model.GraphGradeViewer)
				targetTeamID := newTestID(t)
				if action == "grant_team" {
					createTestTeam(t, database, targetTeamID, remainingID, false)
				}
				teamID := newTestID(t)
				if change == "직접 등급 하향" {
					grantAccount(t, database, graphID, actorID, model.GraphGradeOwner)
				} else {
					createTestTeam(t, database, teamID, actorID, false)
					addTestTeamMember(t, database, teamID, actorID)
					grantTeam(t, database, graphID, teamID, model.GraphGradeOwner)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				tx, err := database.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err := lockGraphs(ctx, tx, []model.ID{graphID}); err != nil {
					t.Fatal(err)
				}
				var holderPID int
				if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
					t.Fatal(err)
				}
				// HTTP 처리기가 변경 전에 관측했던 소유자 등급을 재현한다.
				if grade, found, err := database.EffectiveGrade(ctx, graphID, actorID); err != nil || !found || grade != model.GraphGradeOwner {
					t.Fatalf("선행 인가 = %q %t %v", grade, found, err)
				}
				result := make(chan error, 1)
				var worker sync.WaitGroup
				worker.Go(func() {
					if action == "grant" {
						result <- database.GrantGraph(ctx, graphID, actorID, targetID, GrantSubjectAccount, model.GraphGradeEditor)
					} else if action == "grant_team" {
						result <- database.GrantGraph(ctx, graphID, actorID, targetTeamID, GrantSubjectTeam, model.GraphGradeEditor)
					} else {
						result <- database.RevokeGraphGrantWithAudit(ctx, graphID, actorID, targetID, GrantSubjectAccount)
					}
				})
				defer func() { tx.Rollback(ctx); cancel(); worker.Wait() }()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var blocked bool
					if err := database.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND $1::integer = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					if blocked {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-ticker.C:
					}
				}
				switch change {
				case "직접 등급 하향":
					_, err = tx.Exec(ctx, `UPDATE public.graph_grant SET grade = 'viewer' WHERE graph_id = $1 AND subject_type = 'account' AND subject_id = $2`, graphID.String(), actorID.String())
				case "팀 구성원 제거":
					_, err = tx.Exec(ctx, `DELETE FROM public.team_member WHERE team_id = $1 AND account_id = $2`, teamID.String(), actorID.String())
				case "팀 삭제":
					_, err = tx.Exec(ctx, `UPDATE public.team SET deleted_at = now() WHERE team_id = $1`, teamID.String())
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-result; !errors.Is(err, ErrNotFound) {
					t.Fatalf("등급을 잃은 행위자의 변경 = %v", err)
				}
				if grade, found, err := database.EffectiveGrade(ctx, graphID, targetID); err != nil || !found || grade != model.GraphGradeViewer {
					t.Fatalf("대상 등급이 바뀌었다: %q %t %v", grade, found, err)
				}
				var audits int
				if action == "grant_team" {
					var grants int
					if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM public.graph_grant WHERE graph_id = $1 AND subject_type = 'team' AND subject_id = $2`, graphID.String(), targetTeamID.String()).Scan(&grants); err != nil || grants != 0 {
						t.Fatalf("거부한 팀 부여 = %d, 오류 %v", grants, err)
					}
				}
				if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM public.web_audit_log WHERE graph_id = $1`, graphID.String()).Scan(&audits); err != nil || audits != 0 {
					t.Fatalf("거부한 변경의 감사 기록 = %d, 오류 %v", audits, err)
				}
			})
		}
	}
}

func TestActiveTeamOwnerCanMutateGraphGrantsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID, targetID := newTestID(t), newTestID(t)
	createTestAccount(t, database, actorID)
	createTestAccount(t, database, targetID)
	graphID := createTestGraph(t, database, actorID)
	teamID := newTestID(t)
	createTestTeam(t, database, teamID, actorID, false)
	addTestTeamMember(t, database, teamID, actorID)
	grantTeam(t, database, graphID, teamID, model.GraphGradeOwner)
	grantAccount(t, database, graphID, actorID, model.GraphGradeViewer)
	if err := database.GrantGraph(t.Context(), graphID, actorID, targetID, GrantSubjectAccount, model.GraphGradeEditor); err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeGraphGrantWithAudit(t.Context(), graphID, actorID, targetID, GrantSubjectAccount); err != nil {
		t.Fatal(err)
	}
}
