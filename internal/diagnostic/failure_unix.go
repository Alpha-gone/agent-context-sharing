//go:build linux || darwin

package diagnostic

import (
	"encoding/json/v2"
	"errors"
	"io"
	"slices"
	"sync"
	"time"
)

const failureInterval = 30 * time.Second
const flushBudget = 5 * time.Second

// 외부 FileError.Code도 그대로 출력하지 않는다. 배열 밖의 값은 고정 코드로 축소한다.
var failureCodes = [...]string{
	"invalid_event", "source_mismatch", "encoding_failed", "event_too_large", "queue_full", "closed", "write_failed",
	"canceled", "close_failed", "directory_unavailable", "file_invalid", "file_unavailable", "instance_busy",
	"ownership_invalid", "parent_unsafe", "path_changed", "path_invalid", "permissions_invalid",
	"storage_lock_failed", "storage_lock_timeout", "storage_unlock_failed", "sync_failed",
	"filesystem_unavailable", "storage_read_only", "volatile_storage", "event_expired", "log_invalid", "partial_write",
	"active_file_protected", "capacity_exceeded", "cleanup_capacity", "cleanup_failed", "cleanup_write_failed",
	"event_capacity", "inventory_failed", "inventory_invalid", "inventory_limit", "rotation_failed", "writer_lock_failed",
	"partial_recovered", "shutdown_timeout",
}

func failureCode(err error) string {
	if failure, ok := errors.AsType[*FileError](err); ok && slices.Contains(failureCodes[:], failure.Code) {
		return failure.Code
	}
	return "write_failed"
}

func kindIndex(kind Kind) int {
	if i := slices.Index(fileKinds[:], kind); i >= 0 {
		return i
	}
	return len(fileKinds)
}

type failureCount struct {
	Kind  Kind   `json:"kind"`
	Count uint64 `json:"count"`
}

type failureRow struct {
	Time     time.Time        `json:"time"`
	Instance string           `json:"instance_id"`
	Boot     string           `json:"boot_id"`
	Reason   string           `json:"reason_code"`
	Counts   []failureCount   `json:"counts,omitempty"`
	Dropped  uint64           `json:"dropped"`
	Failures uint64           `json:"failures"`
	Summary  []failureSummary `json:"summary,omitempty"`
}

type failureSummary struct {
	Reason string         `json:"reason_code"`
	Counts []failureCount `json:"counts"`
}

type failureState struct {
	counts        [len(fileKinds) + 1]uint64
	version, sent uint64
	last          time.Time
}

// 누적 배열과 한 출력 작업자만 사용한다. 막힌 stderr마다 goroutine을 만들지 않는다.
// 성공한 Write도 운영 수집기의 보관을 증명하지는 않는다.
type failureOutput struct {
	mu                                sync.Mutex
	source                            Source
	writer                            io.Writer
	now                               func() time.Time
	states                            [len(failureCodes)]failureState
	dropped, failures, outputFailures uint64
	stopping                          bool
	wake, done                        chan struct{}
}

func newFailureOutput(source Source, writer io.Writer, now func() time.Time) *failureOutput {
	if !validInstance(source.instance) || !validID(source.boot) {
		source = Source{}
	}
	output := &failureOutput{source: source, writer: writer, now: now, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go output.run()
	return output
}

func (output *failureOutput) signal() {
	select {
	case output.wake <- struct{}{}:
	default:
	}
}

func (output *failureOutput) add(kind Kind, code string, dropped bool) {
	index := slices.Index(failureCodes[:], code)
	if index < 0 {
		index = slices.Index(failureCodes[:], "write_failed")
	}
	output.mu.Lock()
	state := &output.states[index]
	state.counts[kindIndex(kind)]++
	state.version++
	output.failures++
	if dropped {
		output.dropped++
	}
	output.mu.Unlock()
	output.signal()
}

func failureCounts(counts [len(fileKinds) + 1]uint64) []failureCount {
	var result []failureCount
	for index, count := range counts {
		if count == 0 {
			continue
		}
		kind := Kind("")
		if index < len(fileKinds) {
			kind = fileKinds[index]
		}
		result = append(result, failureCount{kind, count})
	}
	return result
}

func (output *failureOutput) stop() {
	output.mu.Lock()
	output.stopping = true
	output.mu.Unlock()
	output.signal()
}

// 종료 요약은 고정 원인 shutdown 한 행이다. 상세 알림의 30초 제한을 풀지 않는다.
func (output *failureOutput) pending(now time.Time) ([]failureRow, bool) {
	output.mu.Lock()
	defer output.mu.Unlock()
	base := failureRow{Time: now.UTC(), Instance: output.source.instance, Boot: output.source.boot, Dropped: output.dropped, Failures: output.failures}
	var rows []failureRow
	var summary []failureSummary
	for index := range output.states {
		state := &output.states[index]
		if state.version == 0 {
			continue
		}
		counts := failureCounts(state.counts)
		if output.stopping {
			summary = append(summary, failureSummary{failureCodes[index], counts})
		}
		if state.sent == state.version || !state.last.IsZero() && now.Sub(state.last) < failureInterval {
			continue
		}
		row := base
		row.Reason, row.Counts = failureCodes[index], counts
		rows = append(rows, row)
		state.last, state.sent = now, state.version
	}
	if output.stopping && len(summary) != 0 {
		base.Reason, base.Summary = "shutdown", summary
		rows = append(rows, base)
	}
	return rows, output.stopping
}

func (output *failureOutput) run() {
	defer close(output.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-output.wake:
		case <-ticker.C:
		}
		rows, stop := output.pending(output.now())
		for _, row := range rows {
			data, err := json.Marshal(row)
			if err == nil {
				data = append(data, '\n')
				var n int
				n, err = output.writer.Write(data)
				if n != len(data) && err == nil {
					err = io.ErrShortWrite
				}
			}
			if err != nil {
				output.mu.Lock()
				output.outputFailures++
				output.mu.Unlock()
			}
		}
		if stop {
			return
		}
	}
}
