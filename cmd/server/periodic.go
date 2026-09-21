package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

const (
	graceExpiryLockKey       int64 = 4182026101
	proposalCleanupLockKey   int64 = 4182026102
	auditCleanupLockKey      int64 = 4182026103
	revocationCleanupLockKey int64 = 4182026104
	codeCleanupLockKey       int64 = 4182026105
	rateCleanupLockKey       int64 = 4182026106
	embeddingTierLockKey     int64 = 4182026107
)

type periodicStore interface {
	WithTryAdvisoryLock(context.Context, int64, func(context.Context) (int, error)) (int, bool, error)
	ExpireGrace(context.Context, time.Time) (int, error)
	CleanupExpiredProposals(context.Context, time.Time, store.RetentionDays) (int, error)
	CleanupAuditRecords(context.Context, time.Time, store.RetentionDays, store.RetentionDays) (int, error)
	CleanupExpiredRevocations(context.Context, time.Time) (int, error)
	CleanupExpiredAuthorizationCodes(context.Context, time.Time) (int, error)
	CleanupRequestRateWindows(context.Context, time.Time) (int, error)
	MoveColdEmbeddings(context.Context, time.Time, store.RetentionDays) (int, error)
}

type periodicTask struct {
	name     string
	interval time.Duration
	lockKey  int64
	run      func(context.Context) (int, error)
}

// periodicWorker는 모든 애플리케이션 인스턴스에 내장되는 일곱 주기 작업의 시작과 종료를 관리한다.
type periodicWorker struct {
	store  periodicStore
	logger *slog.Logger
	now    func() time.Time
	tasks  []periodicTask

	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

func newPeriodicWorker(database *store.Store, plans plan.AccountPlans, logger *slog.Logger) (*periodicWorker, error) {
	if database == nil {
		return nil, fmt.Errorf("주기 작업 저장소가 없다")
	}
	if logger == nil {
		logger = slog.Default()
	}
	worker := &periodicWorker{store: database, logger: logger, now: time.Now, done: make(chan struct{})}
	limits := func(accountID model.ID) plan.Limits { return plans.For(accountID) }
	worker.tasks = []periodicTask{
		{name: "grace_expiry", interval: time.Hour, lockKey: graceExpiryLockKey, run: func(ctx context.Context) (int, error) {
			return database.ExpireGrace(ctx, worker.now().UTC())
		}},
		{name: "proposal_cleanup", interval: 24 * time.Hour, lockKey: proposalCleanupLockKey, run: func(ctx context.Context) (int, error) {
			return database.CleanupExpiredProposals(ctx, worker.now().UTC(), func(accountID model.ID) int { return limits(accountID).ProposalRetentionDays })
		}},
		{name: "audit_cleanup", interval: 24 * time.Hour, lockKey: auditCleanupLockKey, run: func(ctx context.Context) (int, error) {
			return database.CleanupAuditRecords(ctx, worker.now().UTC(), func(accountID model.ID) int { return limits(accountID).AuditRetentionDays }, func(accountID model.ID) int { return limits(accountID).RejectedOperationDays })
		}},
		{name: "revocation_cleanup", interval: time.Hour, lockKey: revocationCleanupLockKey, run: func(ctx context.Context) (int, error) {
			return database.CleanupExpiredRevocations(ctx, worker.now().UTC())
		}},
		{name: "authorization_code_cleanup", interval: time.Hour, lockKey: codeCleanupLockKey, run: func(ctx context.Context) (int, error) {
			return database.CleanupExpiredAuthorizationCodes(ctx, worker.now().UTC())
		}},
		{name: "request_rate_cleanup", interval: time.Hour, lockKey: rateCleanupLockKey, run: func(ctx context.Context) (int, error) {
			return database.CleanupRequestRateWindows(ctx, worker.now().UTC())
		}},
		{name: "embedding_tier_move", interval: 24 * time.Hour, lockKey: embeddingTierLockKey, run: func(ctx context.Context) (int, error) {
			return database.MoveColdEmbeddings(ctx, worker.now().UTC(), func(accountID model.ID) int { return limits(accountID).TierMoveAfterDays })
		}},
	}
	return worker, nil
}

// Start는 각 작업을 기동 직후 한 번 실행한 뒤 주기마다 실행하고, parent가 끝나면 새 회차를
// 시작하지 않는다. Close 뒤에 호출하면 아무것도 하지 않는다.
func (worker *periodicWorker) Start(parent context.Context) {
	worker.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		worker.cancel = cancel
		var group sync.WaitGroup
		for _, task := range worker.tasks {
			group.Go(func() { worker.runPeriodically(ctx, task) })
		}
		go func() {
			group.Wait()
			close(worker.done)
		}()
	})
}

// Close는 진행 중인 회차를 취소하고 잠금 해제까지 기다린다.
func (worker *periodicWorker) Close(ctx context.Context) error {
	// 시작하지 않은 작업자는 시작 기회를 먼저 소비해, 이후 Start가 done을 다시 닫지 않게 한다.
	worker.startOnce.Do(func() { close(worker.done) })
	worker.stopOnce.Do(func() {
		if worker.cancel != nil {
			worker.cancel()
		}
	})
	select {
	case <-worker.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (worker *periodicWorker) runPeriodically(ctx context.Context, task periodicTask) {
	worker.runTask(ctx, task)
	ticker := time.NewTicker(task.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			worker.runTask(ctx, task)
		}
	}
}

func (worker *periodicWorker) runTask(ctx context.Context, task periodicTask) {
	count, acquired, err := worker.store.WithTryAdvisoryLock(ctx, task.lockKey, task.run)
	if err != nil {
		worker.logger.ErrorContext(ctx, "주기 작업 실패", "task", task.name, "processed", count, "status", "failed", "error", err)
		return
	}
	if !acquired {
		worker.logger.InfoContext(ctx, "주기 작업 건너뜀", "task", task.name, "processed", 0, "status", "skipped")
		return
	}
	worker.logger.InfoContext(ctx, "주기 작업 완료", "task", task.name, "processed", count, "status", "completed")
}
