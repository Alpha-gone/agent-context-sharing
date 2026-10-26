package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"agent_context_sharing/internal/model"
)

type trialContextKey struct{}
type trialRound struct {
	Condition string `json:"condition"`
	Sample    string `json:"sample,omitempty"`
	Repeat    int    `json:"repeat"`
}
type trialProgressSnapshot struct {
	LimitExceeded   bool         `json:"request_limit_exceeded"`
	Stage           string       `json:"stage"`
	Completed       []trialRound `json:"completed_rounds"`
	Requests        int          `json:"requests"`
	Graphs          int          `json:"created_graphs"`
	CleanupAttempts int          `json:"cleanup_attempts"`
	CleanupFailures int          `json:"cleanup_failures"`
	ActiveGraphs    int          `json:"active_graphs"`
	Cleanup         string       `json:"cleanup_status"`
	PreparationMS   float64      `json:"preparation_ms"`
}
type trialProgress struct {
	failureStage string
	mu           sync.Mutex
	value        trialProgressSnapshot
	active       map[model.ID]bool
	limits       manifestExecution
	deadline     time.Time
}

func newTrialProgress(limits manifestExecution) *trialProgress {
	return &trialProgress{value: trialProgressSnapshot{Stage: "input"}, active: map[model.ID]bool{}, limits: limits, deadline: time.Now().Add(time.Duration(limits.TimeoutMS) * time.Millisecond)}
}
func (p *trialProgress) setStage(stage string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.value.Stage = stage
}
func trialFrom(ctx context.Context) *trialProgress {
	p, _ := ctx.Value(trialContextKey{}).(*trialProgress)
	return p
}
func trialContext(p *trialProgress) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if p == nil {
		return ctx, stop
	}
	ctx, cancel := context.WithDeadline(context.WithValue(ctx, trialContextKey{}, p), p.deadline)
	return ctx, func() { cancel(); stop() }
}
func trialRequest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := trialFrom(ctx)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.value.Requests >= p.limits.RequestLimit {
		p.value.LimitExceeded = true
		return failTrial("request_limit_exceeded", nil)
	}
	p.value.Requests++
	return nil
}
func trialIndexLimit(ctx context.Context, limit int) int {
	if p := trialFrom(ctx); p != nil {
		return min(limit, p.limits.IndexRoundLimit)
	}
	return limit
}
func trialGraphCreated(ctx context.Context, id model.ID) {
	if p := trialFrom(ctx); p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.value.Graphs++
		p.active[id] = true
	}
}
func trialGraphCleanup(ctx context.Context, id model.ID, err error) {
	if p := trialFrom(ctx); p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.value.CleanupAttempts++
		if err != nil {
			p.value.CleanupFailures++
		} else {
			delete(p.active, id)
		}
	}
}
func trialRoundDone(ctx context.Context, condition, sample string, repeat int) {
	if p := trialFrom(ctx); p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.value.Completed = append(p.value.Completed, trialRound{condition, sample, repeat})
	}
}
func (p *trialProgress) snapshot() trialProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.value
	v.Completed = slices.Clone(v.Completed)
	slices.SortFunc(v.Completed, func(a, b trialRound) int {
		return cmp.Or(cmp.Compare(a.Condition, b.Condition), cmp.Compare(a.Sample, b.Sample), cmp.Compare(a.Repeat, b.Repeat))
	})
	if p.failureStage != "" {
		v.Stage = p.failureStage
	}
	v.ActiveGraphs = len(p.active)
	v.Cleanup = "not_applicable"
	if v.Graphs > 0 {
		v.Cleanup = "completed"
	}
	if v.ActiveGraphs > 0 || v.CleanupFailures > 0 {
		v.Cleanup = "failed"
	}
	return v
}
func trialPrepared(ctx context.Context, started time.Time, success bool) {
	if p := trialFrom(ctx); p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.value.PreparationMS += float64(time.Since(started).Microseconds()) / 1000
		if success {
			p.value.Stage = "measurement"
		} else if p.failureStage == "" {
			p.failureStage = "preparation"
		}
	}
}
func trialStage(ctx context.Context, stage string) {
	if p := trialFrom(ctx); p != nil {
		p.setStage(stage)
	}
}
func trialSafeCode(code string) error {
	if !manifestCode.MatchString(code) {
		return fmt.Errorf("평가 식별 코드는 비식별 코드여야 한다")
	}
	return nil
}

func trialFailed(ctx context.Context, stage string) {
	if p := trialFrom(ctx); p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.failureStage == "" {
			p.failureStage = stage
		}
	}
}
