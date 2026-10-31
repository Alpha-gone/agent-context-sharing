//go:build linux || darwin

package diagnostic

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"time"
	"uuid"

	"golang.org/x/sys/unix"
)

type partialMarker struct {
	Time  time.Time `json:"time"`
	Kind  Kind      `json:"kind"`
	Bytes int64     `json:"removed_bytes"`
}

// 공유 저장소 잠금과 해당 슬롯의 기록자 잠금을 보유한 호출자만 복구한다.
// 불완전 원문은 복제하지 않는다. 정상 행 검증·표식의 영속화 뒤에만 꼬리를 제거한다.
func (files *Files) repairPartial(item storedLog, now time.Time) (repairErr error) {
	dir, err := files.logDirectory(item.instance)
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := openRegular(dir, item.name, false)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileFailure("file_unavailable", err)
	}
	if info.Size() == 0 {
		return nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
		return fileFailure("file_unavailable", err)
	}
	if last[0] == '\n' {
		return nil
	}
	files.usageCache = nil
	defer func() { files.usageCache = nil }()
	data := make([]byte, min(info.Size(), int64(maxEventBytes)))
	if _, err := file.ReadAt(data, info.Size()-int64(len(data))); err != nil {
		return fileFailure("file_unavailable", err)
	}
	prefix, tail, found := bytes.CutLast(data, []byte{'\n'})
	start := info.Size() - int64(len(data))
	if found {
		start += int64(len(prefix) + 1)
	} else if start != 0 {
		return fileFailure("log_invalid", nil)
	}
	if !found {
		tail = data
	}
	if len(tail) >= maxEventBytes {
		return fileFailure("log_invalid", nil)
	}
	item.size = start
	if _, err := visitLog(file, item, time.Time{}, nil); err != nil {
		return err
	}
	marker, err := json.Marshal(partialMarker{now.UTC(), item.kind, int64(len(tail))})
	if err != nil {
		return fileFailure("encoding_failed", nil)
	}
	marker = append(marker, '\n')
	if err := files.makeRoom(item.instance, item.kind, int64(len(marker)), 1); err != nil {
		return err
	}
	name := ".partial." + string(item.kind) + "." + now.Format(rotationTimeLayout) + "." + uuid.NewV7().String()
	target, err := createStorageFile(dir, name)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if err := target.Close(); err != nil {
			repairErr = errors.Join(repairErr, fileFailure("close_failed", err))
		}
		if !committed {
			if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
				repairErr = errors.Join(repairErr, fileFailure("cleanup_failed", err))
			}
		}
	}()
	if n, err := target.Write(marker); err != nil || n != len(marker) {
		return fileFailure("partial_write", errors.Join(err, io.ErrShortWrite))
	}
	if err := target.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	if err := dir.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	committed = true
	if err := sameEntry(dir, item.name, file, false); err != nil {
		return err
	}
	if err := file.Truncate(start); err != nil {
		return fileFailure("partial_write", err)
	}
	if err := file.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	if files.notice != nil {
		files.notice(item.kind, "partial_recovered")
	}
	return nil
}
