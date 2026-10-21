//go:build linux || darwin

package diagnostic

import (
	"context"
	"encoding/json/v2"
	"sync"
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
	lossReasonCount
)

var lossNames = [...]string{"invalid_event", "source_mismatch", "encoding_failed", "event_too_large", "queue_full", "closed", "write_failed"}

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
	Accepted     uint64
	Written      uint64
	Dropped      uint64
	QueuedEvents int
	QueuedBytes  int
	Writing      bool
	Losses       []DropCount
}

type queueLimits struct{ events, bytes, eventBytes int }
type queuedLine struct {
	kind Kind
	data []byte
}

// Recorder는 인스턴스 하나의 공용 대기열과 단일 기록 루프를 소유한다.
// 복사하지 않는다. 요청 context·Event·호출자 goroutine은 보관하지 않는다.
type Recorder struct {
	source    Source
	limits    queueLimits
	writeLine func(Kind, []byte) error
	closeFile func() error
	wake      chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	queue     []queuedLine
	head      int
	count     int
	bytes     int
	stopping  bool
	writing   bool
	accepted  uint64
	written   uint64
	dropped   uint64
	losses    [len(fileKinds) + 1][lossReasonCount]uint64
	firstErr  error
	closeErr  error
}

func newRecorder(source Source, limits queueLimits, writeLine func(Kind, []byte) error, closeFile func() error) *Recorder {
	recorder := &Recorder{source: source, limits: limits, writeLine: writeLine, closeFile: closeFile,
		wake: make(chan struct{}, 1), done: make(chan struct{}), queue: make([]queuedLine, limits.events)}
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
	index := len(fileKinds)
	for i, candidate := range fileKinds {
		if kind == candidate {
			index = i
			break
		}
	}
	recorder.dropped++
	recorder.losses[index][reason]++
}

func (recorder *Recorder) Stats() RecorderStats {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	stats := RecorderStats{Accepted: recorder.accepted, Written: recorder.written, Dropped: recorder.dropped,
		QueuedEvents: recorder.count, QueuedBytes: recorder.bytes, Writing: recorder.writing}
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
	for {
		recorder.mu.Lock()
		if recorder.count == 0 {
			stopping := recorder.stopping
			recorder.mu.Unlock()
			if stopping {
				recorder.closeErr = recorder.closeFile()
				return
			}
			<-recorder.wake
			continue
		}
		item := recorder.queue[recorder.head]
		recorder.writing = true
		recorder.mu.Unlock()

		err := recorder.writeLine(item.kind, item.data)

		recorder.mu.Lock()
		recorder.queue[recorder.head] = queuedLine{}
		recorder.head = (recorder.head + 1) % len(recorder.queue)
		recorder.count--
		recorder.bytes -= len(item.data)
		recorder.writing = false
		if err != nil {
			recorder.dropLocked(item.kind, writeFailed)
			if recorder.firstErr == nil {
				recorder.firstErr = err
			}
		} else {
			recorder.written++
		}
		recorder.mu.Unlock()
	}
}

// Close는 신규 수락을 막고 기존 대기열과 파일 수명 정리를 기다린다.
// ctx 만료는 호출자의 대기만 끝낸다. 작업자는 소유한 자원 정리를 계속한다.
// 5초 flush·잔여 누락·stderr 통지는 OBS-07의 종료 정책에서 구현한다.
func (recorder *Recorder) Close(ctx context.Context) error {
	recorder.mu.Lock()
	recorder.stopping = true
	recorder.signal()
	recorder.mu.Unlock()
	select {
	case <-recorder.done:
		return recorder.finishError()
	default:
	}
	select {
	case <-recorder.done:
		return recorder.finishError()
	case <-ctx.Done():
		return ctx.Err()
	}
}
