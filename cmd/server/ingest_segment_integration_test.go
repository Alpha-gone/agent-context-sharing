package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/ingest"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

type segmentObserver struct{ reasons []string }

func (o *segmentObserver) RecordFields(_ context.Context, _, _, _, reason string, _ bool, _ diagnostic.Fields) bool {
	o.reasons = append(o.reasons, reason)
	return true
}

// backdateIngestSource advances only the synthetic database clock fixture, not production input.
func backdateIngestSource(t *testing.T, f *mcpWriteFixture, id model.ID, at time.Time) {
	t.Helper()
	query := fmt.Sprintf(`SELECT * FROM ag_catalog.cypher('agent_context', $$ MATCH (n:Context) WHERE n.context_id = '%s' AND n.graph_id = '%s' SET n.recorded_at = '%s' RETURN n $$) AS (node ag_catalog.agtype)`, id.String(), f.graph.String(), at.UTC().Format(time.RFC3339Nano))
	if _, err := f.pool.Exec(t.Context(), query); err != nil {
		t.Fatal(err)
	}
}

func TestIngestSegmentWorkerIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	token, err := f.database.IssueIngestCredential(t.Context(), f.principal.AccountID, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, agent, err := f.database.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.NewWithInference(t.Context(), f.pool.Config().ConnString(), "agent_context", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	worker, err := ingest.NewSegmentWorker(writer, ingest.SegmentConfig{Gap: 30 * time.Minute, MaxTurns: 40, ExcerptChars: 300, BodyChars: 2000}, plan.AccountPlans{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	observer := &segmentObserver{}
	worker.SetDiagnostics(nil, observer)
	at := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	add := func(index int64, occurred time.Time, body string) {
		t.Helper()
		id, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		node := model.Context{ID: id, GraphID: f.graph, Layer: model.LayerSource, Body: body, RecordedAt: at, CreatedBy: f.principal.AccountID, CreatedByAgent: agent, Version: 1, Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelConversation, Locator: fmt.Sprintf("conversation:///synthetic/%d", index)}, OccurredAt: occurred, OriginKind: model.OriginKindUserUtterance, ConversationID: "synthetic", TurnIndex: new(index)}}
		if _, _, err := f.database.CreateIngestSource(t.Context(), f.graph, node, store.WriteLimits{}, 30*time.Minute, ""); err != nil {
			t.Fatal(err)
		}
		backdateIngestSource(t, f, id, at)
	}
	run := func() {
		t.Helper()
		if _, err := f.pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp() WHERE graph_id=$1`, f.graph.String()); err != nil {
			t.Fatal(err)
		}
		if found, err := worker.RunOnce(t.Context()); err != nil || !found {
			t.Fatalf("구간 회차: %v %v", found, err)
		}
	}
	manualSourceID, _ := model.NewID()
	manualIndex := int64(3)
	manualAt := at.Add(30 * time.Minute)
	manualSource := model.Context{ID: manualSourceID, GraphID: f.graph, Layer: model.LayerSource, Body: "에이전트 원천", RecordedAt: at, CreatedBy: f.principal.AccountID, CreatedByAgent: f.agent, Version: 1, Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelConversation, Locator: "conversation:///manual/3"}, OccurredAt: manualAt, OriginKind: model.OriginKindUserUtterance, ConversationID: "synthetic", TurnIndex: &manualIndex}}
	if _, err := f.database.CreateContext(t.Context(), f.graph, manualSource, nil); err != nil {
		t.Fatal(err)
	}
	manualID, _ := model.NewID()
	manualEvent := model.Context{ID: manualID, GraphID: f.graph, Layer: model.LayerEvent, Body: "에이전트 사건", RecordedAt: at, CreatedBy: f.principal.AccountID, CreatedByAgent: f.agent, Version: 1, Event: &model.EventAttributes{ConversationID: "synthetic", MemberIDs: []model.ID{manualSourceID}, Start: manualAt, End: new(manualAt)}}
	if _, err := f.database.CreateContext(t.Context(), f.graph, manualEvent, nil); err != nil {
		t.Fatal(err)
	}
	add(0, at, "처음")
	add(4, at.Add(time.Hour), "다음")
	run()
	rows, err := f.pool.Query(t.Context(), `SELECT event_context_id::text FROM public.conversation_segment WHERE graph_id=$1 ORDER BY first_turn_index`, f.graph.String())
	if err != nil {
		t.Fatal(err)
	}
	var ids []model.ID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		id, err := model.ParseID(raw)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ids) != 2 {
		t.Fatalf("사건 수: %d %v", len(ids), err)
	}
	relations, _, err := f.database.ListContextRelations(t.Context(), f.graph, ids[0], []model.RelationState{model.RelationStateConfirmed}, []model.RelationType{model.RelationTypePrecedes}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range relations {
		if r.ToContextID == ids[1] && r.ConfirmedSource == model.ConfirmationSourceIngest && r.ProposedBy == model.ProposalSourceSystem {
			found = true
		}
	}
	if !found {
		t.Fatal("수집 선행 관계 누락")
	}
	for _, id := range ids {
		var indexCount, inferenceCount int
		if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public.index_task WHERE context_id=$1),(SELECT count(*) FROM public.relation_inference_task WHERE context_id=$1)`, id.String()).Scan(&indexCount, &inferenceCount); err != nil || indexCount != 1 || inferenceCount != 1 {
			t.Fatalf("사건 작업 등록: %d %d %v", indexCount, inferenceCount, err)
		}
	}
	for _, relation := range relations {
		if relation.ToContextID == manualID || relation.FromContextID == manualID {
			t.Fatal("에이전트 사건과 연결됐다")
		}
	}
	taskID, _ := model.NewID()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO public.conversation_segment_task (task_id,graph_id,conversation_id,trigger_account_id,trigger_agent_id,state,enqueued_at,next_attempt_at,correlation_id) VALUES ($1,$2,'synthetic',$3,$4,'pending',statement_timestamp(),statement_timestamp(),'')`, taskID.String(), f.graph.String(), f.principal.AccountID.String(), agent.String()); err != nil {
		t.Fatal(err)
	}
	run()
	var duplicateCount int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id='synthetic'`, f.graph.String()).Scan(&duplicateCount); err != nil || duplicateCount != 2 {
		t.Fatalf("재처리 구간 수: %d %v", duplicateCount, err)
	}
	add(1, at.Add(10*time.Minute), "늦은 발화")
	run()
	event, err := f.database.Context(t.Context(), f.graph, ids[0])
	if err != nil || event.Version != 2 || event.Body != "처음\n늦은 발화" {
		t.Fatalf("늦은 턴 반영: %+v %v", event, err)
	}
	next := event
	next.Event = new(*event.Event)
	next.Body = "사용자 수정"
	next.Version++
	op := &store.OperationRecord{Kind: store.OperationUpdate, GraphID: f.graph, ContextID: next.ID, AccountID: f.principal.AccountID, AgentID: f.agent, JudgmentInput: "synthetic edit"}
	if _, err := f.database.UpdateContextWithOperation(t.Context(), f.graph, event.Version, next, op, store.WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	add(2, at.Add(20*time.Minute), "고정 후 턴")
	run()
	final, err := f.database.Context(t.Context(), f.graph, ids[0])
	if err != nil || final.Version != 3 || final.Body != "사용자 수정" {
		t.Fatalf("고정 사건 변경: %+v %v", final, err)
	}
	hasFrozen := false
	for _, reason := range observer.reasons {
		if reason == "segment_frozen" {
			hasFrozen = true
		}
	}
	if !hasFrozen {
		t.Fatal("고정 구간 진단 누락")
	}
	manualAfter, err := f.database.Context(t.Context(), f.graph, manualID)
	if err != nil || manualAfter.Version != 1 || manualAfter.Body != "에이전트 사건" {
		t.Fatalf("에이전트 사건 변경: %+v %v", manualAfter, err)
	}
}

func TestIngestSegmentPermissionLossIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	editor, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err = f.database.CreateAccount(t.Context(), store.Account{ID: editor, LoginID: "ingest_editor", PasswordHash: "fixture", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err = f.database.GrantGraph(t.Context(), f.graph, f.principal.AccountID, editor, store.GrantSubjectAccount, model.GraphGradeEditor); err != nil {
		t.Fatal(err)
	}
	token, err := f.database.IssueIngestCredential(t.Context(), editor, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, agent, err := f.database.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-2 * time.Hour)
	id, _ := model.NewID()
	index := int64(0)
	source := model.Context{ID: id, GraphID: f.graph, Layer: model.LayerSource, Body: "synthetic", RecordedAt: at, CreatedBy: editor, CreatedByAgent: agent, Version: 1, Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelConversation, Locator: "conversation:///permission/0"}, OccurredAt: at, OriginKind: model.OriginKindUserUtterance, ConversationID: "permission", TurnIndex: &index}}
	if _, _, err = f.database.CreateIngestSource(t.Context(), f.graph, source, store.WriteLimits{}, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	backdateIngestSource(t, f, id, at)
	if err = f.database.RevokeGraphGrant(t.Context(), f.graph, store.GrantSubjectAccount, editor); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp() WHERE graph_id=$1`, f.graph.String()); err != nil {
		t.Fatal(err)
	}
	worker, err := ingest.NewSegmentWorker(f.database, ingest.SegmentConfig{Gap: time.Minute, MaxTurns: 40, ExcerptChars: 300, BodyChars: 2000}, plan.AccountPlans{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found, err := worker.RunOnce(t.Context())
	if !found || !errors.Is(err, store.ErrSegmentPermissionLost) {
		t.Fatalf("위임 권한 거부: %v %v", found, err)
	}
	var state, reason string
	var events int
	if err = f.pool.QueryRow(t.Context(), `SELECT state,last_error FROM public.conversation_segment_task WHERE graph_id=$1 AND conversation_id='permission'`, f.graph.String()).Scan(&state, &reason); err != nil || state != "failed" || reason != "permission_lost" {
		t.Fatalf("권한 상실 상태: %s %s %v", state, reason, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id='permission'`, f.graph.String()).Scan(&events); err != nil || events != 0 {
		t.Fatalf("권한 없는 사건: %d %v", events, err)
	}
	if _, err = f.database.Context(t.Context(), f.graph, id); err != nil {
		t.Fatalf("수집 원천이 사라졌다: %v", err)
	}
	if err = f.database.GrantGraph(t.Context(), f.graph, f.principal.AccountID, editor, store.GrantSubjectAccount, model.GraphGradeEditor); err != nil {
		t.Fatal(err)
	}
	next := source
	next.ID, _ = model.NewID()
	next.Source = new(*source.Source)
	nextIndex := int64(1)
	next.Source.TurnIndex = &nextIndex
	next.Source.Reference.Locator = "conversation:///permission/1"
	next.Source.OccurredAt = at.Add(time.Second)
	if _, _, err = f.database.CreateIngestSource(t.Context(), f.graph, next, store.WriteLimits{}, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	backdateIngestSource(t, f, next.ID, at)
	if err = f.pool.QueryRow(t.Context(), `SELECT state FROM public.conversation_segment_task WHERE graph_id=$1 AND conversation_id='permission'`, f.graph.String()).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("새 수집 뒤 작업 복구: %s %v", state, err)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp() WHERE graph_id=$1`, f.graph.String()); err != nil {
		t.Fatal(err)
	}
	if found, err = worker.RunOnce(t.Context()); err != nil || !found {
		t.Fatalf("권한 복구 뒤 처리: %v %v", found, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id='permission'`, f.graph.String()).Scan(&events); err != nil || events != 1 {
		t.Fatalf("권한 복구 뒤 사건: %d %v", events, err)
	}

}

func TestIngestSegmentServerPlacementIntegration(t *testing.T) {
	for _, mode := range []struct {
		name      string
		placement config.ComponentPlacement
		enabled   bool
	}{
		{"embedded", config.PlacementEmbedded, true}, {"external", config.PlacementExternal, true}, {"disabled", config.PlacementEmbedded, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			f := newMCPWriteFixture(t)
			token, err := f.database.IssueIngestCredential(t.Context(), f.principal.AccountID, "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			_, agent, err := f.database.ValidateIngestCredential(t.Context(), token)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := model.NewID()
			if _, err = f.pool.Exec(t.Context(), `INSERT INTO public.conversation_segment_task (task_id,graph_id,conversation_id,trigger_account_id,trigger_agent_id,state,enqueued_at,next_attempt_at,correlation_id) VALUES ($1,$2,'empty',$3,$4,'pending',statement_timestamp(),statement_timestamp(),'')`, id.String(), f.graph.String(), f.principal.AccountID.String(), agent.String()); err != nil {
				t.Fatal(err)
			}
			cfg := arch06Configuration(t, f.pool.Config().ConnString(), "https://provider.invalid")
			cfg.IndexWorkerPlacement = mode.placement
			cfg.ConversationIngestEnabled = mode.enabled
			assembly, err := assembleApplication(t.Context(), cfg, &http.Client{Transport: &inferenceTransport{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if (assembly.segmenter != nil) != mode.enabled {
				t.Fatal("구간화 작업자 활성 상태 불일치")
			}
			ctx, cancel := context.WithCancel(t.Context())
			assembly.startWorkers(ctx)
			defer func() {
				cancel()
				if err := assembly.close(nil, true); err != nil {
					t.Error(err)
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			var n int
			for {
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment_task WHERE task_id=$1`, id.String()).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n == 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if mode.enabled && mode.placement == config.PlacementEmbedded && n != 0 {
				t.Fatal("내장 작업자가 작업을 소비하지 않았다")
			}
			if (!mode.enabled || mode.placement == config.PlacementExternal) && n != 1 {
				t.Fatal("분리 배치에서 서버가 작업을 소비했다")
			}
		})
	}
}

func TestIngestSegmentTimeReversalIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	token, err := f.database.IssueIngestCredential(t.Context(), f.principal.AccountID, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, agent, err := f.database.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Second)
	times := []time.Time{base.Add(2 * time.Hour), base.Add(2*time.Hour + 10*time.Minute), base}
	var ids []model.ID
	for i, occurred := range times {
		id, _ := model.NewID()
		ids = append(ids, id)
		index := int64(i)
		source := model.Context{ID: id, GraphID: f.graph, Layer: model.LayerSource, Body: "합성 발화", RecordedAt: base, CreatedBy: f.principal.AccountID, CreatedByAgent: agent, Version: 1, Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelConversation, Locator: fmt.Sprintf("conversation:///time-reversal/%d", index)}, OccurredAt: occurred, OriginKind: model.OriginKindUserUtterance, ConversationID: "time-reversal", TurnIndex: &index}}
		if _, _, err = f.database.CreateIngestSource(t.Context(), f.graph, source, store.WriteLimits{}, time.Minute, ""); err != nil {
			t.Fatal(err)
		}
		backdateIngestSource(t, f, id, base)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp() WHERE graph_id=$1`, f.graph.String()); err != nil {
		t.Fatal(err)
	}
	worker, err := ingest.NewSegmentWorker(f.database, ingest.SegmentConfig{Gap: 30 * time.Minute, MaxTurns: 2, ExcerptChars: 300, BodyChars: 2000}, plan.AccountPlans{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := worker.RunOnce(t.Context()); !found || err == nil {
		t.Fatalf("시간 역행 사건 생성 허용: %v %v", found, err)
	}
	var count, attempts int
	var state string
	if err = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id='time-reversal'),attempts,state FROM public.conversation_segment_task WHERE graph_id=$1 AND conversation_id='time-reversal'`, f.graph.String()).Scan(&count, &attempts, &state); err != nil || count != 1 || attempts != 1 || state != "pending" {
		t.Fatalf("부분 반영·재시도: %d %d %s %v", count, attempts, state, err)
	}
	for _, id := range ids {
		if _, err := f.database.Context(t.Context(), f.graph, id); err != nil {
			t.Fatalf("원천 손실: %v", err)
		}
	}
}

func TestIngestSegmentDeletedMemberIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	token, err := f.database.IssueIngestCredential(t.Context(), f.principal.AccountID, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, agent, err := f.database.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Second)
	add := func(index int64, at time.Time) model.ID {
		t.Helper()
		id, _ := model.NewID()
		source := model.Context{ID: id, GraphID: f.graph, Layer: model.LayerSource, Body: fmt.Sprintf("발화 %d", index), RecordedAt: base, CreatedBy: f.principal.AccountID, CreatedByAgent: agent, Version: 1, Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelConversation, Locator: fmt.Sprintf("conversation:///deleted/%d", index)}, OccurredAt: at, OriginKind: model.OriginKindUserUtterance, ConversationID: "deleted-member", TurnIndex: new(index)}}
		if _, _, err := f.database.CreateIngestSource(t.Context(), f.graph, source, store.WriteLimits{}, time.Minute, ""); err != nil {
			t.Fatal(err)
		}
		backdateIngestSource(t, f, id, base)
		return id
	}
	worker, err := ingest.NewSegmentWorker(f.database, ingest.SegmentConfig{Gap: 30 * time.Minute, MaxTurns: 40, ExcerptChars: 300, BodyChars: 2000}, plan.AccountPlans{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	observer := &segmentObserver{}
	worker.SetDiagnostics(nil, observer)
	run := func() {
		t.Helper()
		if _, err := f.pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp() WHERE graph_id=$1`, f.graph.String()); err != nil {
			t.Fatal(err)
		}
		if found, err := worker.RunOnce(t.Context()); !found || err != nil {
			t.Fatalf("구간 처리: %v %v", found, err)
		}
	}
	original := add(0, base)
	add(2, base.Add(time.Hour))
	run()
	var raw string
	if err := f.pool.QueryRow(t.Context(), `SELECT event_context_id::text FROM public.conversation_segment WHERE graph_id=$1 AND first_turn_index=0`, f.graph.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	eventID, err := model.ParseID(raw)
	if err != nil {
		t.Fatal(err)
	}
	op := &store.OperationRecord{Kind: store.OperationDiscard, GraphID: f.graph, ContextID: original, AccountID: f.principal.AccountID, AgentID: f.agent, JudgmentInput: "synthetic discard"}
	if _, err := f.database.DiscardContext(t.Context(), f.graph, original, op, store.WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	late := add(1, base.Add(10*time.Minute))
	add(3, base.Add(2*time.Hour))
	run()
	event, err := f.database.Context(t.Context(), f.graph, eventID)
	if err != nil || event.Version != 1 || len(event.Event.MemberIDs) != 1 || event.Event.MemberIDs[0] != original {
		t.Fatalf("폐기 구성원 사건 변경: %+v %v", event, err)
	}
	if _, err := f.database.Context(t.Context(), f.graph, late); err != nil {
		t.Fatalf("늦은 원천 손실: %v", err)
	}
	var segments, tasks int
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id='deleted-member'),(SELECT count(*) FROM public.conversation_segment_task WHERE graph_id=$1 AND conversation_id='deleted-member')`, f.graph.String()).Scan(&segments, &tasks); err != nil || segments != 3 || tasks != 0 {
		t.Fatalf("후속 구간 또는 작업 상태: %d %d %v", segments, tasks, err)
	}
	found := false
	for _, reason := range observer.reasons {
		if reason == "segment_unassigned" {
			found = true
		}
	}
	if !found {
		t.Fatal("폐기 구성원 진단 누락")
	}
}
