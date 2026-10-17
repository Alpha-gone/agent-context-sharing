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

func TestTeamLifecycleContractIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	manager, other, first, second := newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	for _, account := range []model.ID{manager, other, first, second} {
		createTestAccount(t, database, account)
	}
	team, err := database.CreateTeam(t.Context(), manager, "공유 팀")
	if err != nil || team.ManagerAccountID != manager || !team.ID.IsV7() || team.CreatedAt.Location() != time.UTC {
		t.Fatalf("그래프 없는 계정의 팀 생성=%+v/%v", team, err)
	}
	foreign, err := database.CreateTeam(t.Context(), other, "다른 관리자 팀")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), foreign.ID, other, other); err != nil {
		t.Fatal(err)
	}
	initial, err := database.ListManagedTeams(t.Context(), manager)
	if err != nil || len(initial) != 1 || len(initial[0].MemberLoginIDs) != 0 {
		t.Fatalf("관리자 자동 구성원 추가 또는 다른 팀 노출=%+v/%v", initial, err)
	}
	graphs := []model.ID{createTestGraph(t, database, manager), createTestGraph(t, database, manager)}
	for _, graph := range graphs {
		grantTeam(t, database, graph, team.ID, model.GraphGradeOwner)
	}
	for _, member := range []model.ID{second, first} {
		if err := database.AddTeamMember(t.Context(), team.ID, manager, member); err != nil {
			t.Fatal(err)
		}
	}
	for _, graph := range graphs {
		assertGrade(t, database, graph, first, model.GraphGradeOwner)
		if _, found, err := database.EffectiveGrade(t.Context(), graph, manager); err != nil || found {
			t.Fatalf("관리자에게 자동 상속=%t/%v", found, err)
		}
	}
	if err := database.RemoveTeamMemberWithAudit(t.Context(), team.ID, manager, first); err != nil {
		t.Fatal(err)
	}
	assertTeamGrace(t, database, graphs, false)
	if err := database.RemoveTeamMemberWithAudit(t.Context(), team.ID, manager, second); err != nil {
		t.Fatal(err)
	}
	assertTeamGrace(t, database, graphs, true)
	if err := database.AddTeamMember(t.Context(), team.ID, manager, first); err != nil {
		t.Fatal(err)
	}
	assertTeamGrace(t, database, graphs, false)
	if err := database.SetTeamDeleted(t.Context(), team.ID, manager, true); err != nil {
		t.Fatal(err)
	}
	assertTeamGrace(t, database, graphs, true)
	if err := database.SetTeamDeleted(t.Context(), team.ID, manager, true); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("팀 재삭제=%v", err)
	}
	if err := database.AddTeamMember(t.Context(), team.ID, manager, second); err != nil {
		t.Fatal(err)
	}
	assertTeamGrace(t, database, graphs, true)
	for _, graph := range graphs {
		if _, found, err := database.EffectiveGrade(t.Context(), graph, first); err != nil || found {
			t.Fatalf("삭제 팀의 상속=%t/%v", found, err)
		}
	}
	deleted, err := database.ListManagedTeams(t.Context(), manager)
	if err != nil || len(deleted) != 1 || deleted[0].DeletedAt == nil || deleted[0].ManagerAccountID != manager || len(deleted[0].MemberLoginIDs) != 2 || !slices.IsSorted(deleted[0].MemberLoginIDs) {
		t.Fatalf("삭제 팀 보존·관리자 범위·정렬=%+v/%v", deleted, err)
	}
	if err := database.SetTeamDeleted(t.Context(), team.ID, manager, false); err != nil {
		t.Fatal(err)
	}
	assertTeamGrace(t, database, graphs, false)
	for _, graph := range graphs {
		assertGrade(t, database, graph, first, model.GraphGradeOwner)
		assertGrade(t, database, graph, second, model.GraphGradeOwner)
	}
	var grants, changes int
	if err := database.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM public.graph_grant WHERE subject_type='team' AND subject_id=$1 AND grade='owner'),
		(SELECT count(*) FROM public.web_audit_log WHERE team_id=$1 AND actor_account_id=$2 AND target_kind='team')`, team.ID.String(), manager.String()).Scan(&grants, &changes); err != nil || grants != 2 || changes != 9 {
		t.Fatalf("보존 부여·팀 감사=%d/%d/%v", grants, changes, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestTeamReferenceAndManagerBoundaryIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	manager, stranger, missing := newTestID(t), newTestID(t), newTestID(t)
	createTestAccount(t, database, manager)
	createTestAccount(t, database, stranger)
	before := teamStateFingerprint(t, database, missing)
	if _, err := database.CreateTeam(t.Context(), missing, "부재 관리자"); !errors.Is(err, ErrNotFound) || before != teamStateFingerprint(t, database, missing) {
		t.Fatalf("부재 관리자 생성=%v", err)
	}
	if _, err := database.CreateTeam(t.Context(), manager, ""); err == nil {
		t.Fatal("빈 팀 이름을 허용했다")
	} else if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != "name" {
		t.Fatalf("팀 이름 입력 오류=%v", err)
	}
	team, err := database.CreateTeam(t.Context(), manager, "경계 팀")
	if err != nil {
		t.Fatal(err)
	}
	before = teamStateFingerprint(t, database, manager)
	if err := database.AddTeamMember(t.Context(), team.ID, manager, missing); !errors.Is(err, ErrNotFound) || before != teamStateFingerprint(t, database, manager) {
		t.Fatalf("부재 구성원 추가=%v", err)
	}
	for _, action := range []string{"add", "remove", "delete", "restore"} {
		t.Run(action, func(t *testing.T) {
			if err := teamMutation(t.Context(), database, team.ID, stranger, manager, action); !errors.Is(err, ErrNotFound) || before != teamStateFingerprint(t, database, manager) {
				t.Fatalf("다른 관리자의 변경=%v", err)
			}
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestTeamDanglingMembersDoNotCancelGraceIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	database.graceDays = func(model.ID) int { return 30 }
	manager, missing := newTestID(t), newTestID(t)
	createTestAccount(t, database, manager)
	team, err := database.CreateTeam(t.Context(), manager, "과거 참조 경계")
	if err != nil {
		t.Fatal(err)
	}
	graph := createTestGraph(t, database, manager)
	grantTeam(t, database, graph, team.ID, model.GraphGradeOwner)
	// 과거 잘못된 직접·구성원 참조를 합성한다. 유예 판정이 이를 실제 계정으로 세면 안 된다.
	grantAccount(t, database, graph, missing, model.GraphGradeViewer)
	addTestTeamMember(t, database, team.ID, missing)
	if err := database.SetTeamDeleted(t.Context(), team.ID, manager, true); err != nil {
		t.Fatal(err)
	}
	assertTeamGrace(t, database, []model.ID{graph}, true)
	started, expires := graceTimes(t, database, graph)
	if err := database.SetTeamDeleted(t.Context(), team.ID, manager, false); err != nil {
		t.Fatal(err)
	}
	afterStart, afterExpiry := graceTimes(t, database, graph)
	if afterStart == nil || afterExpiry == nil || !afterStart.Equal(*started) || !afterExpiry.Equal(*expires) {
		t.Fatal("부재 계정으로 유예가 취소되거나 변경됐다")
	}
}

func TestTeamOverlappingGraphsConcurrentIntegration(t *testing.T) {
	for _, action := range []string{"remove", "delete", "restore"} {
		t.Run(action, func(t *testing.T) {
			database := newSmallPoolStore(t)
			database.graceDays = func(model.ID) int { return 30 }
			manager := newTestID(t)
			createTestAccount(t, database, manager)
			graphs := []model.ID{createTestGraph(t, database, manager), createTestGraph(t, database, manager)}
			teams := []model.ID{newTestID(t), newTestID(t)}
			for _, team := range teams {
				createTestTeam(t, database, team, manager, action == "restore")
				addTestTeamMember(t, database, team, manager)
				for _, graph := range graphs {
					grantTeam(t, database, graph, team, model.GraphGradeOwner)
				}
			}
			if action == "restore" {
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at=clock_timestamp(), grace_expires_at=clock_timestamp()+interval '30 days' WHERE graph_id=ANY($1::uuid[])`, []string{graphs[0].String(), graphs[1].String()}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(ctx, tx)
			if _, err := lockGraphs(ctx, tx, graphs); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 2)
			var group sync.WaitGroup
			for _, team := range teams {
				group.Go(func() { done <- teamMutation(ctx, database, team, manager, manager, action) })
			}
			defer func() { rollbackTransaction(ctx, tx); cancel(); group.Wait() }()
			waitGrantWaiters(t, ctx, database, pid, 2)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			assertTeamGrace(t, database, graphs, action != "restore")
			var changes int
			if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM public.web_audit_log WHERE actor_account_id=$1 AND target_kind='team'`, manager.String()).Scan(&changes); err != nil || changes != 2 {
				t.Fatalf("동시 팀 변경 감사=%d/%v", changes, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestTeamAuditFailureAtomicityIntegration(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%t", deferred), func(t *testing.T) {
			database := newSmallPoolStore(t)
			database.graceDays = func(model.ID) int { return 30 }
			manager := newTestID(t)
			createTestAccount(t, database, manager)
			team := newTestID(t)
			createTestTeam(t, database, team, manager, false)
			addTestTeamMember(t, database, team, manager)
			graph := createTestGraph(t, database, manager)
			grantTeam(t, database, graph, team, model.GraphGradeOwner)
			name := "perm04_audit_" + strings.ReplaceAll(manager.String(), "-", "")
			function, trigger := pgx.Identifier{"public", name}.Sanitize(), pgx.Identifier{name}.Sanitize()
			definition := `CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON public.web_audit_log FOR EACH ROW EXECUTE FUNCTION ` + function + `()`
			if deferred {
				definition = `CREATE CONSTRAINT TRIGGER ` + trigger + ` AFTER INSERT ON public.web_audit_log DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + function + `()`
			}
			if _, err := database.pool.Exec(t.Context(), `CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.actor_account_id='`+manager.String()+`'::uuid AND NEW.target_kind='team' THEN RAISE EXCEPTION 'synthetic team audit failure'; END IF; RETURN NEW; END $$; `+definition); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				if _, err := database.pool.Exec(ctx, `DROP TRIGGER `+trigger+` ON public.web_audit_log; DROP FUNCTION `+function+`() `); err != nil {
					t.Error(err)
				}
			})
			for _, action := range []string{"create", "add", "remove", "delete", "restore"} {
				t.Run(action, func(t *testing.T) {
					if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at=NULL, grace_expires_at=NULL WHERE graph_id=$1`, graph.String()); err != nil {
						t.Fatal(err)
					}
					// 복구와 구성원 추가는 접근 없는 유예 상태를 준비해 취소까지 검사한다.
					if action == "restore" {
						if _, err := database.pool.Exec(t.Context(), `UPDATE public.team SET deleted_at=clock_timestamp() WHERE team_id=$1`, team.String()); err != nil {
							t.Fatal(err)
						}
					}
					if action == "add" {
						if _, err := database.pool.Exec(t.Context(), `DELETE FROM public.team_member WHERE team_id=$1`, team.String()); err != nil {
							t.Fatal(err)
						}
					} else if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.team_member VALUES ($1,$2) ON CONFLICT DO NOTHING`, team.String(), manager.String()); err != nil {
						t.Fatal(err)
					}
					if action == "add" || action == "restore" {
						if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at=clock_timestamp(), grace_expires_at=clock_timestamp()+interval '30 days' WHERE graph_id=$1`, graph.String()); err != nil {
							t.Fatal(err)
						}
					}
					before := teamStateFingerprint(t, database, manager)
					err := teamMutation(t.Context(), database, team, manager, manager, action)
					if err == nil || !strings.Contains(err.Error(), "synthetic team audit failure") || before != teamStateFingerprint(t, database, manager) {
						t.Fatalf("팀 변경·감사·유예 롤백=%v", err)
					}
					assertConnectionRecovery(t, database, nil, 0)
				})
			}
		})
	}
}

func TestTeamCanceledWaitIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	manager := newTestID(t)
	createTestAccount(t, database, manager)
	team := newTestID(t)
	createTestTeam(t, database, team, manager, false)
	before := teamStateFingerprint(t, database, manager)
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
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	done := make(chan error, 1)
	var group sync.WaitGroup
	group.Go(func() { done <- database.AddTeamMember(requestCtx, team, manager, manager) })
	defer func() { rollbackTransaction(ctx, tx); cancelRequest(); group.Wait() }()
	waitGrantWaiters(t, ctx, database, pid, 1)
	cancelRequest()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("팀 잠금 대기 취소=%v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if before != teamStateFingerprint(t, database, manager) {
		t.Fatal("취소된 팀 변경·감사가 남았다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func teamMutation(ctx context.Context, database *Store, team, actor, member model.ID, action string) error {
	switch action {
	case "create":
		_, err := database.CreateTeam(ctx, actor, "원자성 팀")
		return err
	case "add":
		return database.AddTeamMember(ctx, team, actor, member)
	case "remove":
		return database.RemoveTeamMemberWithAudit(ctx, team, actor, member)
	default:
		return database.SetTeamDeleted(ctx, team, actor, action == "delete")
	}
}

func assertTeamGrace(t *testing.T, database *Store, graphs []model.ID, want bool) {
	t.Helper()
	for _, graph := range graphs {
		started, _ := graceTimes(t, database, graph)
		if (started != nil) != want {
			t.Fatalf("팀 변경 뒤 유예=%v, 기대=%t", started, want)
		}
	}
}

func teamStateFingerprint(t *testing.T, database *Store, manager model.ID) string {
	t.Helper()
	var parts []string
	for table, where := range map[string]string{
		"team":          "manager_account_id=$1",
		"team_member":   "team_id IN (SELECT team_id FROM owned)",
		"graph_grant":   "subject_type='team' AND subject_id IN (SELECT team_id FROM owned)",
		"context_graph": "graph_id IN (SELECT graph_id FROM public.graph_grant WHERE subject_type='team' AND subject_id IN (SELECT team_id FROM owned))",
		"web_audit_log": "actor_account_id=$1 OR team_id IN (SELECT team_id FROM owned)",
	} {
		parts = append(parts, `(SELECT COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY row_to_json(t)::text),'') FROM public.`+table+` t WHERE `+where+`)`)
	}
	slices.Sort(parts)
	var result string
	if err := database.pool.QueryRow(t.Context(), `WITH owned AS (SELECT team_id FROM public.team WHERE manager_account_id=$1) SELECT md5(`+strings.Join(parts, " || ")+`)`, manager.String()).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
