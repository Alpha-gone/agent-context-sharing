package store

import (
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"errors"
	"testing"
	"time"
)

func TestIngestSourceAtomicIntegration(t *testing.T) {
	database, _, pool := newAgentContractDatabase(t)
	account := newTestID(t)
	createTestAccount(t, database, account)
	graph := createTestGraph(t, database, account)
	grantAccount(t, database, graph, account, model.GraphGradeOwner)
	token, err := database.IssueIngestCredential(t.Context(), account, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, agent, err := database.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	source := conversationSource(t, graph, account, "synthetic-conversation", 0)
	source.CreatedByAgent = agent
	source.RecordedAt = time.Now().UTC().Add(-24 * time.Hour)
	var before time.Time
	if err := pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	value, existing, err := database.CreateIngestSource(t.Context(), graph, source, WriteLimits{}, 30*time.Minute, "synthetic")
	if err != nil || existing || value.ID != source.ID {
		t.Fatalf("원천 저장: existing=%v err=%v", existing, err)
	}
	var after time.Time
	if err := pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if value.RecordedAt.Before(before) || value.RecordedAt.After(after) {
		t.Fatalf("수집 기록 시각이 DB 시계 범위 밖: %v (%v - %v)", value.RecordedAt, before, after)
	}
	var judgment string
	if err := pool.QueryRow(t.Context(), `SELECT judgment_source FROM public.operation_log WHERE graph_id=$1 AND context_id=$2`, graph.String(), source.ID.String()).Scan(&judgment); err != nil || judgment != "ingest" {
		t.Fatalf("판정 주체: %s %v", judgment, err)
	}
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment_task WHERE graph_id=$1 AND conversation_id=$2`, graph.String(), "synthetic-conversation").Scan(&n); err != nil || n != 1 {
		t.Fatalf("작업 등록: %d %v", n, err)
	}
	repeated := source
	repeated.ID = newTestID(t)
	value, existing, err = database.CreateIngestSource(t.Context(), graph, repeated, WriteLimits{}, 30*time.Minute, "synthetic")
	if err != nil || !existing || value.ID != source.ID {
		t.Fatalf("멱등성: existing=%v err=%v", existing, err)
	}
	repeated.Body = "다른 본문"
	if _, _, err := database.CreateIngestSource(t.Context(), graph, repeated, WriteLimits{}, 30*time.Minute, "synthetic"); !errors.Is(err, ErrIngestBodyMismatch) {
		t.Fatalf("본문 불일치: %v", err)
	}
	prior := conversationSource(t, graph, account, "agent-created", 3)
	if _, err := database.CreateContext(t.Context(), graph, prior, nil); err != nil {
		t.Fatal(err)
	}
	incoming := prior
	incoming.ID = newTestID(t)
	incoming.CreatedByAgent = agent
	incoming.Source = new(*prior.Source)
	incoming.Source.Reference.Locator = "conversation:///agent-created/3"
	got, existing, err := database.CreateIngestSource(t.Context(), graph, incoming, WriteLimits{}, 30*time.Minute, "synthetic")
	if err != nil || !existing || got.ID != prior.ID {
		t.Fatalf("에이전트 원천 재사용: %v %v", existing, err)
	}
	limited := conversationSource(t, graph, account, "limited", 2)
	limited.CreatedByAgent = agent
	if _, _, err := database.CreateIngestSource(t.Context(), graph, limited, WriteLimits{StoredCharsPerGraph: 1}, 30*time.Minute, "synthetic"); err == nil {
		t.Fatal("저장 한도 초과 허용")
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment_task WHERE graph_id=$1 AND conversation_id='limited'`, graph.String()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("거부 뒤 작업 잔존: %d %v", n, err)
	}
	viewer := newTestID(t)
	createTestAccount(t, database, viewer)
	if err := database.GrantGraph(t.Context(), graph, account, viewer, GrantSubjectAccount, model.GraphGradeViewer); err != nil {
		t.Fatal(err)
	}
	denied := conversationSource(t, graph, viewer, "viewer", 1)
	denied.CreatedByAgent = agent
	if _, _, err := database.CreateIngestSource(t.Context(), graph, denied, WriteLimits{}, 30*time.Minute, "synthetic"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("등급 거부: %v", err)
	}
}

func TestIngestSegmentEnqueueDiagnosticIntegration(t *testing.T) {
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
	sink := &obs05Sink{}
	db.SetOperationDiagnostics(nil, obs05Reporter(t, diagnostic.Index, sink))
	ctx := obs05Context(t)
	source := conversationSource(t, graph, account, "synthetic-private", 0)
	source.CreatedByAgent = agent
	if _, _, err := db.CreateIngestSource(ctx, graph, source, WriteLimits{}, time.Minute, model.CorrelationID(ctx).String()); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := pool.QueryRow(t.Context(), `SELECT task_id::text FROM public.conversation_segment_task WHERE graph_id=$1`, graph.String()).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range sink.snapshot(t) {
		if event.Stage == "segment_enqueue" {
			found = true
			if event.Fields.Index.TaskID != taskID || event.CorrelationID != model.CorrelationID(ctx).String() || event.Fields.Index.Generation == 0 || event.Result != "pending" {
				t.Fatal("등록 진단 세대 불일치")
			}
		}
	}
	if !found {
		t.Fatal("등록 진단 누락")
	}
}
