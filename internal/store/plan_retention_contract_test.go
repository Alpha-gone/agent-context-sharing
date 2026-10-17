package store

import (
	"fmt"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

func TestPlanRetentionPoliciesIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	finite, unlimited, teamActor := newTestID(t), newTestID(t), newTestID(t)
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"retention_days":365,"audit_retention_days":365},"%s":{"rejected_operation_days":30},"%s":{"retention_days":30,"audit_retention_days":30}}`, finite, unlimited, teamActor))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2028, 10, 31, 12, 34, 56, 0, time.UTC)
	type expectedRecord struct {
		graph    model.ID
		judgment string
		present  bool
	}
	var records []expectedRecord
	var nodes []model.Context
	var graphs []model.ID
	for _, owner := range []model.ID{finite, unlimited} {
		createTestAccount(t, database, owner)
		graph := createTestGraph(t, database, owner)
		graphs = append(graphs, graph)
		limits := plans.For(owner)
		for index, after := range []time.Duration{0, time.Microsecond} {
			source := testSourceContext(t, graph, owner, fmt.Sprintf("api://perm06/retention/%s/%d", graph, index))
			operation := registrationOperation(source, OperationAdd)
			operation.JudgmentInput = fmt.Sprintf("applied-%d", index)
			saved, err := database.CreateContextWithOperation(t.Context(), graph, source, nil, operation, WriteLimits{})
			if err != nil {
				t.Fatal(err)
			}
			nodes = append(nodes, saved)
			cutoff := at.AddDate(0, 0, -365).Add(after)
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.operation_log SET applied_at=$3 WHERE graph_id=$1 AND judgment_input=$2`, graph.String(), operation.JudgmentInput, cutoff); err != nil {
				t.Fatal(err)
			}
			records = append(records, expectedRecord{graph, operation.JudgmentInput, owner == unlimited || after > 0})
			operation.Kind = OperationUpdate
			operation.JudgmentInput = fmt.Sprintf("rejected-%d", index)
			if err := database.RecordRejectedOperation(t.Context(), *operation, "synthetic version conflict"); err != nil {
				t.Fatal(err)
			}
			cutoff = at.AddDate(0, 0, -limits.RejectedOperationRetention.DaysAt(at)).Add(after)
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.operation_log SET applied_at=$3 WHERE graph_id=$1 AND judgment_input=$2`, graph.String(), operation.JudgmentInput, cutoff); err != nil {
				t.Fatal(err)
			}
			records = append(records, expectedRecord{graph, operation.JudgmentInput, after > 0})
		}
	}
	derived := testDerivedContext(t, graphs[0], finite)
	derived.Derived.ValidTo = new(at.AddDate(-3, 0, 0))
	derived, err = database.CreateContext(t.Context(), graphs[0], derived, []model.ID{nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	nodes = append(nodes, derived)
	// 현재 폐기의 복구 근거는 유한 감사 기간보다 오래되어도 유지한다.
	deleted, err := database.DiscardContext(t.Context(), graphs[0], nodes[0].ID, testMCPDiscardOperation(nodes[0]), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	// 근거 폐기가 유효성 표시의 판을 올리므로 정리 전 상태를 기준으로 삼는다.
	nodes[len(nodes)-1], err = database.Context(t.Context(), derived.GraphID, derived.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.operation_log SET applied_at=$2 WHERE graph_id=$1 AND operation_kind='discard'`, graphs[0].String(), at.AddDate(0, 0, -366)); err != nil {
		t.Fatal(err)
	}
	// 팀 감사는 그래프 생성 계정이 아닌 행위 계정의 보존 조건을 따른다.
	createTestAccount(t, database, teamActor)
	oldAudit, newAudit, protectedAudit := newTestID(t), newTestID(t), newTestID(t)
	if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.web_audit_log (audit_id,target_kind,action,actor_account_id,occurred_at,team_id) VALUES ($1,'team','add',$2,$3,$4),($5,'team','add',$2,$6,$4),($7,'team','add',$8,$3,$4)`, oldAudit.String(), teamActor.String(), at.AddDate(0, 0, -30), newTestID(t).String(), newAudit.String(), at.AddDate(0, 0, -30).Add(time.Microsecond), protectedAudit.String(), unlimited.String()); err != nil {
		t.Fatal(err)
	}
	policyFor := func(id model.ID) plan.Limits {
		if id == finite || id == unlimited || id == teamActor {
			return plans.For(id)
		}
		return plan.Limits{} // 다른 시험의 기록에는 정리를 적용하지 않는다.
	}
	count, err := database.CleanupAuditRecords(t.Context(), at, func(id model.ID) int { return policyFor(id).AuditRetentionDays }, func(id model.ID) int { return policyFor(id).RejectedOperationRetention.DaysAt(at) })
	if err != nil || count != 4 {
		t.Fatalf("서로 다른 보존 정리=%d/%v, want 4", count, err)
	}
	for _, record := range records {
		found, err := database.HasOperationJudgment(t.Context(), record.graph, record.judgment)
		if err != nil || found != record.present {
			t.Fatalf("보존 %s/%s=%t/%v, want %t", record.graph, record.judgment, found, err, record.present)
		}
	}
	for index, node := range nodes {
		stored, err := database.Context(t.Context(), node.GraphID, node.ID)
		if index == 0 {
			node = deleted
		}
		if err != nil || stored.Body != node.Body || stored.Version != node.Version {
			t.Fatalf("기록 정리가 본문을 바꿨다: %v", err)
		}
	}
	if found, err := database.HasAppliedDiscard(t.Context(), graphs[0], deleted.ID); err != nil || !found {
		t.Fatalf("현재 폐기의 복구 근거=%t/%v", found, err)
	}
	for _, audit := range []struct {
		id      model.ID
		present bool
	}{{oldAudit, false}, {newAudit, true}, {protectedAudit, true}} {
		var found bool
		if err := database.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM public.web_audit_log WHERE audit_id=$1)`, audit.id.String()).Scan(&found); err != nil || found != audit.present {
			t.Fatalf("팀 감사 보존=%t/%v, want %t", found, err, audit.present)
		}
	}
	assertConnectionRecovery(t, database, nil, 0)
}
