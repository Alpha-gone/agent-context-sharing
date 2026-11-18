package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

// SegmentBackend는 구간화의 스냅숏과 세대별 반영 경계다.
type SegmentBackend interface {
	ClaimConversationSegmentTask(context.Context, time.Duration) (store.ConversationSegmentTask, bool, error)
	ReadConversationSegmentSnapshot(context.Context, store.ConversationSegmentTask) (store.ConversationSegmentSnapshot, error)
	ApplyConversationSegment(context.Context, store.ConversationSegmentTask, model.Context, int64, int64, bool, store.WriteLimits) (model.Context, bool, error)
	UpdateConversationSegment(context.Context, store.ConversationSegmentTask, model.Context, int64, int64, bool, store.WriteLimits) (model.Context, bool, error)
	CompleteConversationSegmentTask(context.Context, store.ConversationSegmentTask) (bool, error)
	DeferConversationSegmentTask(context.Context, store.ConversationSegmentTask, time.Time) (bool, error)
	FailConversationSegmentTask(context.Context, store.ConversationSegmentTask, string, bool) (bool, error)
}

type SegmentWorker struct {
	backend                                 SegmentBackend
	config                                  SegmentConfig
	plans                                   plan.AccountPlans
	logger                                  *slog.Logger
	indexDiagnostics, managementDiagnostics diagnostic.Observer
	runMu                                   sync.Mutex
	startOnce, stopOnce                     sync.Once
	cancel                                  context.CancelFunc
	done                                    chan struct{}
}

// NewSegmentWorker는 작업을 시작하지 않고 구성을 검증한다.
func NewSegmentWorker(backend SegmentBackend, cfg SegmentConfig, plans plan.AccountPlans, logger *slog.Logger) (*SegmentWorker, error) {
	if backend == nil {
		return nil, fmt.Errorf("구간화 저장소가 필요하다")
	}
	if _, err := ClosedSegments(nil, cfg, time.Now().UTC()); err != nil {
		return nil, err
	}
	return &SegmentWorker{backend: backend, config: cfg, plans: plans, logger: logger, done: make(chan struct{})}, nil
}

// SetDiagnostics는 시작 전에 고정 필드만 받는 진단 경계를 주입한다.
func (w *SegmentWorker) SetDiagnostics(index, management diagnostic.Observer) {
	w.indexDiagnostics = index
	w.managementDiagnostics = management
}

// RunOnce는 확보한 세대만 처리하고 실패는 원천과 독립적으로 기록한다.
func (w *SegmentWorker) RunOnce(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !w.runMu.TryLock() {
		return false, nil
	}
	defer w.runMu.Unlock()
	task, found, err := w.backend.ClaimConversationSegmentTask(ctx, 2*time.Minute)
	if err != nil || !found {
		return found, err
	}
	ctx = diagnostic.ResumeJob(ctx, task.CorrelationID)
	if correlation, parseErr := model.ParseID(task.CorrelationID); parseErr == nil {
		ctx, _ = model.WithCorrelationID(ctx, correlation)
	}
	diagnostic.Observe(w.indexDiagnostics, ctx, "INFO", "segment_acquire", "completed", "completed", true, diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: task.GraphID.String()}, Index: diagnostic.IndexFields{TaskID: task.ID.String(), Attempts: uint64(task.Attempts), LeaseResult: "acquired"}})
	err = w.process(ctx, task)
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	reason := "segment_failed"
	retryable := true
	if errors.Is(err, store.ErrSegmentPermissionLost) {
		reason = "permission_lost"
		retryable = false
	}
	diagnosticReason := "store_unavailable"
	if !retryable {
		diagnosticReason = "permission_denied"
	}
	diagnostic.Observe(w.indexDiagnostics, ctx, "WARN", "segment_retry", "failed", diagnosticReason, true, diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: task.GraphID.String()}, Index: diagnostic.IndexFields{TaskID: task.ID.String(), Attempts: uint64(task.Attempts)}})
	_, recordErr := w.backend.FailConversationSegmentTask(ctx, task, reason, retryable)
	if recordErr != nil {
		return true, errors.Join(err, recordErr)
	}
	return true, err
}

type segmentAction struct {
	segment   Segment
	previous  *model.Context
	lateTurns []model.Context
	last      int64
}

func (w *SegmentWorker) process(ctx context.Context, task store.ConversationSegmentTask) error {
	snap, err := w.backend.ReadConversationSegmentSnapshot(ctx, task)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	turns := make(map[model.ID]model.Context, len(snap.Turns))
	for _, turn := range snap.Turns {
		turns[turn.ID] = turn
	}
	known := make(map[model.ID]bool)
	additions := make([][]model.Context, len(snap.Segments))
	var maxLast int64 = -1
	for i, record := range snap.Segments {
		if record.Event.Event == nil || record.First < 0 || record.Last < record.First || (i > 0 && snap.Segments[i-1].Last >= record.First) {
			return fmt.Errorf("저장된 구간 범위가 올바르지 않다")
		}
		maxLast = record.Last
		for _, id := range record.Event.Event.MemberIDs {
			if known[id] {
				return fmt.Errorf("구간 구성원이 중복됐다")
			}
			known[id] = true
		}
	}
	var fresh []model.Context
	for _, turn := range snap.Turns {
		if turn.Source == nil || turn.Source.TurnIndex == nil {
			return fmt.Errorf("수집 턴에 순서가 없다")
		}
		if known[turn.ID] || turn.DeletedAt != nil {
			continue
		}
		index := *turn.Source.TurnIndex
		if index > maxLast {
			fresh = append(fresh, turn)
			continue
		}
		target := -1
		for i, record := range snap.Segments {
			if record.First > index {
				break
			}
			target = i
		}
		if target < 0 {
			w.recordSkippedTurn(ctx, task, turn, "segment_unassigned")
			continue
		}
		record := snap.Segments[target]
		if record.FrozenAt != nil || record.Event.DeletedAt != nil {
			w.recordSkippedTurn(ctx, task, turn, "segment_frozen")
			continue
		}
		if len(record.Event.Event.MemberIDs)+len(additions[target]) >= 1000 {
			w.recordSkippedTurn(ctx, task, turn, "segment_unassigned")
			continue
		}
		additions[target] = append(additions[target], turn)
	}
	var actions []segmentAction
	for i, extra := range additions {
		if len(extra) == 0 {
			continue
		}
		record := snap.Segments[i]
		members := make([]model.Context, 0, len(record.Event.Event.MemberIDs)+len(extra))
		unavailable := false
		for _, id := range record.Event.Event.MemberIDs {
			turn, ok := turns[id]
			if !ok || turn.DeletedAt != nil {
				unavailable = true
				break
			}
			members = append(members, turn)
		}
		if unavailable {
			for _, turn := range extra {
				w.recordSkippedTurn(ctx, task, turn, "segment_unassigned")
			}
			continue
		}
		members = append(members, extra...)
		segment, err := SummarizeSegment(members, w.config)
		if err != nil {
			return err
		}
		previous := record.Event
		actions = append(actions, segmentAction{segment: segment, previous: &previous, lateTurns: extra, last: max(record.Last, segment.Last)})
	}
	closed, err := ClosedSegments(fresh, w.config, snap.Now)
	if err != nil {
		return err
	}
	covered := 0
	for _, segment := range closed {
		covered += len(segment.Members)
		actions = append(actions, segmentAction{segment: segment, last: segment.Last})
	}
	hasOpen := covered < len(fresh)
	limits := w.plans.For(task.AccountID)
	writeLimits := store.WriteLimits{ActorID: task.AccountID, StoredCharsPerGraph: limits.StoredCharactersPerGraph}
	finalized := false
	for i, action := range actions {
		final := i == len(actions)-1 && !hasOpen
		var applied bool
		if action.previous != nil {
			next := *action.previous
			next.Event = new(*action.previous.Event)
			next.Event.MemberIDs = action.segment.Members
			next.Event.Start = action.segment.Start
			next.Event.End = new(action.segment.End)
			next.Body = action.segment.Body
			next.Version++
			_, applied, err = w.backend.UpdateConversationSegment(ctx, task, next, action.previous.Version, action.last, final, writeLimits)
		} else {
			id, idErr := model.NewID()
			if idErr != nil {
				return idErr
			}
			event := model.Context{ID: id, GraphID: task.GraphID, Layer: model.LayerEvent, Body: action.segment.Body, RecordedAt: snap.Now.UTC(), CreatedBy: task.AccountID, CreatedByAgent: task.AgentID, Version: 1, Event: &model.EventAttributes{ConversationID: task.ConversationID, MemberIDs: action.segment.Members, Start: action.segment.Start, End: new(action.segment.End)}}
			_, applied, err = w.backend.ApplyConversationSegment(ctx, task, event, action.segment.First, action.segment.Last, final, writeLimits)
		}
		if errors.Is(err, store.ErrSegmentFrozen) {
			for _, turn := range action.lateTurns {
				w.recordSkippedTurn(ctx, task, turn, "segment_frozen")
			}
			continue
		}
		if err != nil {
			return err
		}
		if !applied {
			return nil
		}
		if final {
			finalized = true
		}
	}
	if hasOpen {
		_, err = w.backend.DeferConversationSegmentTask(ctx, task, fresh[len(fresh)-1].RecordedAt.Add(w.config.Gap))
		return err
	}
	if !finalized {
		_, err = w.backend.CompleteConversationSegmentTask(ctx, task)
		return err
	}
	return nil
}

func (w *SegmentWorker) recordSkippedTurn(ctx context.Context, task store.ConversationSegmentTask, turn model.Context, reason string) {
	diagnostic.Observe(w.managementDiagnostics, ctx, "INFO", "segment_skip", "skipped", reason, true, diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: task.GraphID.String(), ContextID: turn.ID.String()}})
}

// Start는 독립 작업 회차를 시작한다.
func (w *SegmentWorker) Start(parent context.Context) {
	w.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		w.cancel = cancel
		go func() {
			defer close(w.done)
			for ctx.Err() == nil {
				worked, err := w.RunOnce(ctx)
				if err != nil && ctx.Err() == nil && w.logger != nil {
					w.logger.ErrorContext(ctx, "구간화 작업 처리 실패", "reason_code", "segment_cycle_failed")
				}
				if worked && err == nil {
					continue
				}
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	})
}

// Close는 회차를 취소하고 종료를 기다린다.
func (w *SegmentWorker) Close(ctx context.Context) error {
	w.startOnce.Do(func() { close(w.done) })
	w.stopOnce.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
	})
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
