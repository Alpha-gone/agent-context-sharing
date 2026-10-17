package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"github.com/jackc/pgx/v5"
)

type countedGradeQueryer struct {
	gradeQueryer
	calls int
}

func (queryer *countedGradeQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	queryer.calls++
	return queryer.gradeQueryer.QueryRow(ctx, sql, args...)
}

func TestEffectiveGradeContractIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	account, other := newTestID(t), newTestID(t)
	createTestAccount(t, database, account)
	createTestAccount(t, database, other)
	teams := []model.ID{newTestID(t), newTestID(t), newTestID(t), newTestID(t), newTestID(t)}
	for index, team := range teams {
		createTestTeam(t, database, team, other, index == 2)
		if index < 3 {
			addTestTeamMember(t, database, team, account)
		} else if index == 4 {
			addTestTeamMember(t, database, team, other)
		}
	}
	grades := []model.GraphGrade{"", model.GraphGradeViewer, model.GraphGradeEditor, model.GraphGradeOwner}
	wantListed := make(map[model.ID]model.GraphGrade)
	for directRank, direct := range grades {
		for teamRank, inherited := range grades {
			t.Run("direct="+string(direct)+"/team="+string(inherited), func(t *testing.T) {
				graph := createTestGraph(t, database, other)
				if direct != "" {
					grantAccount(t, database, graph, account, direct)
				}
				if inherited != "" {
					// 여러 활성 팀의 상속은 최고 등급을 쓰고 목록은 중복하지 않는다.
					grantTeam(t, database, graph, teams[0], inherited)
					grantTeam(t, database, graph, teams[1], model.GraphGradeViewer)
				}
				for _, team := range teams[2:] {
					// 삭제 팀·빈 팀·다른 계정만 속한 팀의 최고 등급은 적용하지 않는다.
					grantTeam(t, database, graph, team, model.GraphGradeOwner)
				}
				want := grades[max(directRank, teamRank)]
				queryer := &countedGradeQueryer{gradeQueryer: database.pool}
				grade, found, err := effectiveGrade(t.Context(), queryer, graph, account)
				if err != nil || grade != want || found != (want != "") || queryer.calls != 1 {
					t.Fatalf("등급=%q, found=%t, 질의=%d, 오류=%v; want %q, 질의 1개", grade, found, queryer.calls, err, want)
				}
				for requiredRank, required := range grades[1:] {
					err := perm.Require(t.Context(), database, graph, account, required)
					if want == "" {
						if _, ok := errors.AsType[perm.NotFoundError](err); !ok {
							t.Fatalf("무권한 계정의 %s 검사 = %v", required, err)
						}
					} else if max(directRank, teamRank) < requiredRank+1 {
						if denied, ok := errors.AsType[perm.DeniedError](err); !ok || denied.Required != required {
							t.Fatalf("등급 부족의 %s 검사 = %v", required, err)
						}
					} else if err != nil {
						t.Fatalf("허용할 %s 검사 = %v", required, err)
					}
				}
				if want != "" {
					wantListed[graph] = want
				}
			})
		}
	}
	listed, next, err := database.ListGraphs(t.Context(), account, model.GraphListFilter{}, "", 50)
	if err != nil || next != "" || len(listed) != len(wantListed) {
		t.Fatalf("권한 있는 목록 = %d개, 다음=%q, 오류=%v; want %d개", len(listed), next, err, len(wantListed))
	}
	for _, graph := range listed {
		if want, exists := wantListed[graph.ID]; !exists || graph.Grade != want {
			t.Fatalf("무권한·중복 그래프 또는 다른 등급이 목록에 노출됐다: %+v", graph)
		}
		delete(wantListed, graph.ID)
	}
	if len(wantListed) != 0 {
		t.Fatalf("권한 있는 그래프가 목록에서 누락됐다: %v", wantListed)
	}
	if err := perm.Require(t.Context(), database, newTestID(t), account, model.GraphGradeOwner); err == nil {
		t.Fatal("존재하지 않는 그래프가 허용됐다")
	} else if _, ok := errors.AsType[perm.NotFoundError](err); !ok {
		t.Fatalf("존재하지 않는 그래프의 오류 = %v", err)
	}
}

func TestEffectiveGradeCurrentStateIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	account := newTestID(t)
	createTestAccount(t, database, account)
	graph, team := createTestGraph(t, database, account), newTestID(t)
	createTestTeam(t, database, team, account, false)
	addTestTeamMember(t, database, team, account)
	grantAccount(t, database, graph, account, model.GraphGradeViewer)
	grantTeam(t, database, graph, team, model.GraphGradeOwner)
	assertGrade(t, database, graph, account, model.GraphGradeOwner)
	// 등급 판정 자체를 검증한다. 변경 API의 잠금·감사는 PERM-03/04에서 인수한다.
	for _, step := range []struct {
		name, sql string
		args      []any
		want      model.GraphGrade
	}{
		{"구성원 제거", `DELETE FROM public.team_member WHERE team_id=$1 AND account_id=$2`, []any{team.String(), account.String()}, model.GraphGradeViewer},
		{"구성원 재등록", `INSERT INTO public.team_member(team_id,account_id) VALUES($1,$2)`, []any{team.String(), account.String()}, model.GraphGradeOwner},
		{"팀 삭제", `UPDATE public.team SET deleted_at=clock_timestamp() WHERE team_id=$1`, []any{team.String()}, model.GraphGradeViewer},
		{"팀 복구", `UPDATE public.team SET deleted_at=NULL WHERE team_id=$1`, []any{team.String()}, model.GraphGradeOwner},
		{"팀 등급 하향", `UPDATE public.graph_grant SET grade='editor' WHERE graph_id=$1 AND subject_type='team' AND subject_id=$2`, []any{graph.String(), team.String()}, model.GraphGradeEditor},
		{"직접 등급 상향", `UPDATE public.graph_grant SET grade='owner' WHERE graph_id=$1 AND subject_type='account' AND subject_id=$2`, []any{graph.String(), account.String()}, model.GraphGradeOwner},
		{"직접 등급 회수", `DELETE FROM public.graph_grant WHERE graph_id=$1 AND subject_type='account' AND subject_id=$2`, []any{graph.String(), account.String()}, model.GraphGradeEditor},
		{"팀 등급 회수", `DELETE FROM public.graph_grant WHERE graph_id=$1 AND subject_type='team' AND subject_id=$2`, []any{graph.String(), team.String()}, ""},
	} {
		t.Run(step.name, func(t *testing.T) {
			result, err := database.pool.Exec(t.Context(), step.sql, step.args...)
			if err != nil || result.RowsAffected() != 1 {
				t.Fatalf("현재 등급 표본 변경 = %d행, %v", result.RowsAffected(), err)
			}
			grade, found, err := database.EffectiveGrade(t.Context(), graph, account)
			if err != nil || grade != step.want || found != (step.want != "") {
				t.Fatalf("현재 등급=%q, found=%t, 오류=%v; want %q", grade, found, err, step.want)
			}
			listed, _, err := database.ListGraphs(t.Context(), account, model.GraphListFilter{}, "", 10)
			if err != nil || (step.want == "" && len(listed) != 0) || (step.want != "" && (len(listed) != 1 || listed[0].ID != graph || listed[0].Grade != step.want)) {
				t.Fatalf("변경 직후 목록=%+v, 오류=%v", listed, err)
			}
		})
	}
}

func TestGraphCreationWithoutExistingGradeIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	account := newTestID(t)
	createTestAccount(t, database, account)
	listed, _, err := database.ListGraphs(t.Context(), account, model.GraphListFilter{}, "", 10)
	if err != nil || len(listed) != 0 {
		t.Fatalf("새 계정 목록=%+v, 오류=%v", listed, err)
	}
	now := time.Now().UTC()
	graph := model.Graph{ID: newTestID(t), Name: "PERM-02", CreatedBy: account, CreatedAt: now, LastActivityAt: now, Version: 1}
	if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
		t.Fatalf("기존 그래프 등급 없는 계정의 생성: %v", err)
	}
	assertGrade(t, database, graph.ID, account, model.GraphGradeOwner)
}

func TestEffectiveGradeInvalidIDs(t *testing.T) {
	id := newTestID(t)
	for _, ids := range [][2]model.ID{{{}, id}, {id, {}}} {
		grade, found, err := (&Store{}).EffectiveGrade(t.Context(), ids[0], ids[1])
		if err == nil || found || grade != "" {
			t.Fatalf("잘못된 식별자의 판정=%q, found=%t, 오류=%v", grade, found, err)
		}
	}
}

func TestEffectiveGradeCancellationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	account := newTestID(t)
	createTestAccount(t, database, account)
	graph := createTestGraph(t, database, account)
	grantAccount(t, database, graph, account, model.GraphGradeOwner)
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `LOCK TABLE public.graph_grant IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	grade, found, err := database.EffectiveGrade(ctx, graph, account)
	if !errors.Is(err, context.DeadlineExceeded) || grade != "" || found {
		t.Fatalf("SQL 대기 취소=%q, found=%t, 오류=%v", grade, found, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertConnectionRecovery(t, database, nil, 0)
	assertGrade(t, database, graph, account, model.GraphGradeOwner)
	assertConnectionRecovery(t, database, nil, 0)
}
