//go:build linux || darwin

package diagnostic

import (
	"context"
	"errors"
	"io"
)

// NewRecorder는 파일 준비를 완료한 뒤 단일 기록 루프를 시작한다.
// Source는 같은 기동의 Reporter와 공유하고 Close로 파일·잠금을 반환한다.
// 회전·보존·stderr 정책의 조립은 후속 관측성 작업에서 수행한다.
func NewRecorder(ctx context.Context, directory string, source Source) (*Recorder, error) {
	files, err := PrepareFiles(ctx, directory, source)
	if err != nil {
		return nil, err
	}
	return newRecorder(source, queueLimits{maxQueuedEvents, maxQueuedBytes, maxEventBytes}, files.appendLine, files.Close), nil
}

func (files *Files) appendLine(kind Kind, line []byte) error {
	return files.withStorageLock(context.Background(), func() error {
		file := files.active[kind]
		info, err := file.Stat()
		if err != nil {
			return fileFailure("file_unavailable", err)
		}
		// 잘린 마지막 행에 다음 JSON을 붙이지 않는다. 격리·복구는 OBS-07의 책임이다.
		if info.Size() != 0 {
			var tail [1]byte
			if _, err := file.ReadAt(tail[:], info.Size()-1); err != nil {
				return fileFailure("file_unavailable", err)
			}
			if tail[0] != '\n' {
				return fileFailure("partial_write", nil)
			}
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
	return errors.Join(recorder.firstErr, recorder.closeErr)
}
