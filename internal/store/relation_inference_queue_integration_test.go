package store

import (
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func inferenceCreateOperation(value model.Context) *OperationRecord {
	return &OperationRecord{Kind: OperationAdd, GraphID: value.GraphID, ContextID: value.ID,
		JudgmentInput: "추론 등록 검증", AccountID: value.CreatedBy, AgentID: value.CreatedByAgent}
}

func TestRelationInferenceQueueRegistrationAndGenerationIntegration(t *testing.T) {
	db := newIsolatedIntegrationStore(t)
	graph, actor, member := lifeProposalFixture(t, db)
	off := testEventContext(t, graph, actor, member.ID)
	if _, err := db.CreateContextWithOperation(t.Context(), graph, off, nil, inferenceCreateOperation(off), WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.relation_inference_task`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("off 작업=%d: %v", count, err)
	}
	db.inferenceEnabled = true
	event := testEventContext(t, graph, actor, member.ID)
	stored, err := db.CreateContextWithOperation(t.Context(), graph, event, nil, inferenceCreateOperation(event), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	type claimResult struct {
		task  inferenceTask
		found bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	for range 2 {
		go func() {
			<-start
			task, found, err := db.claimInferenceTask(t.Context(), time.Minute)
			results <- claimResult{task, found, err}
		}()
	}
	close(start)
	var first inferenceTask
	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.found {
			first = result.task
			winners++
		}
	}
	if winners != 1 || first.ContextID != stored.ID || first.TriggerAccountID != actor || first.TargetVersion != stored.Version {
		t.Fatalf("동시 확보=%d, %+v", winners, first)
	}
	changed := stored
	changed.Body += " 변경"
	changed.Version++
	op := inferenceCreateOperation(changed)
	op.Kind, op.TargetVersion = OperationUpdate, changed.Version
	if _, err := db.UpdateContextWithOperation(t.Context(), graph, stored.Version, changed, op, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	if matched, err := db.failInferenceTask(t.Context(), first, "provider_unavailable", true, 0); err != nil || matched {
		t.Fatalf("이전 세대 실패=%v, %v", matched, err)
	}
	current, found, err := db.claimInferenceTask(t.Context(), time.Minute)
	if err != nil || !found || current.TargetVersion != changed.Version || !current.Generation.After(first.Generation) {
		t.Fatalf("새 세대=%+v, %v", current, err)
	}
	if matched, err := db.failInferenceTask(t.Context(), current, "output_invalid", false, 2); err != nil || !matched {
		t.Fatalf("출력 실패=%v, %v", matched, err)
	}
	var state string
	var outputAttempts int
	if err := db.pool.QueryRow(t.Context(), `SELECT state,output_attempts FROM public.relation_inference_task WHERE context_id=$1`, stored.ID.String()).Scan(&state, &outputAttempts); err != nil || state != "failed" || outputAttempts != 2 {
		t.Fatalf("최종 상태=%s 출력 위반=%d: %v", state, outputAttempts, err)
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE public.relation_inference_task SET state='pending',attempts=4,output_attempts=0,next_attempt_at=statement_timestamp() WHERE context_id=$1`, stored.ID.String()); err != nil {
		t.Fatal(err)
	}
	expired, found, err := db.claimInferenceTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatalf("리스 확보=%v, %v", found, err)
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE public.relation_inference_task SET next_attempt_at=statement_timestamp()-interval '1 second' WHERE task_id=$1`, expired.ID.String()); err != nil {
		t.Fatal(err)
	}
	reclaimed, found, err := db.claimInferenceTask(t.Context(), time.Minute)
	if err != nil || !found || !reclaimed.LeasedUntil.After(expired.LeasedUntil) {
		t.Fatalf("리스 재확보=%v, %v", found, err)
	}
	if matched, err := db.failInferenceTask(t.Context(), expired, "provider_unavailable", true, 0); err != nil || matched {
		t.Fatalf("만료 리스 실패=%v, %v", matched, err)
	}
	if matched, err := db.failInferenceTask(t.Context(), reclaimed, "provider_unavailable", true, 0); err != nil || !matched {
		t.Fatalf("다섯 번째 실패=%v, %v", matched, err)
	}
	if err := db.pool.QueryRow(t.Context(), `SELECT state FROM public.relation_inference_task WHERE context_id=$1`, stored.ID.String()).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("재시도 소진=%s: %v", state, err)
	}
	op.Kind = OperationDiscard
	if _, err := db.DiscardContext(t.Context(), graph, stored.ID, op, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	op.Kind = OperationUpdate
	restored, err := db.RestoreContext(t.Context(), graph, stored.ID, op, WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := db.pool.QueryRow(t.Context(), `SELECT state,target_version FROM public.relation_inference_task WHERE context_id=$1`, stored.ID.String()).Scan(&state, &version); err != nil || state != "pending" || version != restored.Version {
		t.Fatalf("복구 재등록=%s/%d: %v", state, version, err)
	}
	latest, found, err := db.claimInferenceTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatalf("복구 작업 확보=%v, %v", found, err)
	}
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := takeInferenceTask(t.Context(), tx, first); err != nil || matched {
		t.Fatalf("이전 세대 결과=%v, %v", matched, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err = db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := takeInferenceTask(t.Context(), tx, latest); err != nil || !matched {
		t.Fatalf("현재 세대 소비=%v, %v", matched, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err = db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := takeInferenceTask(t.Context(), tx, latest); err != nil || !matched {
		t.Fatalf("롤백 뒤 세대 소비=%v, %v", matched, err)
	}
	runID := newTestID(t)
	if err := insertInferenceRun(t.Context(), tx, inferenceRunRecord{ID: runID, GraphID: graph, ContextID: stored.ID, TargetVersion: latest.TargetVersion,
		ProfileFingerprint: "synthetic", ProfileVersions: "{}", Candidates: "[]", Result: "none", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.relation_inference_task WHERE context_id=$1`, stored.ID.String()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("결과 커밋 뒤 작업=%d, %v", count, err)
	}
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.relation_inference_run WHERE run_id=$1`, runID.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("결과 회차=%d, %v", count, err)
	}

	remove := lifeFailGraphWrite(t, db, graph, "relation_inference_task", "INSERT", false)
	defer remove()
	failed := testEventContext(t, graph, actor, member.ID)
	if _, err := db.CreateContextWithOperation(t.Context(), graph, failed, nil, inferenceCreateOperation(failed), WriteLimits{}); err == nil {
		t.Fatal("작업 등록 실패를 허용했다")
	}
	if _, err := db.Context(t.Context(), graph, failed.ID); err == nil {
		t.Fatal("실패한 사건 저장이 남았다")
	}
}
