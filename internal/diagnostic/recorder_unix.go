//go:build linux || darwin

package diagnostic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// NewRecorder는 파일 준비를 완료한 뒤 단일 기록 루프를 시작한다.
// Source는 같은 기동의 Reporter와 공유하고 Close로 파일·잠금을 반환한다.
// 회전·보존·총상한을 준비하고 유휴 상태에서도 매시간 정리한다.
// 실행 중 장애는 안전한 stderr 출력으로 알리고 종료 flush는 최대 5초다.
func NewRecorder(ctx context.Context, directory string, source Source) (*Recorder, error) {
	return prepareRecorder(ctx, directory, source, defaultStoragePolicy())
}

func prepareRecorder(ctx context.Context, directory string, source Source, policy *storagePolicy) (*Recorder, error) {
	return prepareRecorderOutput(ctx, directory, source, policy, newFailureOutput(source, os.Stderr, time.Now))
}

func prepareRecorderOutput(ctx context.Context, directory string, source Source, policy *storagePolicy, output *failureOutput) (_ *Recorder, resultErr error) {
	defer func() {
		if resultErr != nil {
			output.add(System, failureCode(resultErr), false)
			output.stop()
		}
	}()
	files, err := PrepareFiles(ctx, directory, source)
	if err != nil {
		return nil, err
	}
	files.policy = policy
	files.notice = func(kind Kind, code string) { output.add(kind, code, false) }
	files.ioContext, files.cancelIO = context.WithCancel(context.Background())
	if err := files.withStorageLock(ctx, func() error { return files.maintainLocked(policy.now().UTC()) }); err != nil {
		return nil, errors.Join(err, files.Close())
	}
	ticker := time.NewTicker(policy.interval)
	var identity unix.Stat_t
	if err := unix.Fstat(int(files.root.Fd()), &identity); err != nil {
		ticker.Stop()
		files.cancelIO()
		return nil, errors.Join(fileFailure("inventory_failed", err), files.Close())
	}
	// 열린 저장소의 실체를 사용해 경로 별칭은 합치고 다른 디렉터리는 구분한다.
	scope := sha256.Sum256(fmt.Appendf(nil, "diagnostic:%d:%d:%s", identity.Dev, identity.Ino, source.instance))
	maintenanceContext, cancelMaintenance := context.WithCancel(files.ioContext)
	upkeep := &recorderUpkeep{context: maintenanceContext, cancelMaintenance: cancelMaintenance, scope: int64(binary.BigEndian.Uint64(scope[:8])), removed: func() uint64 { return files.removed }, run: files.maintain, ticks: ticker.C, stop: ticker.Stop, cancel: files.cancelIO}
	return startRecorder(source, queueLimits{maxQueuedEvents, maxQueuedBytes, maxEventBytes}, files.appendLine, files.Close, upkeep, output, flushBudget, files.appendBatch), nil
}

func (files *Files) appendLine(kind Kind, line []byte) error {
	ctx := files.ioContext
	if ctx == nil {
		ctx = context.Background()
	}
	return files.withStorageLock(ctx, func() error { return files.appendLocked(ctx, kind, line) })
}

func (files *Files) appendLocked(ctx context.Context, kind Kind, line []byte) error {
	if err := files.recoverActive(kind); err != nil {
		return err
	}
	if files.policy != nil {
		name, _ := kind.Filename()
		if err := files.repairPartial(storedLog{instance: files.source.instance, name: name, kind: kind, active: true}, files.policy.now().UTC()); err != nil {
			return err
		}
	}
	if files.policy != nil {
		now := files.policy.now().UTC()
		if err := files.validateStoredTime(line, now); err != nil {
			return err
		}
		if err := files.beforeAppend(kind, len(line), now); err != nil {
			return err
		}
	}
	file := files.active[kind]
	if file == nil {
		return fileFailure("file_unavailable", nil)
	}
	info, err := file.Stat()
	if err != nil {
		return fileFailure("file_unavailable", err)
	}
	// 정책 없는 직접 파일 사용에서도 불완전 행에 다음 JSON을 붙이지 않는다.
	if info.Size() != 0 {
		var tail [1]byte
		if _, err := file.ReadAt(tail[:], info.Size()-1); err != nil {
			return fileFailure("file_unavailable", err)
		}
		if tail[0] != '\n' {
			return fileFailure("partial_write", nil)
		}
	}
	if err := ctx.Err(); err != nil {
		return fileFailure("canceled", err)
	}
	n, err := file.Write(line)
	files.addUsage(kind, int64(n))
	if n == 0 && errors.Is(err, unix.ENOSPC) {
		return fileFailure("storage_full", err)
	}
	if n == 0 && errors.Is(err, unix.EROFS) {
		return fileFailure("storage_read_only", err)
	}
	if n != len(line) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fileFailure("partial_write", err)
	}
	if err != nil {
		return fileFailure("file_unavailable", err)
	}
	if err := file.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	return nil
}

func (recorder *Recorder) finishError() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.output.mu.Lock()
	outputFailures := recorder.output.outputFailures
	recorder.output.mu.Unlock()
	var outputErr error
	if outputFailures != 0 {
		outputErr = fileFailure("stderr_failed", nil)
	}
	return errors.Join(recorder.firstErr, recorder.closeErr, outputErr)
}

// 사용량 캐시는 공유 잠금의 한 보유 구간에만 유효하다.
func (files *Files) appendBatch(batch []queuedLine) []error {
	results := make([]error, len(batch))
	ctx := files.ioContext
	if ctx == nil {
		ctx = context.Background()
	}
	err := files.withStorageLock(ctx, func() error {
		for _, kind := range fileKinds {
			var data []byte
			var indices []int
			var writeGroup func([]int, []byte)
			writeGroup = func(group []int, data []byte) {
				err := files.appendLocked(ctx, kind, data)
				code := failureCode(err)
				// 쓰기 전 예약/시각 거부만 나눈다. I/O 실패는 재시도하지 않는다.
				if len(group) > 1 && (code == "capacity_exceeded" || code == "event_capacity" || code == "event_expired") {
					middle, offset := len(group)/2, 0
					for _, index := range group[:middle] {
						offset += len(batch[index].data)
					}
					writeGroup(group[:middle], data[:offset])
					writeGroup(group[middle:], data[offset:])
					return
				}
				for _, index := range group {
					results[index] = err
				}
				if err != nil {
					files.usageCache = nil
				}
			}
			flush := func() {
				if len(indices) == 0 {
					return
				}
				writeGroup(indices, data)
				data, indices = nil, nil
			}
			for i, item := range batch {
				if item.kind != kind {
					continue
				}
				if files.policy != nil {
					if err := files.validateStoredTime(item.data, files.policy.now().UTC()); err != nil {
						results[i] = err
						continue
					}
					// 회전 상한보다 작은 완전 행 묶음만 한 번에 예약한다.
					if int64(len(data)+len(item.data)) >= files.policy.activeBytes {
						flush()
					}
				}
				data = append(data, item.data...)
				indices = append(indices, i)
			}
			flush()
		}
		return nil
	})
	if err != nil {
		for i := range results {
			results[i] = err
		}
	}
	return results
}

func (files *Files) validateStoredTime(line []byte, now time.Time) error {
	// 일괄 데이터도 각 이벤트의 시각을 독립적으로 검증한다.
	for len(line) != 0 {
		end := bytes.IndexByte(line, '\n')
		if end < 0 {
			return fileFailure("log_invalid", nil)
		}
		var event struct {
			Time time.Time `json:"time"`
		}
		if err := json.Unmarshal(line[:end], &event); err != nil {
			return fileFailure("log_invalid", nil)
		}
		if !event.Time.After(now.Add(-files.policy.retention)) {
			return fileFailure("event_expired", nil)
		}
		line = line[end+1:]
	}
	return nil
}
