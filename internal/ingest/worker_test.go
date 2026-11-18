package ingest

import (
	"context"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

type segmentBackendStub struct {
	task               store.ConversationSegmentTask
	snapshot           store.ConversationSegmentSnapshot
	updates, completes int
}

func (b *segmentBackendStub) ClaimConversationSegmentTask(context.Context, time.Duration) (store.ConversationSegmentTask, bool, error) {
	return b.task, true, nil
}
func (b *segmentBackendStub) ReadConversationSegmentSnapshot(context.Context, store.ConversationSegmentTask) (store.ConversationSegmentSnapshot, error) {
	return b.snapshot, nil
}
func (b *segmentBackendStub) ApplyConversationSegment(context.Context, store.ConversationSegmentTask, model.Context, int64, int64, bool, store.WriteLimits) (model.Context, bool, error) {
	return model.Context{}, false, nil
}
func (b *segmentBackendStub) UpdateConversationSegment(context.Context, store.ConversationSegmentTask, model.Context, int64, int64, bool, store.WriteLimits) (model.Context, bool, error) {
	b.updates++
	return model.Context{}, true, nil
}
func (b *segmentBackendStub) CompleteConversationSegmentTask(context.Context, store.ConversationSegmentTask) (bool, error) {
	b.completes++
	return true, nil
}
func (b *segmentBackendStub) DeferConversationSegmentTask(context.Context, store.ConversationSegmentTask, time.Time) (bool, error) {
	return true, nil
}
func (b *segmentBackendStub) FailConversationSegmentTask(context.Context, store.ConversationSegmentTask, string, bool) (bool, error) {
	return true, nil
}

type segmentDiagnosticStub struct{ reasons []string }

func (d *segmentDiagnosticStub) RecordFields(_ context.Context, _, _, _, reason string, _ bool, _ diagnostic.Fields) bool {
	d.reasons = append(d.reasons, reason)
	return true
}

func TestSegmentWorkerSkipsFullSegment(t *testing.T) {
	id := func() model.ID { v, _ := model.NewID(); return v }
	now := time.Now().UTC()
	graph := id()
	members := make([]model.ID, 0, 1000)
	turns := make([]model.Context, 0, 1001)
	for i := range 1000 {
		turnID := id()
		index := int64(i * 2)
		members = append(members, turnID)
		turns = append(turns, model.Context{ID: turnID, GraphID: graph, Body: "synthetic", RecordedAt: now, Source: &model.SourceAttributes{TurnIndex: &index, OccurredAt: now, OriginKind: model.OriginKindUserUtterance}})
	}
	extra := int64(1)
	turns = append(turns, model.Context{ID: id(), GraphID: graph, Body: "late", RecordedAt: now, Source: &model.SourceAttributes{TurnIndex: &extra, OccurredAt: now, OriginKind: model.OriginKindUserUtterance}})
	event := model.Context{ID: id(), GraphID: graph, Event: &model.EventAttributes{MemberIDs: members}}
	backend := &segmentBackendStub{task: store.ConversationSegmentTask{ID: id(), GraphID: graph, AccountID: id(), AgentID: id(), Generation: now, LeasedUntil: now}, snapshot: store.ConversationSegmentSnapshot{Now: now, Turns: turns, Segments: []store.ConversationSegmentRecord{{Event: event, First: 0, Last: 1998}}}}
	worker, err := NewSegmentWorker(backend, SegmentConfig{Gap: time.Minute, MaxTurns: 40, ExcerptChars: 300, BodyChars: 2000}, plan.AccountPlans{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	observer := &segmentDiagnosticStub{}
	worker.SetDiagnostics(nil, observer)
	if found, err := worker.RunOnce(t.Context()); err != nil || !found {
		t.Fatalf("작업: %v %v", found, err)
	}
	if backend.updates != 0 || backend.completes != 1 || len(observer.reasons) != 1 || observer.reasons[0] != "segment_unassigned" {
		t.Fatalf("상한 처리: updates=%d completes=%d reasons=%v", backend.updates, backend.completes, observer.reasons)
	}
}

func TestSegmentWorkerConcurrentStartClose(t *testing.T) {
	backend := &segmentBackendStub{}
	worker, err := NewSegmentWorker(backend, SegmentConfig{Gap: time.Minute, MaxTurns: 40, ExcerptChars: 300, BodyChars: 2000}, plan.AccountPlans{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() { worker.Start(ctx) })
		group.Go(func() {
			closeCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := worker.Close(closeCtx); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
}
