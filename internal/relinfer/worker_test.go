package relinfer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type queueStub struct {
	claims, failures int
	lease            time.Duration
	task             model.RelationInferenceTask
	retryable        bool
	outputFailures   int
	reason           string
	deferred         int
}

func (q *queueStub) DeferRelationInferenceTask(_ context.Context, _ model.RelationInferenceTask, _ time.Time) (bool, error) {
	q.deferred++
	return true, nil
}

func (q *queueStub) ClaimRelationInferenceTask(_ context.Context, lease time.Duration) (model.RelationInferenceTask, bool, error) {
	q.claims++
	q.lease = lease
	return q.task, true, nil
}
func (q *queueStub) FailRelationInferenceTask(_ context.Context, _ model.RelationInferenceTask, reason string, retryable bool, outputFailures int) (bool, error) {
	q.failures++
	q.retryable = retryable
	q.outputFailures = outputFailures
	q.reason = reason
	return true, nil
}

func TestWorkerProviderStatusPolicy(t *testing.T) {
	for _, sample := range []struct {
		status       int
		retry, block bool
	}{
		{401, false, true}, {403, false, true}, {408, true, true}, {429, true, true}, {503, true, true}, {400, false, false},
	} {
		queue := &queueStub{}
		failure := &Failure{Reason: "provider_rejected", ProviderStatus: sample.status, Retryable: true}
		worker, err := New(queue, &processorStub{failure: failure}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if queue.retryable != sample.retry || !worker.retryAt.IsZero() != sample.block || !failure.Retryable {
			t.Fatalf("제공자 상태 %d: retry=%v block=%v", sample.status, queue.retryable, !worker.retryAt.IsZero())
		}
	}
}

type processorStub struct {
	calls, probes int
	failure       *Failure
	probeError    error
	processError  error
	prepareError  error
}

func (p *processorStub) Prepare(context.Context, model.RelationInferenceTask) ([]byte, *Failure, error) {
	return []byte("synthetic input"), nil, p.prepareError
}

func TestWorkerCandidateFailureDoesNotBlockProvider(t *testing.T) {
	queue := &queueStub{}
	worker, err := New(queue, &processorStub{prepareError: context.DeadlineExceeded}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("후보 시간 초과=%v", err)
	}
	if queue.reason != "candidate_failed" || !queue.retryable || !worker.retryAt.IsZero() {
		t.Fatalf("후보 실패가 제공자를 차단했다: %+v", queue)
	}
}
func (p *processorStub) Process(context.Context, model.RelationInferenceTask, []byte, int) (*Failure, error) {
	p.calls++
	return p.failure, p.processError
}

type retryProcessor struct {
	inputs        []string
	attempts      []int
	failTwice     bool
	secondFailure *Failure
}

func (p *retryProcessor) Prepare(context.Context, model.RelationInferenceTask) ([]byte, *Failure, error) {
	return []byte("fixed input"), nil, nil
}
func (p *retryProcessor) Process(_ context.Context, _ model.RelationInferenceTask, input []byte, attempt int) (*Failure, error) {
	p.inputs = append(p.inputs, string(input))
	p.attempts = append(p.attempts, attempt)
	input[0] = 'X'
	if attempt == 2 && p.secondFailure != nil {
		return p.secondFailure, nil
	}
	if attempt == 1 || p.failTwice {
		return &Failure{OutputInvalid: true}, nil
	}
	return nil, nil
}

func TestWorkerRecordsFirstInvalidBeforeProviderFailure(t *testing.T) {
	queue := &queueStub{}
	processor := &retryProcessor{secondFailure: &Failure{Reason: "provider_unavailable", ProviderStatus: 429}}
	worker, err := New(queue, processor, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if queue.failures != 1 || queue.outputFailures != 1 || !queue.retryable || queue.reason != "provider_unavailable" || worker.retryAt.IsZero() {
		t.Fatalf("출력 위반 후 제공자 실패=%+v", queue)
	}
}
func (*retryProcessor) Probe(context.Context) (time.Duration, error) { return 0, nil }

func TestWorkerRetriesIdenticalInputOnce(t *testing.T) {
	for _, twice := range []bool{false, true} {
		queue, processor := &queueStub{}, &retryProcessor{failTwice: twice}
		worker, err := New(queue, processor, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(processor.inputs) != 2 || processor.inputs[0] != "fixed input" || processor.inputs[1] != "fixed input" || processor.attempts[0] != 1 || processor.attempts[1] != 2 {
			t.Fatalf("동일 입력 재시도=%v/%v", processor.inputs, processor.attempts)
		}
		if twice && (queue.failures != 1 || queue.outputFailures != 2) || !twice && queue.failures != 0 {
			t.Fatalf("출력 실패=%d/%d", queue.failures, queue.outputFailures)
		}
	}
}

type cancelRetryProcessor struct {
	cancel context.CancelFunc
	calls  int
}

func (*cancelRetryProcessor) Prepare(context.Context, model.RelationInferenceTask) ([]byte, *Failure, error) {
	return []byte("input"), nil, nil
}
func (p *cancelRetryProcessor) Process(context.Context, model.RelationInferenceTask, []byte, int) (*Failure, error) {
	p.calls++
	p.cancel()
	return &Failure{OutputInvalid: true}, nil
}
func (*cancelRetryProcessor) Probe(context.Context) (time.Duration, error) { return 0, nil }

func TestWorkerCancellationStopsOutputRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queue := &queueStub{}
	processor := &cancelRetryProcessor{cancel: cancel}
	worker, err := New(queue, processor, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); !errors.Is(err, context.Canceled) || processor.calls != 1 || queue.failures != 0 {
		t.Fatalf("재시도 전 취소=%v, 호출=%d, 실패 기록=%d", err, processor.calls, queue.failures)
	}
}

func TestWorkerPreservesFailureWithoutExposingDetails(t *testing.T) {
	cause := errors.New("synthetic-secret")
	queue := &queueStub{}
	worker, err := New(queue, &processorStub{processError: cause}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(t.Context()); !errors.Is(err, cause) || strings.Contains(err.Error(), "synthetic-secret") || queue.failures != 1 {
		t.Fatalf("처리 실패 보존과 비식별: %v", err)
	}
}

func TestWorkerTimeoutBlocksProvider(t *testing.T) {
	queue := &queueStub{}
	worker, err := New(queue, &processorStub{processError: context.DeadlineExceeded}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("시간 초과=%v", err)
	}
	if found, err := worker.RunOnce(t.Context()); err != nil || found || queue.claims != 1 {
		t.Fatalf("시간 초과 뒤 확보=%v, %v", found, err)
	}
}
func (p *processorStub) Probe(context.Context) (time.Duration, error) {
	p.probes++
	return 0, p.probeError
}

func TestWorkerBlocksAndProbesProvider(t *testing.T) {
	queue := &queueStub{}
	processor := &processorStub{failure: &Failure{Reason: "provider_unavailable", Retryable: true, ProviderWide: true}}
	worker, err := New(queue, processor, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	worker.now = func() time.Time { return now }
	if found, err := worker.RunOnce(t.Context()); err != nil || !found || queue.claims != 1 || queue.failures != 1 || queue.lease != time.Minute {
		t.Fatalf("최초 회차 found=%v claims=%d failures=%d: %v", found, queue.claims, queue.failures, err)
	}
	if found, err := worker.RunOnce(t.Context()); err != nil || found || queue.claims != 1 {
		t.Fatalf("차단 중 확보=%v: %v", found, err)
	}
	now = worker.retryAt
	processor.probeError = errors.New("provider")
	if found, err := worker.RunOnce(t.Context()); err != nil || found || queue.claims != 1 || processor.probes != 1 {
		t.Fatalf("탐침 실패=%v: %v", found, err)
	}
	now = worker.retryAt
	processor.probeError, processor.failure = nil, nil
	if found, err := worker.RunOnce(t.Context()); err != nil || !found || queue.claims != 2 || processor.probes != 2 {
		t.Fatalf("회복=%v: %v", found, err)
	}
}

func TestWorkerBusyAndCanceled(t *testing.T) {
	worker, err := New(&queueStub{}, &processorStub{}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if found, err := worker.RunOnce(t.Context()); err != nil || found {
		t.Fatalf("중복 회차=%v, %v", found, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := worker.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("취소=%v", err)
	}
}

type waitingProcessor struct {
	until time.Time
	calls int
}

func (p *waitingProcessor) Prepare(context.Context, model.RelationInferenceTask) ([]byte, *Failure, error) {
	return nil, &Failure{WaitUntil: &p.until}, nil
}
func (p *waitingProcessor) Process(context.Context, model.RelationInferenceTask, []byte, int) (*Failure, error) {
	p.calls++
	return nil, nil
}
func (*waitingProcessor) Probe(context.Context) (time.Duration, error) { return 0, nil }
func TestWorkerDefersIndexWaitWithoutFailure(t *testing.T) {
	q := &queueStub{}
	p := &waitingProcessor{until: time.Now().Add(time.Minute)}
	worker, err := New(q, p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if q.deferred != 1 || q.failures != 0 || p.calls != 0 {
		t.Fatalf("대기 처리: %+v calls=%d", q, p.calls)
	}
}
