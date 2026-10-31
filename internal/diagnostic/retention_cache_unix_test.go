//go:build linux || darwin

package diagnostic

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetentionCacheInvalidatesFileChanges(t *testing.T) {
	for _, mode := range []string{"rewrite_restore_mtime", "replace_inode", "append_corruption", "permissions"} {
		t.Run(mode, func(t *testing.T) {
			dir := obs02Directory(t)
			files := obs04Files(t, dir, "cache")
			line := obs04Line(t, files, Request, obs04Now)
			name := obs04Raw(t, files, Request, line, false, obs04Now)
			if err := files.maintain(); err != nil {
				t.Fatal(err)
			}
			before := files.retentionScannedBytes
			if err := files.maintain(); err != nil || files.retentionScannedBytes != before {
				t.Fatal("불변 회전 파일 재해석", err)
			}
			path := filepath.Join(dir, "cache", name)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			bad := bytes.Clone(line)
			bad[0] = '!'
			switch mode {
			case "rewrite_restore_mtime":
				time.Sleep(time.Millisecond)
				if err := os.WriteFile(path, bad, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "replace_inode":
				replacement := filepath.Join(dir, "cache", "replacement")
				if err := os.WriteFile(replacement, bad, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			case "append_corruption":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.Write([]byte("!\n"))
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			if err := files.maintain(); err == nil {
				t.Fatal("캐시가 변조·손상 검출을 우회했다")
			}
		})
	}
}

func TestRetentionCacheExpiryAndRemoval(t *testing.T) {
	dir := obs02Directory(t)
	files := obs04Files(t, dir, "cache")
	old := obs04Line(t, files, Request, obs04Now.Add(-6*24*time.Hour))
	valid := obs04Line(t, files, Request, obs04Now)
	name := obs04Raw(t, files, Request, append(old, valid...), false, obs04Now)
	if err := files.maintain(); err != nil {
		t.Fatal(err)
	}
	before := files.retentionScannedBytes
	files.policy.now = func() time.Time { return obs04Now.Add(24 * time.Hour) }
	if err := files.maintain(); err != nil {
		t.Fatal(err)
	}
	if files.retentionScannedBytes <= before || !bytes.Equal(obs04Data(t, dir, "cache", name), valid) {
		t.Fatal("만료 경계에서 캐시 재사용·유효 행 손상")
	}
	if err := files.maintain(); err != nil {
		t.Fatal(err)
	}
	if len(files.retentionCache) != 1 {
		t.Fatal("압축 후 새 파일 캐시", len(files.retentionCache))
	}
	if err := os.Remove(filepath.Join(dir, "cache", name)); err != nil {
		t.Fatal(err)
	}
	if err := files.maintain(); err != nil || len(files.retentionCache) != 0 {
		t.Fatal("삭제된 파일의 캐시 잔류", err)
	}
}

func TestRetentionReadYieldsStorageLock(t *testing.T) {
	dir := obs02Directory(t)
	first := obs04Files(t, dir, "first")
	second := obs04Files(t, dir, "second")
	if err := first.withStorageLock(t.Context(), func() error {
		return first.withoutStorageLock(func() error {
			return second.appendLine(Request, obs04Line(t, second, Request, obs04Now))
		})
	}); err != nil {
		t.Fatal("회전 파일 읽기가 다른 인스턴스의 기록을 막았다", err)
	}
	if first.storageHeld {
		t.Fatal("완료 후 저장소 잠금 잔류")
	}
	// 재획득 실패 때 잠금을 갖지 않은 callback이 계속 변경하지 않게 한다.
	ctx, cancel := context.WithCancel(t.Context())
	first.ioContext = ctx
	err := first.withStorageLock(t.Context(), func() error {
		return first.withoutStorageLock(func() error { cancel(); return nil })
	})
	if !errors.Is(err, context.Canceled) || first.storageHeld {
		t.Fatal("재획득 실패의 취소·소유 경계", err)
	}
	if err := second.appendLine(Request, obs04Line(t, second, Request, obs04Now)); err != nil {
		t.Fatal("실패 뒤 다른 인스턴스 잠금 손상", err)
	}
}

func TestRetentionRemovalDoesNotHideBindingFailure(t *testing.T) {
	for _, mode := range []string{"archive_only", "active_missing", "storage_lock_missing"} {
		t.Run(mode, func(t *testing.T) {
			dir := obs02Directory(t)
			files := obs04Files(t, dir, "cache")
			line := obs04Line(t, files, Request, obs04Now)
			name := obs04Raw(t, files, Request, line, false, obs04Now)
			item := storedLog{instance: "cache", name: name, kind: Request, size: int64(len(line))}
			if err := files.withStorageLock(t.Context(), func() error {
				if err := files.removeLog(item); err != nil {
					return err
				}
				if mode == "active_missing" {
					if err := os.Remove(filepath.Join(dir, "cache", "request.jsonl")); err != nil {
						return err
					}
				} else if mode == "storage_lock_missing" {
					if err := os.Remove(filepath.Join(dir, storageLockName)); err != nil {
						return err
					}
				}
				_, err := files.inspectLog(item, obs04Now.Add(-files.policy.retention))
				if err == nil || errors.Is(err, errRetainedLogRemoved) != (mode == "archive_only") {
					t.Fatal("일반 ENOENT를 정상 회전 파일 퇴거로 숨겼다", mode, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetentionStaleInventoryAfterOtherCleanup(t *testing.T) {
	dir := obs02Directory(t)
	first := obs04Files(t, dir, "first")
	second := obs04Files(t, dir, "second")
	old := obs04Line(t, first, Request, obs04Now.Add(-6*24*time.Hour))
	valid := obs04Line(t, first, Request, obs04Now)
	name := obs04Raw(t, first, Request, append(old, valid...), false, obs04Now)
	item := storedLog{instance: "first", name: name, kind: Request, size: int64(len(old) + len(valid))}
	if err := first.maintain(); err != nil {
		t.Fatal(err)
	}
	second.policy.now = func() time.Time { return obs04Now.Add(24 * time.Hour) }
	if err := second.maintain(); err != nil {
		t.Fatal(err)
	}
	if err := first.withStorageLock(t.Context(), func() error {
		summary, err := first.inspectLog(item, obs04Now.Add(-7*24*time.Hour))
		if err != nil {
			return err
		}
		if summary.sourceSize != int64(len(valid)) || summary.valid != 1 {
			t.Fatal("이전 목록의 크기로 압축 후 파일을 읽었다", summary)
		}
		// 다른 기록자가 먼저 퇴거한 부분 행 표식도 안전한 no-op이다.
		marker := storedLog{instance: "first", name: ".partial.request." + obs04Now.Format(rotationTimeLayout) + "." + first.source.boot, kind: Request, quarantined: true}
		if err := first.removeLog(marker); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal("다른 기록자의 정상 정리로 현재 회차가 실패했다", err)
	}
}
