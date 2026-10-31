package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func obs06ID(t *testing.T, n int) model.ID {
	t.Helper()
	id, err := model.ParseID(fmt.Sprintf("019a0000-abcd-7000-8000-%012x", n))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func obs06Operation(t *testing.T, database *Store, graph, actor, target, id model.ID, at time.Time, result string) {
	t.Helper()
	_, err := database.pool.Exec(t.Context(), `INSERT INTO public.operation_log
		(operation_id,graph_id,operation_kind,context_id,target_version,judgment_input,actor_account_id,actor_agent_id,applied_at,result,reject_reason)
		VALUES($1,$2,'keep',$3,1,'synthetic audit',$4,$4,$5,$6,CASE WHEN $6='rejected' THEN 'synthetic rejection' END)`,
		id.String(), graph.String(), target.String(), actor.String(), at, result)
	if err != nil {
		t.Fatal(err)
	}
}

func obs06Web(t *testing.T, database *Store, graph, actor, target, id model.ID, at time.Time) {
	t.Helper()
	_, err := database.pool.Exec(t.Context(), `INSERT INTO public.web_audit_log
		(audit_id,graph_id,target_kind,action,actor_account_id,occurred_at,target_context_id)
		VALUES($1,$2,'web_delete','restore',$3,$4,$5)`, id.String(), graph.String(), actor.String(), at, target.String())
	if err != nil {
		t.Fatal(err)
	}
}

func TestOBS06AuditPagesIntegration(t *testing.T) {
	database, _, _ := newAgentContractDatabase(t)
	graph, actor, node := boundaryGraph(t, database)
	at := time.Now().UTC().Truncate(time.Second)
	low, high := obs06ID(t, 1), obs06ID(t, 2)
	obs06Operation(t, database, graph, actor, node.ID, low, at, "applied")
	obs06Operation(t, database, graph, actor, node.ID, high, at, "applied")
	// 두 테이블의 ID까지 같아도 별개의 행으로 페이지를 이어 읽는다.
	obs06Web(t, database, graph, actor, node.ID, high, at)
	obs06Web(t, database, graph, actor, node.ID, low, at.Add(-time.Second))
	other, otherActor, otherNode := boundaryGraph(t, database)
	obs06Web(t, database, other, otherActor, otherNode.ID, obs06ID(t, 3), at.Add(time.Second))
	want := []string{"web:" + high.String(), "operation:" + high.String(), "operation:" + low.String(), "web:" + low.String()}
	for _, size := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			var got []string
			cursor := ""
			for page := 0; ; page++ {
				if page > 4 {
					t.Fatal("커서가 끝나지 않는다")
				}
				entries, next, err := database.ListAuditEntries(t.Context(), graph, actor, model.AuditFilter{}, cursor, size)
				if err != nil || len(entries) > size {
					t.Fatalf("페이지=%+v/%v", entries, err)
				}
				for _, e := range entries {
					if e.ActorID != actor {
						t.Fatal("다른 그래프 기록을 반환했다")
					}
					got = append(got, e.Source+":"+e.ID.String())
				}
				if next == "" {
					break
				}
				if next == cursor {
					t.Fatal("같은 커서를 반환했다")
				}
				cursor = next
			}
			if !slices.Equal(got, want) {
				t.Fatalf("안정된 전체 순서=%v, 기대=%v", got, want)
			}
		})
	}
	until := at.Add(time.Second)
	entries, next, err := database.ListAuditEntries(t.Context(), graph, actor, model.AuditFilter{From: &at, Until: &until, Target: node.ID}, "", 4)
	if err != nil || len(entries) != 3 || next != "" {
		t.Fatalf("기간·대상 필터=%+v/%s/%v", entries, next, err)
	}
	entries, _, err = database.ListAuditEntries(t.Context(), graph, actor, model.AuditFilter{Until: &at}, "", 4)
	if err != nil || len(entries) != 1 || entries[0].Source != "web" {
		t.Fatalf("끝 시각 제외=%+v/%v", entries, err)
	}
	entries, next, err = database.ListAuditEntries(t.Context(), graph, actor, model.AuditFilter{Target: newTestID(t)}, "", 4)
	if err != nil || len(entries) != 0 || next != "" {
		t.Fatalf("없는 대상=%+v/%s/%v", entries, next, err)
	}
}

func TestOBS06AuditOwnerAndCancellationIntegration(t *testing.T) {
	database, _, _ := newAgentContractDatabase(t)
	graph, owner, _ := boundaryGraph(t, database)
	viewer, member := newTestID(t), newTestID(t)
	createTestAccount(t, database, viewer)
	createTestAccount(t, database, member)
	grantAccount(t, database, graph, viewer, model.GraphGradeViewer)
	team, err := database.CreateTeam(t.Context(), owner, "OBS-06 owners")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), team.ID, owner, member); err != nil {
		t.Fatal(err)
	}
	if err := database.GrantGraph(t.Context(), graph, owner, team.ID, GrantSubjectTeam, model.GraphGradeOwner); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []model.ID{viewer, newTestID(t)} {
		entries, next, err := database.ListAuditEntries(t.Context(), graph, actor, model.AuditFilter{}, "", 2)
		if !errors.Is(err, ErrNotFound) || entries != nil || next != "" {
			t.Fatalf("소유자 밖 감사=%+v/%s/%v", entries, next, err)
		}
	}
	if _, _, err := database.ListAuditEntries(t.Context(), graph, member, model.AuditFilter{}, "", 2); err != nil {
		t.Fatal(err)
	}
	if err := database.SetTeamDeleted(t.Context(), team.ID, owner, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.ListAuditEntries(t.Context(), graph, member, model.AuditFilter{}, "", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("삭제 팀 상속=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	entries, next, err := database.ListAuditEntries(ctx, graph, owner, model.AuditFilter{}, "", 2)
	if !errors.Is(err, context.Canceled) || entries != nil || next != "" {
		t.Fatalf("취소 결과=%+v/%s/%v", entries, next, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

type obs06AuditTracer struct {
	changed atomic.Bool
	change  func()
}

type obs06GradeKey struct{}

func (*obs06AuditTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, obs06GradeKey{}, strings.Contains(data.SQL, "WITH candidate AS"))
}

func (tracer *obs06AuditTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if yes, _ := ctx.Value(obs06GradeKey{}).(bool); yes && tracer.changed.CompareAndSwap(false, true) {
		tracer.change()
	}
}

func TestOBS06AuditSnapshotIntegration(t *testing.T) {
	database, writer, _ := newAgentContractDatabase(t)
	graph, actor, node := boundaryGraph(t, database)
	at := time.Now().UTC()
	obs06Operation(t, database, graph, actor, node.ID, obs06ID(t, 1), at, "applied")
	obs06Web(t, database, graph, actor, node.ID, obs06ID(t, 2), at)
	tracer := &obs06AuditTracer{change: func() {
		// 소유자 확인이 스냅숏을 만든 직후 다른 연결이 등급과 웹 기록을 제거한다.
		if _, err := writer.pool.Exec(t.Context(), `DELETE FROM public.web_audit_log WHERE graph_id=$1`, graph.String()); err != nil {
			t.Error(err)
		}
		if _, err := writer.pool.Exec(t.Context(), `DELETE FROM public.graph_grant WHERE graph_id=$1`, graph.String()); err != nil {
			t.Error(err)
		}
	}}
	cfg := database.pool.Config()
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	database.pool.Close()
	database.pool = pool
	entries, _, err := database.ListAuditEntries(t.Context(), graph, actor, model.AuditFilter{}, "", 2)
	if err != nil || len(entries) != 2 || !tracer.changed.Load() {
		t.Fatalf("스냅숏 감사=%+v/%v", entries, err)
	}
	if _, _, err := database.ListAuditEntries(t.Context(), graph, actor, model.AuditFilter{}, "", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("다음 조회의 현재 권한=%v", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestOBS06AuditInvalidInputs(t *testing.T) {
	database := &Store{}
	id := newTestID(t)
	at := time.Now().UTC()
	local := at.In(time.FixedZone("offset", 3600))
	for _, filter := range []model.AuditFilter{{From: new(time.Time{})}, {From: &local}, {From: &at, Until: &at}, {Target: model.ID{1}}} {
		if _, _, err := database.ListAuditEntries(t.Context(), id, id, filter, "", 1); err == nil {
			t.Fatal("잘못된 필터를 수락했다")
		}
	}
	for _, raw := range []string{"!", strings.Repeat("a", 513), "2026-10-22T00:00:00Z|" + id.String() + "|invalid", "2026-10-22T00:00:00+09:00|" + id.String() + "|web"} {
		if strings.Contains(raw, "|") {
			raw = base64.RawURLEncoding.EncodeToString([]byte(raw))
		}
		if _, _, err := database.ListAuditEntries(t.Context(), id, id, model.AuditFilter{}, raw, 1); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("잘못된 커서=%v", err)
		}
	}
	for _, size := range []int{0, -1, 1 << 31} {
		if _, _, err := database.ListAuditEntries(t.Context(), id, id, model.AuditFilter{}, "", size); err == nil {
			t.Fatal("잘못된 페이지 크기를 수락했다")
		}
	}
}

func TestOBS06InheritedRestoreRetentionIntegration(t *testing.T) {
	for _, path := range []string{"grant", "member", "restore"} {
		t.Run(path, func(t *testing.T) {
			database, _, _ := newAgentContractDatabase(t)
			graph, creator, _ := boundaryGraph(t, database)
			member := newTestID(t)
			createTestAccount(t, database, member)
			team, err := database.CreateTeam(t.Context(), creator, "OBS-06 inherited")
			if err != nil {
				t.Fatal(err)
			}
			if path == "restore" {
				if err := database.SetTeamDeleted(t.Context(), team.ID, creator, true); err != nil {
					t.Fatal(err)
				}
				// 삭제 중에는 유효한 소유자가 아니다. 팀 복구 때 상속 근거를 만든다.
				addTestTeamMember(t, database, team.ID, member)
				grantTeam(t, database, graph, team.ID, model.GraphGradeOwner)
				if err := database.SetTeamDeleted(t.Context(), team.ID, creator, false); err != nil {
					t.Fatal(err)
				}
			} else {
				if path == "grant" {
					if err := database.AddTeamMember(t.Context(), team.ID, creator, member); err != nil {
						t.Fatal(err)
					}
				}
				if err := database.GrantGraph(t.Context(), graph, creator, team.ID, GrantSubjectTeam, model.GraphGradeOwner); err != nil {
					t.Fatal(err)
				}
				if path == "member" {
					if err := database.AddTeamMember(t.Context(), team.ID, creator, member); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := database.RevokeGraphGrantWithAudit(t.Context(), graph, creator, creator, GrantSubjectAccount); err != nil {
				t.Fatal(err)
			}
			if err := database.RemoveTeamMemberWithAudit(t.Context(), team.ID, creator, member); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_expires_at=clock_timestamp() WHERE graph_id=$1 AND grace_started_at IS NOT NULL`, graph.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := database.ExpireGrace(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := database.RequestGraphRestore(t.Context(), graph, newTestID(t)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("미확인 소유자=%v", err)
			}
			if err := database.RequestGraphRestore(t.Context(), graph, member); err != nil {
				t.Fatal(err)
			}
			future := now.AddDate(1, 0, 0)
			days := func(model.ID) int { return 1 }
			if _, err := database.CleanupAuditRecords(t.Context(), future, days, days); err != nil {
				t.Fatal(err)
			}
			eligible, err := database.ListRestoreEligibleGraphs(t.Context(), member)
			if err != nil || !containsGraph(eligible, graph) {
				t.Fatalf("상속 근거 유실=%+v/%v", eligible, err)
			}
			pending, err := database.PendingRestoreRequests(t.Context())
			if err != nil || len(pending) != 1 || pending[0].RequestedBy != member {
				t.Fatalf("미처리 요청=%+v/%v", pending, err)
			}
			if err := database.OperatorRestoreGraph(t.Context(), graph, creator); err != nil {
				t.Fatal(err)
			}
			// 다시 자동 삭제되어도 같은 시각의 과거 요청·처리는 완료로 남아야 한다.
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.web_audit_log SET occurred_at=$2 WHERE graph_id=$1 AND target_kind IN ('restore_request','operator_restore')`, graph.String(), now); err != nil {
				t.Fatal(err)
			}
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at=$2,grace_started_at=$2 WHERE graph_id=$1`, graph.String(), now); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				pending, err := database.PendingRestoreRequests(t.Context())
				if err != nil || len(pending) != 0 {
					t.Fatalf("처리 완료 요청이 다시 나타났다=%+v/%v", pending, err)
				}
				if _, err := database.CleanupAuditRecords(t.Context(), future, days, days); err != nil {
					t.Fatal(err)
				}
			}
			var restored int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log WHERE graph_id=$1 AND target_kind='operator_restore' AND after_grade='owner'`, graph.String()).Scan(&restored); err != nil || restored != 1 {
				t.Fatalf("운영 복구 근거=%d/%v", restored, err)
			}
		})
	}
}

func TestOBS06AuditRetentionPoliciesIntegration(t *testing.T) {
	database, _, _ := newAgentContractDatabase(t)
	graph, actor, node := boundaryGraph(t, database)
	now := time.Now().UTC()
	old, recent := now.AddDate(0, 0, -60), now.AddDate(0, 0, -2)
	obs06Operation(t, database, graph, actor, node.ID, obs06ID(t, 1), old, "applied")
	obs06Operation(t, database, graph, actor, node.ID, obs06ID(t, 2), recent, "applied")
	obs06Operation(t, database, graph, actor, node.ID, obs06ID(t, 3), recent, "rejected")
	obs06Web(t, database, graph, actor, node.ID, obs06ID(t, 4), old)
	obs06Web(t, database, graph, actor, node.ID, obs06ID(t, 5), recent)
	zero := func(model.ID) int { return 0 }
	if n, err := database.CleanupAuditRecords(t.Context(), now, zero, zero); err != nil || n != 0 {
		t.Fatalf("무기한 보존=%d/%v", n, err)
	}
	if n, err := database.CleanupAuditRecords(t.Context(), now, func(model.ID) int { return 30 }, func(model.ID) int { return 1 }); err != nil || n != 3 {
		t.Fatalf("적용·거부 보존 분리=%d/%v", n, err)
	}
	entries, _, err := database.ListAuditEntries(t.Context(), graph, actor, model.AuditFilter{}, "", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("정리 후 기록=%+v/%v", entries, err)
	}
	operation := registrationOperation(node, OperationDiscard)
	if _, err := database.DiscardContext(t.Context(), graph, node.ID, operation, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CleanupAuditRecords(t.Context(), now.AddDate(1, 0, 0), func(model.ID) int { return 1 }, zero); err != nil {
		t.Fatal(err)
	}
	if found, err := database.HasAppliedDiscard(t.Context(), graph, node.ID); err != nil || !found {
		t.Fatalf("현재 폐기 근거=%t/%v", found, err)
	}
	operation.Kind = OperationUpdate
	if _, err := database.RestoreContext(t.Context(), graph, node.ID, operation, WriteLimits{}); err != nil {
		t.Fatalf("정리 후 실제 복구=%v", err)
	}
}
