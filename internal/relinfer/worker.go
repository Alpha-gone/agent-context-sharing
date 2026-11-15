// Package relinfer는 서버 관계 추론 작업의 회차와 제공자 확보 차단을 관리한다.
package relinfer

import (
	"agent_context_sharing/internal/diagnostic"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"

	"agent_context_sharing/internal/model"
)

// Queue는 제공자 호출 전에 리스를 커밋하고 실패를 같은 세대에만 기록한다.
type Queue interface {
	ClaimRelationInferenceTask(context.Context, time.Duration) (model.RelationInferenceTask, bool, error)
	FailRelationInferenceTask(context.Context, model.RelationInferenceTask, string, bool, int) (bool, error)
	DeferRelationInferenceTask(context.Context, model.RelationInferenceTask, time.Time) (bool, error)
}

// Failure는 저장 가능한 고정 사유와 제공자 장애 여부를 구분한다.
type Failure struct {
	Reason                                 string
	Retryable, OutputInvalid, ProviderWide bool
	RetryAfter                             time.Duration
	ProviderStatus                         int
	WaitUntil                              *time.Time
}

type processingError struct{ cause error }

func (err processingError) Error() string { return "관계 추론 처리 실패" }
func (err processingError) Unwrap() error { return err.cause }

// Processor는 입력을 한 번 준비한다. Process의 성공은 판정 반영·기록·작업 삭제를
// 같은 트랜잭션에서 완료했음을 뜻한다. 실제 후보·판정 구현은 RINF-03·04에서 주입한다.
type Processor interface {
	Prepare(context.Context, model.RelationInferenceTask) ([]byte, *Failure, error)
	Process(context.Context, model.RelationInferenceTask, []byte, int) (*Failure, error)
	Probe(context.Context) (retryAfter time.Duration, err error)
}

type Worker struct {
	queue               Queue
	processor           Processor
	timeout             time.Duration
	mu                  sync.Mutex
	failures            int
	retryAt             time.Time
	now                 func() time.Time
	startOnce, stopOnce sync.Once
	cancel              context.CancelFunc
	done                chan struct{}
	client              *http.Client
	logger              *slog.Logger
	diagnostics         diagnostic.Observer
}

// New는 실제 제공자나 작업을 만들지 않고 회차 의존성만 검증한다.
func New(queue Queue, processor Processor, timeout time.Duration) (*Worker, error) {
	if queue == nil || processor == nil || timeout <= 0 || timeout > time.Duration(1<<62-1) {
		return nil, fmt.Errorf("관계 추론 작업자 구성이 올바르지 않다")
	}
	return &Worker{queue: queue, processor: processor, timeout: timeout, now: time.Now, done: make(chan struct{})}, nil
}

// RunOnce는 차단 중 탐침을 먼저 확인하고 사용 가능한 작업을 최대 하나 처리한다.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !w.mu.TryLock() {
		return false, nil
	}
	defer w.mu.Unlock()
	if !w.retryAt.IsZero() {
		if w.now().Before(w.retryAt) {
			return false, nil
		}
		probeCtx, cancel := context.WithTimeout(ctx, w.timeout)
		retryAfter, err := w.processor.Probe(probeCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			w.block(retryAfter)
			return false, nil
		}
		w.failures, w.retryAt = 0, time.Time{}
		diagnostic.Observe(w.diagnostics, ctx, "INFO", "relation_inference_retry", "completed", "provider_resumed", false, diagnostic.Fields{})
	}
	task, found, err := w.queue.ClaimRelationInferenceTask(ctx, w.timeout*2)
	if err != nil || !found {
		return false, err
	}
	ctx = diagnostic.ResumeJob(ctx, task.CorrelationID)
	diagnostic.Observe(w.diagnostics, ctx, "INFO", "relation_inference_acquire", "completed", "completed", true, diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: task.GraphID.String(), ContextID: task.ContextID.String()}, Index: diagnostic.IndexFields{TaskID: task.ID.String(), Generation: uint64(task.Generation.UnixMicro()), Attempts: uint64(task.Attempts), LeaseResult: "acquired"}})
	prepareCtx, cancel := context.WithTimeout(ctx, w.timeout)
	input, failure, processErr := w.processor.Prepare(prepareCtx, task)
	cancel()
	if failure != nil && failure.OutputInvalid {
		failure = &Failure{Reason: "candidate_failed", Retryable: true}
	}
	if processErr == nil && failure == nil && len(input) == 0 {
		failure = &Failure{Reason: "candidate_failed", Retryable: true}
	}
	input = bytes.Clone(input)
	outputFailures := 0
	attempted := false
	if failure == nil && processErr == nil {
		for attempt := 0; attempt < 2; attempt++ {
			attempted = true
			callCtx, cancel := context.WithTimeout(ctx, w.timeout)
			failure, processErr = w.processor.Process(callCtx, task, bytes.Clone(input), attempt+1)
			cancel()
			if failure != nil && failure.OutputInvalid {
				outputFailures++
			}
			if failure == nil || !failure.OutputInvalid || processErr != nil {
				break
			}
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
		}
	}
	if processErr != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if failure == nil || failure.OutputInvalid {
			failure = &Failure{Reason: "result_failed", Retryable: true}
			if _, network := errors.AsType[net.Error](processErr); attempted && (network || errors.Is(processErr, context.DeadlineExceeded)) {
				failure.Reason, failure.ProviderWide = "provider_unavailable", true
			} else if !attempted {
				failure.Reason = "candidate_failed"
			}
		}
	}
	if failure == nil {
		return true, nil
	}
	if failure.WaitUntil != nil {
		if attempted || processErr != nil {
			return true, fmt.Errorf("추론 색인 대기 상태가 올바르지 않다")
		}
		_, err := w.queue.DeferRelationInferenceTask(ctx, task, *failure.WaitUntil)
		return true, err
	}
	classified := *failure
	failure = &classified
	if failure.OutputInvalid {
		failure.Reason = "output_invalid"
	}
	if !failure.OutputInvalid {
		switch status := failure.ProviderStatus; {
		case status == 401 || status == 403:
			failure.Retryable, failure.ProviderWide = false, true
		case status == 408 || status == 429 || (status >= 500 && status < 600):
			failure.Retryable, failure.ProviderWide = true, true
		case status >= 400:
			failure.Retryable = false
		}
	}
	if failure.ProviderWide {
		w.block(failure.RetryAfter)
		diagnostic.Observe(w.diagnostics, ctx, "WARN", "relation_inference_retry", "pending", "provider_blocked", false, diagnostic.Fields{})
	}
	matched, err := w.queue.FailRelationInferenceTask(ctx, task, failure.Reason, failure.Retryable, outputFailures)
	reason := "store_unavailable"
	switch failure.Reason {
	case "provider_unavailable":
		reason = "provider_unavailable"
	case "provider_rejected":
		reason = "provider_contract_violation"
	case "output_invalid":
		reason = "output_invalid"
	}
	if err == nil && !matched {
		reason = "stale_generation"
	}
	diagnostic.Observe(w.diagnostics, ctx, "WARN", "relation_inference_retry", "failed", reason, true, diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: task.GraphID.String(), ContextID: task.ContextID.String()}, Index: diagnostic.IndexFields{TaskID: task.ID.String(), Attempts: uint64(task.Attempts)}})
	if err != nil {
		if processErr != nil {
			return true, errors.Join(processingError{processErr}, err)
		}
		return true, err
	}
	if processErr != nil {
		return true, processingError{processErr}
	}
	return true, nil
}

func (w *Worker) block(retryAfter time.Duration) {
	w.failures = min(w.failures+1, 5)
	ceiling := min(time.Minute*time.Duration(1<<(w.failures-1)), 15*time.Minute)
	delay := ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)+1))
	delay = max(delay, min(max(retryAfter, 0), 24*time.Hour))
	w.retryAt = w.now().Add(delay)
}
