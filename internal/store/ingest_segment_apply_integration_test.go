package store

import (
	"errors"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestConversationSegmentApplyIntegration(t *testing.T) {
	db, _, pool := newAgentContractDatabase(t)
	account := newTestID(t)
	createTestAccount(t, db, account)
	graph := createTestGraph(t, db, account)
	grantAccount(t, db, graph, account, model.GraphGradeOwner)
	token, err := db.IssueIngestCredential(t.Context(), account, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, agent, err := db.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	src := conversationSource(t, graph, account, "synthetic", 0)
	src.CreatedByAgent = agent
	if _, _, err = db.CreateIngestSource(t.Context(), graph, src, WriteLimits{}, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp()`); err != nil {
		t.Fatal(err)
	}
	task, found, err := db.ClaimConversationSegmentTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatal("작업 확보", err)
	}
	agentSource := conversationSource(t, graph, account, "synthetic", 1)
	if _, err := db.CreateContext(t.Context(), graph, agentSource, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.ReadConversationSegmentSnapshot(t.Context(), task)
	if err != nil || len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != src.ID || snapshot.Now.IsZero() {
		t.Fatalf("수집 스냅숏: %d %v", len(snapshot.Turns), err)
	}
	event := conversationEvent(t, graph, account, src.ID, "synthetic", src.Source.OccurredAt, new(src.Source.OccurredAt))
	event.CreatedByAgent = agent
	remove := lifeFailGraphWrite(t, db, graph, "conversation_segment", "INSERT", false)
	if _, _, err := db.ApplyConversationSegment(t.Context(), task, event, 0, 0, true, WriteLimits{}); err == nil {
		t.Fatal("구간 기록 실패를 허용했다")
	}
	remove()
	if _, err := db.Context(t.Context(), graph, event.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("실패한 사건이 남았다: %v", err)
	}
	if _, err := db.Context(t.Context(), graph, src.ID); err != nil {
		t.Fatalf("원천이 되돌려졌다: %v", err)
	}
	got, applied, err := db.ApplyConversationSegment(t.Context(), task, event, 0, 0, true, WriteLimits{})
	if err != nil || !applied || got.ID != event.ID {
		t.Fatalf("사건 반영: %v %v", applied, err)
	}
	var count int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment WHERE event_context_id=$1`, event.ID.String()).Scan(&count); err != nil || count != 1 {
		t.Fatal("구간 저장", count, err)
	}
	if _, applied, err = db.ApplyConversationSegment(t.Context(), task, event, 0, 0, true, WriteLimits{}); err != nil || applied {
		t.Fatalf("완료 세대 재반영: %v %v", applied, err)
	}
	if _, err = db.Context(t.Context(), graph, event.ID); err != nil {
		t.Fatal(err)
	}

	late := conversationSource(t, graph, account, "synthetic", 2)
	late.CreatedByAgent = agent
	if _, _, err = db.CreateIngestSource(t.Context(), graph, late, WriteLimits{}, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp()`); err != nil {
		t.Fatal(err)
	}
	task, found, err = db.ClaimConversationSegmentTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatal("늦은 턴 확보", err)
	}
	next := attachReferences(got, nil, got.Event.MemberIDs)
	next.Event.MemberIDs = append(next.Event.MemberIDs, late.ID)
	next.Event.End = new(late.Source.OccurredAt)
	next.Body += " late"
	next.Version++
	updated, applied, err := db.UpdateConversationSegment(t.Context(), task, next, got.Version, 2, true, WriteLimits{})
	if err != nil || !applied || updated.Version != 2 {
		t.Fatalf("늦은 턴 갱신: %v %v", applied, err)
	}
	edited := attachReferences(updated, nil, updated.Event.MemberIDs)
	edited.Version++
	edited.Body += " 수정"
	if _, err = db.UpdateContextWithOperation(t.Context(), graph, updated.Version, edited, registrationOperation(edited, OperationUpdate), WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	extra := conversationSource(t, graph, account, "synthetic", 3)
	extra.CreatedByAgent = agent
	if _, _, err = db.CreateIngestSource(t.Context(), graph, extra, WriteLimits{}, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp()`); err != nil {
		t.Fatal(err)
	}
	task, found, err = db.ClaimConversationSegmentTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatal("고정 구간 작업 확보", err)
	}
	edited.Event.MemberIDs = append(edited.Event.MemberIDs, extra.ID)
	edited.Version++
	if _, _, err = db.UpdateConversationSegment(t.Context(), task, edited, edited.Version-1, 3, true, WriteLimits{}); !errors.Is(err, ErrSegmentFrozen) {
		t.Fatalf("고정 구간 변경 허용: %v", err)
	}
}

func TestConversationSegmentApplyStaleIntegration(t *testing.T) {
	db, _, pool := newAgentContractDatabase(t)
	account := newTestID(t)
	createTestAccount(t, db, account)
	graph := createTestGraph(t, db, account)
	grantAccount(t, db, graph, account, model.GraphGradeOwner)
	token, err := db.IssueIngestCredential(t.Context(), account, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, agent, err := db.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	first := conversationSource(t, graph, account, "stale", 0)
	first.CreatedByAgent = agent
	if _, _, err = db.CreateIngestSource(t.Context(), graph, first, WriteLimits{}, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp()`); err != nil {
		t.Fatal(err)
	}
	task, found, err := db.ClaimConversationSegmentTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatal("리스", err)
	}
	second := conversationSource(t, graph, account, "stale", 1)
	second.CreatedByAgent = agent
	if _, _, err = db.CreateIngestSource(t.Context(), graph, second, WriteLimits{}, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	event := conversationEvent(t, graph, account, first.ID, "stale", first.Source.OccurredAt, new(first.Source.OccurredAt))
	event.CreatedByAgent = agent
	if _, applied, err := db.ApplyConversationSegment(t.Context(), task, event, 0, 0, true, WriteLimits{}); err != nil || applied {
		t.Fatalf("이전 세대가 반영됐다: %v %v", applied, err)
	}
	if _, err := db.Context(t.Context(), graph, event.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("이전 세대 사건 존재: %v", err)
	}
}
