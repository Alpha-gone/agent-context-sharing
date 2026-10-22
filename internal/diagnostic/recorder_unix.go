//go:build linux || darwin

package diagnostic

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"time"
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
	upkeep := &recorderUpkeep{run: files.maintain, ticks: ticker.C, stop: ticker.Stop, cancel: files.cancelIO}
	return startRecorder(source, queueLimits{maxQueuedEvents, maxQueuedBytes, maxEventBytes}, files.appendLine, files.Close, upkeep, output, flushBudget), nil
}

func (files *Files) appendLine(kind Kind, line []byte) error {
	ctx := files.ioContext
	if ctx == nil {
		ctx = context.Background()
	}
	return files.withStorageLock(ctx, func() error {
		if files.policy != nil {
			name, _ := kind.Filename()
			if err := files.repairPartial(storedLog{instance: files.source.instance, name: name, kind: kind, active: true}, files.policy.now().UTC()); err != nil {
				return err
			}
		}
		if files.policy != nil {
			now := files.policy.now().UTC()
			var event struct {
				Time time.Time `json:"time"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				return fileFailure("log_invalid", nil)
			}
			if !event.Time.After(now.Add(-files.policy.retention)) {
				return fileFailure("event_expired", nil)
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
	})
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
