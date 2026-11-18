package store

import (
	"testing"
	"time"
)

func TestConversationSegmentQueueIntegration(t *testing.T) {
	first, second, pool := newAgentContractDatabase(t)
	taskID, graph, account, agent := newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	_, err := pool.Exec(t.Context(), `INSERT INTO public.conversation_segment_task (task_id,graph_id,conversation_id,trigger_account_id,trigger_agent_id,state,enqueued_at,next_attempt_at,correlation_id) VALUES ($1,$2,'synthetic',$3,$4,'pending',statement_timestamp(),statement_timestamp(),'')`, taskID.String(), graph.String(), account.String(), agent.String())
	if err != nil {
		t.Fatal(err)
	}
	task, found, err := first.ClaimConversationSegmentTask(t.Context(), time.Minute)
	if err != nil || !found || task.ID != taskID {
		t.Fatalf("작업 확보: %v %v", found, err)
	}
	if _, found, err := second.ClaimConversationSegmentTask(t.Context(), time.Minute); err != nil || found {
		t.Fatalf("중복 확보: %v %v", found, err)
	}
	_, err = pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET enqueued_at=enqueued_at+interval '1 microsecond', next_attempt_at=statement_timestamp() WHERE task_id=$1`, taskID.String())
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := first.FailConversationSegmentTask(t.Context(), task, "segment_failed", true); err != nil || changed {
		t.Fatalf("이전 세대 변경: %v %v", changed, err)
	}
	current, found, err := second.ClaimConversationSegmentTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatalf("새 세대 확보: %v %v", found, err)
	}
	if changed, err := first.FailConversationSegmentTask(t.Context(), current, "permission_lost", true); err != nil || !changed {
		t.Fatalf("권한 상실 기록: %v %v", changed, err)
	}
	var state string
	var attempts int
	if err := pool.QueryRow(t.Context(), `SELECT state,attempts FROM public.conversation_segment_task WHERE task_id=$1`, taskID.String()).Scan(&state, &attempts); err != nil || state != "failed" || attempts != 1 {
		t.Fatalf("실패 상태: %s %d %v", state, attempts, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET state='pending',attempts=4,next_attempt_at=statement_timestamp() WHERE task_id=$1`, taskID.String()); err != nil {
		t.Fatal(err)
	}
	fifth, found, err := first.ClaimConversationSegmentTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatal("다섯 번째 확보", err)
	}
	if changed, err := second.FailConversationSegmentTask(t.Context(), fifth, "segment_failed", true); err != nil || !changed {
		t.Fatal("재시도 한계 기록", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT state,attempts FROM public.conversation_segment_task WHERE task_id=$1`, taskID.String()).Scan(&state, &attempts); err != nil || state != "failed" || attempts != 5 {
		t.Fatalf("재시도 한계: %s %d %v", state, attempts, err)
	}

}
