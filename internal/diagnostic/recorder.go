//go:build linux || darwin

package diagnostic

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"sync"
	"time"
)

const (
	maxBatchEvents         = 128
	maxBatchBytes          = 256 << 10
	maxQueuedEvents        = 1024
	maxQueuedBytes         = 16 << 20
	maxEventBytes          = 16 << 10
	maintenanceGuardBudget = time.Second
)

type lossReason uint8

const (
	invalidEvent lossReason = iota
	sourceMismatch
	encodingFailed
	eventTooLarge
	queueFull
	recorderClosed
	writeFailed
	shutdownTimeout
	lossReasonCount
)

var lossNames = [...]string{"invalid_event", "source_mismatch", "encoding_failed", "event_too_large", "queue_full", "closed", "write_failed", "shutdown_timeout"}

// DropCount는 원문·임의 오류 없이 종류와 고정 원인별 누적 누락을 나타낸다.
// 미지원 종류를 입력한 이벤트의 Kind는 빈 문자열이다.
type DropCount struct {
	Kind   Kind
	Reason string
	Count  uint64
}

// RecorderStats의 대기량은 쓰는 중인 일괄 기록도 포함한다.
// Written은 완전한 행의 append·sync 성공이며 업무 감사 성공을 뜻하지 않는다.
type RecorderStats struct {
	Accepted            uint64
	Written             uint64
	Dropped             uint64
	QueuedEvents        int
	QueuedBytes         int
	Writing             bool
	Losses              []DropCount
	MaintenanceFailures uint64
	StderrFailures      uint64
}

type recorderUpkeep struct {
	cancelMaintenance context.CancelFunc
	context           context.Context
	scope             int64
	removed           func() uint64
	run               func() error
	ticks             <-chan time.Time
	stop              func()
	cancel            func()
}

type queueLimits struct{ events, bytes, eventBytes int }
type queuedLine struct {
	kind Kind
	data []byte
}

// Recorder는 인스턴스 하나의 공용 대기열과 단일 기록 루프를 소유한다.
// 복사하지 않는다. 요청 context·Event·호출자 goroutine은 보관하지 않는다.
type Recorder struct {
	maintenanceStopped  bool
	maintenanceDone     chan struct{}
	maintenanceErr      error
	maintenanceGuard    MaintenanceGuard
	source              Source
	limits              queueLimits
	writeLine           func(Kind, []byte) error
	writeBatch          func([]queuedLine) []error
	closeFile           func() error
	wake                chan struct{}
	done                chan struct{}
	mu                  sync.Mutex
	queue               []queuedLine
	head                int
	count               int
	bytes               int
	stopping            bool
	writing             bool
	accepted            uint64
	written             uint64
	dropped             uint64
	losses              [len(fileKinds) + 1][lossReasonCount]uint64
	firstErr            error
	closeErr            error
	upkeep              *recorderUpkeep
	maintenanceFailures uint64
	output              *failureOutput
	flushTime           time.Duration
	flushTimer          *time.Timer
	deadline            chan struct{}
	expired             bool
}

func newRecorder(source Source, limits queueLimits, writeLine func(Kind, []byte) error, closeFile func() error, upkeep *recorderUpkeep) *Recorder {
	return startRecorder(source, limits, writeLine, closeFile, upkeep, newFailureOutput(source, os.Stderr, time.Now), flushBudget)
}

func startRecorder(source Source, limits queueLimits, writeLine func(Kind, []byte) error, closeFile func() error, upkeep *recorderUpkeep, output *failureOutput, budget time.Duration, batch ...func([]queuedLine) []error) *Recorder {
	recorder := &Recorder{source: source, limits: limits, writeLine: writeLine, closeFile: closeFile,
		wake: make(chan struct{}, 1), done: make(chan struct{}), queue: make([]queuedLine, limits.events), upkeep: upkeep,
		output: output, flushTime: budget, deadline: make(chan struct{})}
	if len(batch) != 0 {
		recorder.writeBatch = batch[0]
	}
	go recorder.run()
	return recorder
}

type encodedEvent struct {
	Event
	FieldsOmitted bool `json:"fields_omitted,omitzero"`
}

// encodeLine의 제한은 개행까지 포함한다. 검증은 선택 필드 축소보다 먼저 하여
// 잘못된 입력을 생략으로 숨겨 유효 이벤트로 바꾸지 않는다.
func encodeLine(event Event, limit int) ([]byte, lossReason, bool) {
	if event.Validate() != nil {
		return nil, invalidEvent, false
	}
	wire := encodedEvent{Event: event}
	data, err := json.Marshal(&wire)
	if err != nil {
		return nil, encodingFailed, false
	}
	if len(data)+1 > limit && event.Fields != (Fields{}) {
		wire.Fields = Fields{}
		wire.FieldsOmitted = true
		data, err = json.Marshal(&wire)
		if err != nil {
			return nil, encodingFailed, false
		}
	}
	if len(data)+1 > limit {
		return nil, eventTooLarge, false
	}
	// 큐가 호출자 값이나 인코더의 여유 버퍼를 붙잡지 않게 정확한 길이로 소유한다.
	line := make([]byte, len(data)+1)
	copy(line, data)
	line[len(data)] = '\n'
	return line, 0, true
}

// Emit은 파일 I/O를 기다리지 않는다. ctx의 요청 취소와 무관하게 값만 기록하여
// 커밋 후 취소도 진단할 수 있으며 ctx 자체는 보관하지 않는다.
// true는 대기열 수락이고 영속 저장 성공은 Stats와 Close로 확인한다.
func (recorder *Recorder) Emit(_ context.Context, event Event) bool {
	line, reason, ok := encodeLine(event, recorder.limits.eventBytes)
	if !ok {
		recorder.drop(event.Kind, reason)
		return false
	}
	if event.InstanceID != recorder.source.instance || event.BootID != recorder.source.boot {
		recorder.drop(event.Kind, sourceMismatch)
		return false
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.stopping {
		recorder.dropLocked(event.Kind, recorderClosed)
		return false
	}
	if recorder.count == len(recorder.queue) || len(line) > recorder.limits.bytes-recorder.bytes {
		recorder.dropLocked(event.Kind, queueFull)
		return false
	}
	recorder.queue[(recorder.head+recorder.count)%len(recorder.queue)] = queuedLine{kind: event.Kind, data: line}
	recorder.count++
	recorder.bytes += len(line)
	recorder.accepted++
	recorder.signal()
	return true
}

func (recorder *Recorder) signal() {
	select {
	case recorder.wake <- struct{}{}:
	default:
	}
}

func (recorder *Recorder) drop(kind Kind, reason lossReason) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.dropLocked(kind, reason)
}

func (recorder *Recorder) dropLocked(kind Kind, reason lossReason) {
	recorder.lossLocked(kind, reason, lossNames[reason])
}

func (recorder *Recorder) lossLocked(kind Kind, reason lossReason, code string) {
	recorder.dropped++
	recorder.losses[kindIndex(kind)][reason]++
	recorder.output.add(kind, code, true)
}

func (recorder *Recorder) Stats() RecorderStats {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	stats := RecorderStats{Accepted: recorder.accepted, Written: recorder.written, Dropped: recorder.dropped,
		QueuedEvents: recorder.count, QueuedBytes: recorder.bytes, Writing: recorder.writing, MaintenanceFailures: recorder.maintenanceFailures}
	recorder.output.mu.Lock()
	stats.StderrFailures = recorder.output.outputFailures
	recorder.output.mu.Unlock()
	for i, counts := range recorder.losses {
		kind := Kind("")
		if i < len(fileKinds) {
			kind = fileKinds[i]
		}
		for reason, count := range counts {
			if count != 0 {
				stats.Losses = append(stats.Losses, DropCount{Kind: kind, Reason: lossNames[reason], Count: count})
			}
		}
	}
	return stats
}

func (recorder *Recorder) run() {
	defer close(recorder.done)
	var ticks <-chan time.Time
	if recorder.upkeep != nil {
		ticks = recorder.upkeep.ticks
		defer recorder.upkeep.stop()
	}
	for {
		select {
		case <-ticks:
			recorder.maintain()
		default:
		}
		recorder.mu.Lock()
		if recorder.count == 0 {
			stopping := recorder.stopping
			recorder.mu.Unlock()
			if stopping {
				err := recorder.closeFile()
				recorder.mu.Lock()
				recorder.closeErr = err
				if err != nil {
					recorder.output.add(System, failureCode(err), false)
				}
				recorder.mu.Unlock()
				recorder.output.stop()
				return
			}
			select {
			case <-recorder.wake:
			case <-ticks:
				recorder.maintain()
			}
			continue
		}
		limit := 1
		if recorder.writeBatch != nil {
			limit = min(recorder.count, maxBatchEvents)
		}
		batch := make([]queuedLine, 0, limit)
		bytes := 0
		for i := range limit {
			item := recorder.queue[(recorder.head+i)%len(recorder.queue)]
			if bytes+len(item.data) > maxBatchBytes {
				break
			}
			batch = append(batch, item)
			bytes += len(item.data)
		}
		recorder.writing = true
		recorder.mu.Unlock()

		var results []error
		if recorder.writeBatch != nil {
			results = recorder.writeBatch(batch)
		} else {
			results = []error{recorder.writeLine(batch[0].kind, batch[0].data)}
		}

		recorder.mu.Lock()
		if recorder.expired {
			recorder.writing = false
			recorder.mu.Unlock()
			continue
		}
		for i, item := range batch {
			recorder.queue[recorder.head] = queuedLine{}
			recorder.head = (recorder.head + 1) % len(recorder.queue)
			recorder.count--
			recorder.bytes -= len(item.data)
			if err := results[i]; err != nil {
				recorder.lossLocked(item.kind, writeFailed, failureCode(err))
				if recorder.firstErr == nil {
					recorder.firstErr = err
				}
			} else {
				recorder.written++
			}
		}
		recorder.writing = false
		recorder.mu.Unlock()
	}
}

func (recorder *Recorder) maintain() {
	recorder.mu.Lock()
	if recorder.stopping || recorder.maintenanceStopped {
		recorder.mu.Unlock()
		return
	}
	done := make(chan struct{})
	recorder.maintenanceDone = done
	guard := recorder.maintenanceGuard
	recorder.mu.Unlock()
	defer func() {
		recorder.mu.Lock()
		close(done)
		recorder.maintenanceDone = nil
		recorder.mu.Unlock()
	}()
	run := func() (int, error) {
		var before uint64
		if recorder.upkeep.removed != nil {
			before = recorder.upkeep.removed()
		}
		err := recorder.upkeep.run()
		if recorder.upkeep.removed != nil {
			return int(recorder.upkeep.removed() - before), err
		}
		return 0, err
	}
	var err error
	if guard != nil {
		// DB 예약·연결 획득 대기가 단일 기록 루프를 무기한 막지 않게 한다.
		ctx, cancel := context.WithTimeout(recorder.upkeep.context, maintenanceGuardBudget)
		defer cancel()
		_, _, err = guard(ctx, recorder.upkeep.scope, run)
	} else {
		_, err = run()
	}
	if err != nil {
		recorder.mu.Lock()
		if recorder.maintenanceStopped && onlyMaintenanceCancellation(err) {
			recorder.mu.Unlock()
			return
		}
		recorder.maintenanceFailures++
		if recorder.maintenanceErr == nil {
			recorder.maintenanceErr = err
		}
		recorder.output.add(System, failureCode(err), false)
		if recorder.firstErr == nil {
			recorder.firstErr = err
		}
		recorder.mu.Unlock()
	}
}

// 취소와 잠금 해제/파일 오류가 함께 반환되면 취소만으로 실패를 숨기지 않는다.
func onlyMaintenanceCancellation(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyMaintenanceCancellation(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyMaintenanceCancellation(wrapped.Unwrap())
	}
	return errors.Is(err, context.Canceled)
}

// MaintenanceGuard는 파일 정리의 실행권을 저장소·인스턴스 범위로 주입한다.
// context의 마감·취소를 예약과 연결 획득에 전달해야 한다.
// callback의 건수는 제거한 회전 파일 수이며 실제 파일 잠금은 계속 적용된다.
type MaintenanceGuard func(context.Context, int64, func() (int, error)) (int, bool, error)

// SetMaintenanceGuard는 DB 조립 뒤 시간 기반 정리에만 실행권을 연결한다.
// 기동·회전·새 기록의 정책 검사와 단일 기록 루프의 소유권은 유지한다.
func (recorder *Recorder) SetMaintenanceGuard(guard MaintenanceGuard) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.maintenanceGuard = guard
}

// StopMaintenance는 DB 풀 종료 전에 시간 기반 정리를 취소하고 진행 회차를 기다린다.
// 새 기록·파일 정책·flush는 계속 사용할 수 있으며 다시 시작하지 않는다.
func (recorder *Recorder) StopMaintenance(ctx context.Context) error {
	recorder.mu.Lock()
	recorder.maintenanceStopped = true
	done := recorder.maintenanceDone
	var cancel context.CancelFunc
	if recorder.upkeep != nil {
		cancel = recorder.upkeep.cancelMaintenance
	}
	recorder.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.maintenanceErr
}

// Close는 최초 호출부터 최대 5초 동안 flush한다. 호출자 취소는 그 호출의
// 대기만 끝내며 공통 마감을 다시 시작하지 않는다. OS I/O 정리는 소유 루프가 마친다.
func (recorder *Recorder) Close(ctx context.Context) error {
	recorder.mu.Lock()
	if !recorder.stopping {
		recorder.stopping = true
		recorder.flushTimer = time.AfterFunc(recorder.flushTime, recorder.expire)
	}
	recorder.signal()
	recorder.mu.Unlock()
	select {
	case <-recorder.done:
		select {
		case <-recorder.output.done:
			recorder.flushTimer.Stop()
			return recorder.finishError()
		case <-recorder.deadline:
			return recorder.finishError()
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-recorder.deadline:
		return recorder.finishError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (recorder *Recorder) expire() {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.expired {
		return
	}
	select {
	case <-recorder.done:
		select {
		case <-recorder.output.done:
			return
		default:
		}
	default:
	}
	recorder.expired = true
	if recorder.upkeep != nil && recorder.upkeep.cancel != nil {
		recorder.upkeep.cancel()
	}
	for i := range recorder.count {
		index := (recorder.head + i) % len(recorder.queue)
		recorder.dropLocked(recorder.queue[index].kind, shutdownTimeout)
		recorder.queue[index] = queuedLine{}
	}
	recorder.count, recorder.bytes = 0, 0
	recorder.firstErr = errors.Join(recorder.firstErr, fileFailure("shutdown_timeout", context.DeadlineExceeded))
	recorder.output.add(System, "shutdown_timeout", false)
	recorder.output.stop()
	close(recorder.deadline)
	recorder.signal()
}
