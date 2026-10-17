package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func TestGrantRejectsMissingAccountIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	grantAccount(t, database, graph, actor, model.GraphGradeOwner)
	before := grantStateFingerprint(t, database, graph)
	for _, grade := range []model.GraphGrade{model.GraphGradeOwner, model.GraphGradeEditor, model.GraphGradeViewer} {
		t.Run(string(grade), func(t *testing.T) {
			if err := database.GrantGraph(t.Context(), graph, actor, newTestID(t), GrantSubjectAccount, grade); !errors.Is(err, ErrNotFound) {
				t.Fatalf("부재 계정 부여=%v", err)
			}
			if before != grantStateFingerprint(t, database, graph) {
				t.Fatal("부재 계정 부여가 상태·등급·감사를 바꿨다")
			}
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestGrantOwnershipHandoffIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor, successor := newTestID(t), newTestID(t)
	for _, account := range []model.ID{actor, successor} {
		createTestAccount(t, database, account)
	}
	graph := createTestGraph(t, database, actor)
	grantAccount(t, database, graph, actor, model.GraphGradeOwner)
	// 기존 열람자를 소유자로 올린 뒤 이전 소유자가 자기 부여를 회수한다.
	if err := database.GrantGraph(t.Context(), graph, actor, successor, GrantSubjectAccount, model.GraphGradeViewer); err != nil {
		t.Fatal(err)
	}
	if err := database.GrantGraph(t.Context(), graph, actor, successor, GrantSubjectAccount, model.GraphGradeOwner); err != nil {
		t.Fatal(err)
	}
	assertGrade(t, database, graph, actor, model.GraphGradeOwner)
	assertGrade(t, database, graph, successor, model.GraphGradeOwner)
	if err := database.RevokeGraphGrantWithAudit(t.Context(), graph, actor, actor, GrantSubjectAccount); err != nil {
		t.Fatal(err)
	}
	if _, found, err := database.EffectiveGrade(t.Context(), graph, actor); err != nil || found {
		t.Fatalf("인계 뒤 이전 소유자 접근=%t/%v", found, err)
	}
	before := grantStateFingerprint(t, database, graph)
	if err := database.GrantGraph(t.Context(), graph, actor, actor, GrantSubjectAccount, model.GraphGradeOwner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("인계 뒤 이전 소유자의 재부여=%v", err)
	}
	if before != grantStateFingerprint(t, database, graph) {
		t.Fatal("인계 뒤 거부된 재부여가 남았다")
	}
	var transfers, revokes int
	if err := database.pool.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE target_kind='ownership_transfer' AND action='transfer'
			AND subject_id=$2 AND before_grade='viewer' AND after_grade='owner'),
		count(*) FILTER (WHERE target_kind='grant' AND action='revoke'
			AND subject_id=$3 AND before_grade='owner' AND after_grade IS NULL)
		FROM public.web_audit_log WHERE graph_id=$1 AND actor_account_id=$3 AND subject_type='account'`,
		graph.String(), successor.String(), actor.String()).Scan(&transfers, &revokes); err != nil || transfers != 1 || revokes != 1 {
		t.Fatalf("인계·회수 이력=%d/%d/%v", transfers, revokes, err)
	}
	value, err := database.Graph(t.Context(), graph)
	if err != nil || value.CreatedBy != actor || value.GraceStartedAt != nil {
		t.Fatalf("생성자·유예=%+v/%v", value, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestGrantIgnoresDanglingOwnersIntegration(t *testing.T) {
	for _, subjectType := range []GrantSubjectType{GrantSubjectAccount, GrantSubjectTeam} {
		for _, action := range []string{"revoke", "demote"} {
			t.Run(string(subjectType)+"/"+action, func(t *testing.T) {
				database := newSmallPoolStore(t)
				actor, missing := newTestID(t), newTestID(t)
				createTestAccount(t, database, actor)
				graph := createTestGraph(t, database, actor)
				grantAccount(t, database, graph, actor, model.GraphGradeOwner)
				// 외래 키 없는 다형 참조의 과거 잘못된 행을 합성한다. 자동 보정은 하지 않는다.
				if subjectType == GrantSubjectAccount {
					grantAccount(t, database, graph, missing, model.GraphGradeOwner)
				} else {
					team := newTestID(t)
					createTestTeam(t, database, team, actor, false)
					addTestTeamMember(t, database, team, missing)
					grantTeam(t, database, graph, team, model.GraphGradeOwner)
				}
				before := grantStateFingerprint(t, database, graph)
				var err error
				if action == "revoke" {
					err = database.RevokeGraphGrantWithAudit(t.Context(), graph, actor, actor, GrantSubjectAccount)
				} else {
					err = database.GrantGraph(t.Context(), graph, actor, actor, GrantSubjectAccount, model.GraphGradeEditor)
				}
				if !errors.Is(err, ErrLastOwner) || before != grantStateFingerprint(t, database, graph) {
					t.Fatalf("부재 계정 소유자 보호=%v", err)
				}
			})
		}
	}
}

func TestGrantConcurrentLastOwnerIntegration(t *testing.T) {
	for _, subjectType := range []GrantSubjectType{GrantSubjectAccount, GrantSubjectTeam} {
		for _, action := range []string{"revoke", "demote"} {
			t.Run(string(subjectType)+"/"+action, func(t *testing.T) {
				database := newSmallPoolStore(t)
				actors := []model.ID{newTestID(t), newTestID(t)}
				for _, actor := range actors {
					createTestAccount(t, database, actor)
				}
				graph := createTestGraph(t, database, actors[0])
				subjects := slices.Clone(actors)
				for i, actor := range actors {
					if subjectType == GrantSubjectTeam {
						subjects[i] = newTestID(t)
						createTestTeam(t, database, subjects[i], actor, false)
						addTestTeamMember(t, database, subjects[i], actor)
						grantTeam(t, database, graph, subjects[i], model.GraphGradeOwner)
					} else {
						grantAccount(t, database, graph, actor, model.GraphGradeOwner)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				tx, err := database.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer rollbackTransaction(ctx, tx)
				if _, err := lockGraphs(ctx, tx, []model.ID{graph}); err != nil {
					t.Fatal(err)
				}
				var pid int
				if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 2)
				var group sync.WaitGroup
				for i, actor := range actors {
					group.Go(func() {
						if action == "revoke" {
							done <- database.RevokeGraphGrantWithAudit(ctx, graph, actor, subjects[i], subjectType)
						} else {
							done <- database.GrantGraph(ctx, graph, actor, subjects[i], subjectType, model.GraphGradeEditor)
						}
					})
				}
				defer func() { rollbackTransaction(ctx, tx); cancel(); group.Wait() }()
				// 두 요청이 실제로 잠금을 기다리는 것을 확인한 뒤 해제한다.
				waitGrantWaiters(t, ctx, database, pid, 2)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				succeeded, rejected := 0, 0
				for range 2 {
					switch err := <-done; {
					case err == nil:
						succeeded++
					case errors.Is(err, ErrLastOwner):
						rejected++
					default:
						t.Fatalf("동시 변경=%v", err)
					}
				}
				var owners, audits int
				if err := database.pool.QueryRow(ctx, `SELECT
					(SELECT count(DISTINCT account_id) FROM (`+usableOwnersSQL+`) AS owner),
					(SELECT count(*) FROM public.web_audit_log WHERE graph_id=$1)`, graph.String()).Scan(&owners, &audits); err != nil || owners != 1 || audits != 1 || succeeded != 1 || rejected != 1 {
					t.Fatalf("성공/거부/소유자/감사=%d/%d/%d/%d/%v", succeeded, rejected, owners, audits, err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func waitGrantWaiters(t *testing.T, ctx context.Context, database *Store, pid, count int) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		// 같은 행의 두 번째 대기자는 보유자 대신 앞선 대기자의 tuple 잠금에 걸릴 수 있다.
		if err := database.pool.QueryRow(ctx, `WITH RECURSIVE blocked(pid) AS (
			SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid))
			UNION
			SELECT activity.pid FROM pg_stat_activity AS activity
			JOIN blocked ON blocked.pid=ANY(pg_blocking_pids(activity.pid))
			WHERE activity.datname=current_database()
		) SELECT count(*) FROM blocked`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestGrantTargetTeamRecheckedIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor, team := newTestID(t), newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	grantAccount(t, database, graph, actor, model.GraphGradeOwner)
	createTestTeam(t, database, team, actor, false)
	before := grantStateFingerprint(t, database, graph)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(ctx, tx)
	if _, err := lockTeam(ctx, tx, team); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var group sync.WaitGroup
	group.Go(func() { done <- database.GrantGraph(ctx, graph, actor, team, GrantSubjectTeam, model.GraphGradeOwner) })
	defer func() { rollbackTransaction(ctx, tx); cancel(); group.Wait() }()
	waitGrantWaiters(t, ctx, database, pid, 1)
	if _, err := tx.Exec(ctx, `UPDATE public.team SET deleted_at=clock_timestamp() WHERE team_id=$1`, team.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrNotFound) {
		t.Fatalf("대기 중 삭제된 대상 팀=%v", err)
	}
	if before != grantStateFingerprint(t, database, graph) {
		t.Fatal("삭제된 팀 부여가 남았다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestGrantAuditFailureRollsBackIntegration(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%t", deferred), func(t *testing.T) {
			database := newSmallPoolStore(t)
			actor, subject, newcomer, team := newTestID(t), newTestID(t), newTestID(t), newTestID(t)
			createTestAccount(t, database, actor)
			createTestAccount(t, database, subject)
			createTestAccount(t, database, newcomer)
			graph := createTestGraph(t, database, actor)
			grantAccount(t, database, graph, actor, model.GraphGradeOwner)
			grantAccount(t, database, graph, subject, model.GraphGradeOwner)
			createTestTeam(t, database, team, actor, false)
			grantTeam(t, database, graph, team, model.GraphGradeOwner)
			// 정상 접근 계정이 복원된 뒤의 미정리 유예를 합성해 취소의 원자성도 검사한다.
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at=clock_timestamp(), grace_expires_at=clock_timestamp()+interval '30 days' WHERE graph_id=$1`, graph.String()); err != nil {
				t.Fatal(err)
			}
			name := "perm03_audit_" + strings.ReplaceAll(graph.String(), "-", "")
			function := pgx.Identifier{"public", name}.Sanitize()
			trigger := pgx.Identifier{name}.Sanitize()
			definition := `CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON public.web_audit_log FOR EACH ROW EXECUTE FUNCTION ` + function + `()`
			if deferred {
				definition = `CREATE CONSTRAINT TRIGGER ` + trigger + ` AFTER INSERT ON public.web_audit_log DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + function + `()`
			}
			if _, err := database.pool.Exec(t.Context(), `CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.graph_id='`+graph.String()+`'::uuid THEN RAISE EXCEPTION 'synthetic grant audit failure'; END IF; RETURN NEW; END $$; `+definition); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				if _, err := database.pool.Exec(ctx, `DROP TRIGGER `+trigger+` ON public.web_audit_log; DROP FUNCTION `+function+`() `); err != nil {
					t.Error(err)
				}
			})
			before := grantStateFingerprint(t, database, graph)
			for _, action := range []string{"grant_account", "transfer", "demote", "revoke_account", "grant_team", "revoke_team"} {
				t.Run(action, func(t *testing.T) {
					var err error
					switch action {
					case "revoke_account":
						err = database.RevokeGraphGrantWithAudit(t.Context(), graph, actor, subject, GrantSubjectAccount)
					case "revoke_team":
						err = database.RevokeGraphGrantWithAudit(t.Context(), graph, actor, team, GrantSubjectTeam)
					case "grant_team":
						err = database.GrantGraph(t.Context(), graph, actor, team, GrantSubjectTeam, model.GraphGradeViewer)
					case "transfer":
						err = database.GrantGraph(t.Context(), graph, actor, newcomer, GrantSubjectAccount, model.GraphGradeOwner)
					default:
						target := subject
						if action == "grant_account" {
							target = newcomer
						}
						err = database.GrantGraph(t.Context(), graph, actor, target, GrantSubjectAccount, model.GraphGradeEditor)
					}
					if err == nil || !strings.Contains(err.Error(), "synthetic grant audit failure") {
						t.Fatalf("감사·커밋 실패=%v", err)
					}
					if before != grantStateFingerprint(t, database, graph) {
						t.Fatal("감사·커밋 실패 뒤 등급·유예·감사가 남았다")
					}
					assertConnectionRecovery(t, database, nil, 0)
				})
			}
		})
	}
}

func TestGrantCanceledWaitRollsBackIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor, subject := newTestID(t), newTestID(t)
	createTestAccount(t, database, actor)
	createTestAccount(t, database, subject)
	graph := createTestGraph(t, database, actor)
	grantAccount(t, database, graph, actor, model.GraphGradeOwner)
	before := grantStateFingerprint(t, database, graph)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(ctx, tx)
	if _, err := lockGraphs(ctx, tx, []model.ID{graph}); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	done := make(chan error, 1)
	var group sync.WaitGroup
	group.Go(func() {
		done <- database.GrantGraph(requestCtx, graph, actor, subject, GrantSubjectAccount, model.GraphGradeOwner)
	})
	defer func() { rollbackTransaction(ctx, tx); cancelRequest(); group.Wait() }()
	waitGrantWaiters(t, ctx, database, pid, 1)
	cancelRequest()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("잠금 대기 취소=%v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if before != grantStateFingerprint(t, database, graph) {
		t.Fatal("취소된 부여가 남았다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func grantStateFingerprint(t *testing.T, database *Store, graph model.ID) string {
	t.Helper()
	var grants string
	if err := database.pool.QueryRow(t.Context(), `SELECT COALESCE(string_agg(row_to_json(g)::text, '' ORDER BY subject_type, subject_id), '') FROM public.graph_grant AS g WHERE graph_id=$1`, graph.String()).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	return planStateFingerprint(t, database, graph) + grants
}
