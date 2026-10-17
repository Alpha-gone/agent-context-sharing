package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func TestLifeDeletionRechecksOwnerIntegration(t *testing.T) {
	for _, action := range []string{"graph_delete", "graph_restore", "node_delete", "node_restore"} {
		for _, change := range []string{"demote", "remove_member", "delete_team"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				database := newSmallPoolStore(t)
				owner, actor := newTestID(t), newTestID(t)
				createTestAccount(t, database, owner)
				createTestAccount(t, database, actor)
				graph := createTestGraph(t, database, owner)
				grantAccount(t, database, graph, owner, model.GraphGradeOwner)
				source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, owner, "api://life06/owner/"+graph.String()), nil)
				if err != nil {
					t.Fatal(err)
				}
				if action == "graph_restore" {
					if err := database.SetGraphDeleted(t.Context(), graph, owner, true); err != nil {
						t.Fatal(err)
					}
				}
				if action == "node_restore" {
					if _, err := database.SetContextDeleted(t.Context(), graph, source.ID, owner, true, 0); err != nil {
						t.Fatal(err)
					}
				}
				team := newTestID(t)
				if change == "demote" {
					grantAccount(t, database, graph, actor, model.GraphGradeOwner)
				} else {
					createTestTeam(t, database, team, actor, false)
					addTestTeamMember(t, database, team, actor)
					grantTeam(t, database, graph, team, model.GraphGradeOwner)
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
				if grade, ok, err := database.EffectiveGrade(ctx, graph, actor); err != nil || !ok || grade != model.GraphGradeOwner {
					t.Fatalf("선행 소유자=%s/%t/%v", grade, ok, err)
				}
				before := planStateFingerprint(t, database, graph)
				done := make(chan error, 1)
				var group sync.WaitGroup
				group.Go(func() { done <- lifeDeletion(ctx, database, graph, source.ID, actor, action) })
				defer func() { rollbackTransaction(ctx, tx); cancel(); group.Wait() }()
				waitLifeBlock(t, ctx, database, pid)
				switch change {
				case "demote":
					_, err = tx.Exec(ctx, `UPDATE public.graph_grant SET grade='viewer' WHERE graph_id=$1 AND subject_id=$2`, graph.String(), actor.String())
				case "remove_member":
					_, err = tx.Exec(ctx, `DELETE FROM public.team_member WHERE team_id=$1 AND account_id=$2`, team.String(), actor.String())
				case "delete_team":
					_, err = tx.Exec(ctx, `UPDATE public.team SET deleted_at=clock_timestamp() WHERE team_id=$1`, team.String())
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-done; !errors.Is(err, ErrNotFound) {
					t.Fatalf("권한 변경 뒤 삭제=%v", err)
				}
				if before != planStateFingerprint(t, database, graph) {
					t.Fatal("권한 회수 뒤 상태·누계·색인·감사가 바뀌었다")
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func lifeDeletion(ctx context.Context, database *Store, graph, node, actor model.ID, action string) error {
	if strings.HasPrefix(action, "graph_") {
		return database.SetGraphDeleted(ctx, graph, actor, action == "graph_delete")
	}
	_, err := database.SetContextDeleted(ctx, graph, node, actor, action == "node_delete", 0)
	return err
}

func waitLifeBlock(t *testing.T, ctx context.Context, database *Store, pid int) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := database.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestLifeDeletionImpactAndPreservationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, owner, source := boundaryGraph(t, database)
	other, excluded := newTestID(t), newTestID(t)
	for _, id := range []model.ID{other, excluded} {
		createTestAccount(t, database, id)
	}
	grantAccount(t, database, graph, other, model.GraphGradeViewer)
	for _, deleted := range []bool{false, true} {
		team := newTestID(t)
		createTestTeam(t, database, team, owner, deleted)
		if deleted {
			addTestTeamMember(t, database, team, excluded)
		} else {
			addTestTeamMember(t, database, team, owner)
			addTestTeamMember(t, database, team, other)
		}
		grantTeam(t, database, graph, team, model.GraphGradeOwner)
	}
	derived, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, owner), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		event := testEventContext(t, graph, owner, source.ID)
		if _, err := database.CreateContext(t.Context(), graph, event, nil); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, owner), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetContextDeleted(t.Context(), graph, deleted.ID, owner, true, 0); err != nil {
		t.Fatal(err)
	}
	impact, err := database.DeletionImpact(t.Context(), graph, model.ID{})
	if err != nil || impact.Accounts != 2 || impact.Contexts != 4 {
		t.Fatalf("그래프 고유 영향=%+v/%v", impact, err)
	}
	impact, err = database.DeletionImpact(t.Context(), graph, source.ID)
	if err != nil || impact.Derived != 1 || impact.Events != 2 {
		t.Fatalf("원천 활성 영향=%+v/%v", impact, err)
	}
	if _, err := database.SetContextDeleted(t.Context(), graph, source.ID, owner, true, 0); err != nil {
		t.Fatal(err)
	}
	node, err := database.Context(t.Context(), graph, derived.ID)
	if err != nil || node.DeletedAt != nil || !node.Derived.EvidenceInvalidated {
		t.Fatalf("연결 파생 보존=%+v/%v", node, err)
	}
	var before, after string
	query := `SELECT md5(string_agg(row_to_json(n)::text,'' ORDER BY id)) FROM ` + database.contextTable() + ` n WHERE properties->>'graph_id'::text=$1`
	if err := database.pool.QueryRow(t.Context(), query, graph.String()).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := database.SetGraphDeleted(t.Context(), graph, owner, true); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetContextDeleted(t.Context(), graph, source.ID, owner, false, 0); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("삭제 그래프의 노드 복구=%v", err)
	}
	if err := database.pool.QueryRow(t.Context(), query, graph.String()).Scan(&after); err != nil || before != after {
		t.Fatalf("그래프 삭제가 정점을 바꿨다: %v", err)
	}
	if err := database.SetGraphDeleted(t.Context(), graph, owner, false); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetContextDeleted(t.Context(), graph, source.ID, owner, false, 0); err != nil {
		t.Fatal(err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeTeamLockIncludesNewGrantIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	database.graceDays = func(model.ID) int { return 30 }
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	team := newTestID(t)
	createTestTeam(t, database, team, actor, false)
	addTestTeamMember(t, database, team, actor)
	lower, higher := createTestGraph(t, database, actor), createTestGraph(t, database, actor)
	grantTeam(t, database, higher, team, model.GraphGradeOwner)
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
	group.Go(func() { done <- database.RemoveTeamMemberWithAudit(ctx, team, actor, actor) })
	defer func() { rollbackTransaction(ctx, tx); cancel(); group.Wait() }()
	waitLifeBlock(t, ctx, database, pid)
	if _, err := lockGraphs(ctx, tx, []model.ID{lower}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.graph_grant VALUES($1,'team',$2,'owner')`, lower.String(), team.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, graph := range []model.ID{lower, higher} {
		if start, end := graceTimes(t, database, graph); start == nil || end == nil {
			t.Fatalf("새 부여를 포함한 유예 누락=%s", graph)
		}
	}
	if err := database.AddTeamMember(ctx, team, actor, actor); err != nil {
		t.Fatal(err)
	}
	for _, graph := range []model.ID{lower, higher} {
		if start, end := graceTimes(t, database, graph); start != nil || end != nil {
			t.Fatal("실제 재연결 뒤 유예가 남았다")
		}
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeGraceExpiryReconnectionIntegration(t *testing.T) {
	for _, reconnectFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(reconnectFirst), func(t *testing.T) {
			database := newSmallPoolStore(t)
			database.graceDays = func(model.ID) int { return 1 }
			actor := newTestID(t)
			createTestAccount(t, database, actor)
			graph := createTestGraph(t, database, actor)
			team := newTestID(t)
			createTestTeam(t, database, team, actor, false)
			addTestTeamMember(t, database, team, actor)
			grantTeam(t, database, graph, team, model.GraphGradeOwner)
			if err := database.RemoveTeamMemberWithAudit(t.Context(), team, actor, actor); err != nil {
				t.Fatal(err)
			}
			_, expires := graceTimes(t, database, graph)
			if expires == nil {
				t.Fatal("유예 만료가 없다")
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
			done := make(chan error, 1)
			var group sync.WaitGroup
			group.Go(func() {
				if reconnectFirst {
					_, err := database.ExpireGrace(ctx, *expires)
					done <- err
				} else {
					done <- database.AddTeamMember(ctx, team, actor, actor)
				}
			})
			defer func() { rollbackTransaction(ctx, tx); cancel(); group.Wait() }()
			waitLifeBlock(t, ctx, database, pid)
			if reconnectFirst {
				if _, err := tx.Exec(ctx, `INSERT INTO public.team_member VALUES($1,$2)`, team.String(), actor.String()); err != nil {
					t.Fatal(err)
				}
				if err := cancelGraceForConnectedGraphs(ctx, tx, []model.ID{graph}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := tx.Exec(ctx, `UPDATE public.context_graph SET deleted_at=$2 WHERE graph_id=$1 AND grace_expires_at<=$2`, graph.String(), *expires); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			stored, err := database.Graph(ctx, graph)
			if err != nil {
				t.Fatal(err)
			}
			if reconnectFirst {
				if stored.DeletedAt != nil || stored.GraceStartedAt != nil {
					t.Fatal("재연결 뒤 이전 만료 시각으로 삭제됐다")
				}
			} else {
				if stored.DeletedAt == nil || stored.GraceStartedAt == nil {
					t.Fatal("만료 뒤 재연결이 자동 삭제를 되살렸다")
				}
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestLifeGraceInvalidPeriodRollsBackIntegration(t *testing.T) {
	for _, days := range []int{-1, int(math.MaxInt32) + 1} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			database := newSmallPoolStore(t)
			database.graceDays = func(model.ID) int { return days }
			actor := newTestID(t)
			createTestAccount(t, database, actor)
			graph := createTestGraph(t, database, actor)
			team := newTestID(t)
			createTestTeam(t, database, team, actor, false)
			addTestTeamMember(t, database, team, actor)
			grantTeam(t, database, graph, team, model.GraphGradeOwner)
			before := planStateFingerprint(t, database, graph)
			err := database.RemoveTeamMemberWithAudit(t.Context(), team, actor, actor)
			if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != "grace_days" {
				t.Fatalf("표현할 수 없는 유예=%v", err)
			}
			var exists bool
			if err := database.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM public.team_member WHERE team_id=$1 AND account_id=$2)`, team.String(), actor.String()).Scan(&exists); err != nil || !exists || before != planStateFingerprint(t, database, graph) {
				t.Fatalf("유예 실패 롤백=%t/%v", exists, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestLifeDeletionAuditRollbackIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, node := boundaryGraph(t, database)
	name := "life06_audit_" + strings.ReplaceAll(graph.String(), "-", "")
	quoted := pgx.Identifier{"public", name}.Sanitize()
	trigger := pgx.Identifier{name}.Sanitize()
	if _, err := database.pool.Exec(t.Context(), `CREATE FUNCTION `+quoted+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.graph_id='`+graph.String()+`'::uuid THEN RAISE EXCEPTION 'synthetic audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER `+trigger+` BEFORE INSERT ON public.web_audit_log FOR EACH ROW EXECUTE FUNCTION `+quoted+`() `); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if _, err := database.pool.Exec(ctx, `DROP TRIGGER `+trigger+` ON public.web_audit_log; DROP FUNCTION `+quoted+`() `); err != nil {
			t.Error(err)
		}
	})
	before := planStateFingerprint(t, database, graph)
	for _, action := range []string{"graph_delete", "node_delete"} {
		if err := lifeDeletion(t.Context(), database, graph, node.ID, actor, action); err == nil {
			t.Fatal("감사 실패가 성공했다")
		}
		if before != planStateFingerprint(t, database, graph) {
			t.Fatal("감사 실패 뒤 상태가 남았다")
		}
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeWebNodePreservesMCPLockOrderIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	node, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(ctx, tx)
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM `+database.contextTable()+` WHERE properties->>'context_id'::text=$1 FOR UPDATE`, node.ID.String()).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var group sync.WaitGroup
	group.Go(func() { _, err := database.SetContextDeleted(ctx, graph, node.ID, actor, true, 0); done <- err })
	defer func() { rollbackTransaction(ctx, tx); cancel(); group.Wait() }()
	waitLifeBlock(t, ctx, database, pid)
	next := node
	next.Version++
	next.Body += "동시 수정"
	if _, err := database.UpdateContextWithOperation(context.WithValue(ctx, writeTransactionContextKey{}, tx), graph, node.Version, next, nil, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stored, err := database.Context(ctx, graph, node.ID)
	if err != nil || stored.Body != next.Body || stored.Version != next.Version+1 || stored.DeletedAt == nil {
		t.Fatalf("잠금 대기 뒤 현재 본문·판=%+v/%v", stored, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}
