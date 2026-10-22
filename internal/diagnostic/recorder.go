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
	maxQueuedEvents = 1024
	maxQueuedBytes  = 16 << 20
	maxEventBytes   = 16 << 10
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

// RecorderStats의 대기량은 쓰는 중인 한 이벤트도 포함한다.
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
	run    func() error
	ticks  <-chan time.Time
	stop   func()
	cancel func()
}

type queueLimits struct{ events, bytes, eventBytes int }
type queuedLine struct {
	kind Kind
	data []byte
}

// Recorder는 인스턴스 하나의 공용 대기열과 단일 기록 루프를 소유한다.
// 복사하지 않는다. 요청 context·Event·호출자 goroutine은 보관하지 않는다.
type Recorder struct {
	source              Source
	limits              queueLimits
	writeLine           func(Kind, []byte) error
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

func startRecorder(source Source, limits queueLimits, writeLine func(Kind, []byte) error, closeFile func() error, upkeep *recorderUpkeep, output *failureOutput, budget time.Duration) *Recorder {
	recorder := &Recorder{source: source, limits: limits, writeLine: writeLine, closeFile: closeFile,
		wake: make(chan struct{}, 1), done: make(chan struct{}), queue: make([]queuedLine, limits.events), upkeep: upkeep,
		output: output, flushTime: budget, deadline: make(chan struct{})}
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
		item := recorder.queue[recorder.head]
		recorder.writing = true
		recorder.mu.Unlock()

		err := recorder.writeLine(item.kind, item.data)

		recorder.mu.Lock()
		if recorder.expired {
			recorder.writing = false
			recorder.mu.Unlock()
			continue
		}
		recorder.queue[recorder.head] = queuedLine{}
		recorder.head = (recorder.head + 1) % len(recorder.queue)
		recorder.count--
		recorder.bytes -= len(item.data)
		recorder.writing = false
		if err != nil {
			recorder.lossLocked(item.kind, writeFailed, failureCode(err))
			if recorder.firstErr == nil {
				recorder.firstErr = err
			}
		} else {
			recorder.written++
		}
		recorder.mu.Unlock()
	}
}

func (recorder *Recorder) maintain() {
	if err := recorder.upkeep.run(); err != nil {
		recorder.mu.Lock()
		recorder.maintenanceFailures++
		recorder.output.add(System, failureCode(err), false)
		if recorder.firstErr == nil {
			recorder.firstErr = err
		}
		recorder.mu.Unlock()
	}
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
