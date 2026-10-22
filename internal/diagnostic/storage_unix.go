//go:build linux || darwin

package diagnostic

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"uuid"

	"golang.org/x/sys/unix"
)

const rotationTimeLayout = "20060102T150405Z"
const maxStorageEntries = 16384

// 고정 정책은 외부 구성으로 열지 않고 모듈 시험에서만 한도·시계를 주입한다.
type storagePolicy struct {
	activeBytes, kindBytes, instanceBytes, totalBytes int64
	kindFiles                                         int
	retention, interval                               time.Duration
	now                                               func() time.Time
	lastSweep                                         time.Time
	days                                              map[Kind]string
}

func defaultStoragePolicy() *storagePolicy {
	return &storagePolicy{activeBytes: 10 << 20, kindFiles: 10, kindBytes: 100 << 20,
		instanceBytes: 600 << 20, totalBytes: 1 << 30, retention: 7 * 24 * time.Hour,
		interval: time.Hour, now: time.Now, days: make(map[Kind]string)}
}

type storedLog struct {
	instance, name    string
	kind              Kind
	active, temporary bool
	quarantined       bool
	size              int64
	stamp             time.Time
}

func logName(name string) (kind Kind, active, temporary bool, stamp time.Time, ok bool) {
	for _, candidate := range fileKinds {
		if name == string(candidate)+".jsonl" {
			return candidate, true, false, time.Time{}, true
		}
	}
	if rest, found := strings.CutPrefix(name, ".compact."); found {
		parts := strings.Split(rest, ".")
		if len(parts) == 2 && validKind(Kind(parts[0])) && validID(parts[1]) {
			return Kind(parts[0]), false, true, time.Time{}, true
		}
		return
	}
	if rest, found := strings.CutPrefix(name, ".partial."); found {
		parts := strings.Split(rest, ".")
		if len(parts) == 3 && validKind(Kind(parts[0])) && validID(parts[2]) {
			stamp, err := time.Parse(rotationTimeLayout, parts[1])
			if err == nil {
				return Kind(parts[0]), false, false, stamp, true
			}
		}
		return
	}
	parts := strings.Split(name, ".")
	if len(parts) != 4 || !validKind(Kind(parts[0])) || !validID(parts[2]) || parts[3] != "jsonl" {
		return
	}
	stamp, err := time.Parse(rotationTimeLayout, parts[1])
	if err == nil {
		return Kind(parts[0]), false, false, stamp, true
	}
	return
}

// 디렉터리의 공유 offset을 바꾸지 않고 유한한 묶음으로 이름을 읽는다.
func storageNames(directory *os.File) ([]string, error) {
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fileFailure("inventory_failed", err)
	}
	reader := os.NewFile(uintptr(fd), "inventory")
	defer reader.Close()
	var names []string
	for {
		batch, err := reader.Readdirnames(128)
		if len(names)+len(batch) > maxStorageEntries {
			return nil, fileFailure("inventory_limit", nil)
		}
		names = append(names, batch...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fileFailure("inventory_failed", err)
		}
	}
	slices.Sort(names)
	return names, nil
}

func (files *Files) inventory() ([]storedLog, error) {
	names, err := storageNames(files.root)
	if err != nil {
		return nil, err
	}
	var logs []storedLog
	for _, instance := range names {
		if instance == storageLockName {
			continue
		}
		if !validInstance(instance) {
			return nil, fileFailure("inventory_invalid", nil)
		}
		slot, err := openDirectory(files.root, instance, false)
		if err != nil {
			return nil, err
		}
		items, scanErr := func() ([]storedLog, error) {
			defer slot.Close()
			entries, err := storageNames(slot)
			if err != nil {
				return nil, err
			}
			if len(logs)+len(entries) > maxStorageEntries {
				return nil, fileFailure("inventory_limit", nil)
			}
			var result []storedLog
			for _, name := range entries {
				if err := checkEntry(slot, name, false); err != nil {
					return nil, err
				}
				if name == writerLockName {
					continue
				}
				kind, active, temporary, stamp, ok := logName(name)
				if !ok {
					return nil, fileFailure("inventory_invalid", nil)
				}
				var stat unix.Stat_t
				if err := unix.Fstatat(int(slot.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					return nil, fileFailure("inventory_failed", err)
				}
				if stat.Size < 0 {
					return nil, fileFailure("inventory_invalid", nil)
				}
				result = append(result, storedLog{instance: instance, name: name, kind: kind, active: active, temporary: temporary, quarantined: strings.HasPrefix(name, ".partial."), size: stat.Size, stamp: stamp})
			}
			return result, nil
		}()
		if scanErr != nil {
			return nil, scanErr
		}
		logs = append(logs, items...)
	}
	slices.SortFunc(logs, func(a, b storedLog) int {
		return cmp.Or(a.stamp.Compare(b.stamp), strings.Compare(a.instance, b.instance), strings.Compare(a.name, b.name))
	})
	return logs, nil
}

func (files *Files) logDirectory(instance string) (*os.File, error) {
	return openDirectory(files.root, instance, false)
}

func boundedSize(current, added, limit int64) int64 {
	if current > limit || added > limit-current {
		return limit + 1
	}
	return current + added
}

// 추가 예약을 포함해 모든 경계를 검사하며 임시 파일도 바이트·개수에 포함한다.
func (policy *storagePolicy) violations(logs []storedLog, instance string, kind Kind, extraBytes int64, extraFiles int) (map[string]bool, map[string]bool, bool) {
	type usage struct {
		bytes int64
		count int
	}
	kinds := make(map[string]usage)
	instances := make(map[string]int64)
	var total int64
	for _, item := range logs {
		key := item.instance + "/" + string(item.kind)
		u := kinds[key]
		u.bytes = boundedSize(u.bytes, item.size, policy.kindBytes)
		u.count++
		kinds[key] = u
		instances[item.instance] = boundedSize(instances[item.instance], item.size, policy.instanceBytes)
		total = boundedSize(total, item.size, policy.totalBytes)
	}
	if instance != "" {
		key := instance + "/" + string(kind)
		u := kinds[key]
		u.bytes = boundedSize(u.bytes, extraBytes, policy.kindBytes)
		u.count += extraFiles
		kinds[key] = u
		instances[instance] = boundedSize(instances[instance], extraBytes, policy.instanceBytes)
		total = boundedSize(total, extraBytes, policy.totalBytes)
	}
	badKinds, badInstances := make(map[string]bool), make(map[string]bool)
	for key, u := range kinds {
		if u.bytes > policy.kindBytes || u.count > policy.kindFiles {
			badKinds[key] = true
		}
	}
	for key, size := range instances {
		if size > policy.instanceBytes {
			badInstances[key] = true
		}
	}
	return badKinds, badInstances, total > policy.totalBytes
}

func (files *Files) removeLog(item storedLog) error {
	if item.active {
		return fileFailure("active_file_protected", nil)
	}
	dir, err := files.logDirectory(item.instance)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := checkEntry(dir, item.name, false); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(dir.Fd()), item.name, 0); err != nil {
		return fileFailure("cleanup_failed", err)
	}
	if err := dir.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	return nil
}

func (files *Files) makeRoom(instance string, kind Kind, bytes int64, count int) error {
	logs, err := files.inventory()
	if err != nil {
		return err
	}
	for {
		badKinds, badInstances, badTotal := files.policy.violations(logs, instance, kind, bytes, count)
		if len(badKinds) == 0 && len(badInstances) == 0 && !badTotal {
			return nil
		}
		index := slices.IndexFunc(logs, func(item storedLog) bool {
			return !item.active && !item.temporary && (badTotal || badInstances[item.instance] || badKinds[item.instance+"/"+string(item.kind)])
		})
		if index < 0 {
			return fileFailure("capacity_exceeded", nil)
		}
		if err := files.removeLog(logs[index]); err != nil {
			return err
		}
		logs = slices.Delete(logs, index, index+1)
	}
}

func createStorageFile(directory *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDWR|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return nil, fileFailure("file_unavailable", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := checkFD(file, false); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (files *Files) rotate(item storedLog, now time.Time) error {
	if err := files.makeRoom(item.instance, item.kind, 0, 1); err != nil {
		return err
	}
	dir, err := files.logDirectory(item.instance)
	if err != nil {
		return err
	}
	defer dir.Close()
	if item.instance == files.source.instance {
		file := files.active[item.kind]
		if file == nil {
			return fileFailure("file_unavailable", nil)
		}
		if err := file.Close(); err != nil {
			return fileFailure("close_failed", err)
		}
		delete(files.active, item.kind)
	}
	rotated := string(item.kind) + "." + now.Format(rotationTimeLayout) + "." + uuid.NewV7().String() + ".jsonl"
	if err := renameExclusive(int(dir.Fd()), item.name, rotated); err != nil {
		if item.instance == files.source.instance {
			if reopened, openErr := openRegular(dir, item.name, false); openErr == nil {
				files.active[item.kind] = reopened
			}
		}
		return fileFailure("rotation_failed", err)
	}
	active, err := createStorageFile(dir, item.name)
	if err != nil {
		return err
	}
	if item.instance == files.source.instance {
		files.active[item.kind] = active
		files.policy.days[item.kind] = now.Format("2006-01-02")
	} else {
		defer active.Close()
	}
	if err := active.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	if err := dir.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	return nil
}

// 다른 instance의 사용 중인 활성 파일은 건드리지 않고 잠금을 역순으로 기다리지 않는다.
func (files *Files) withIdleSlot(instance string, operation func() error) error {
	if instance == files.source.instance {
		return operation()
	}
	dir, err := files.logDirectory(instance)
	if err != nil {
		return err
	}
	defer dir.Close()
	lock, err := openRegular(dir, writerLockName, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return fileFailure("writer_lock_failed", err)
	}
	return operation()
}

type retentionSummary struct {
	expired, valid int
	validBytes     int64
}

// 한 행의 버퍼는 16 KiB로 제한하며 혼합 파일의 유효 행은 원래 바이트를 보존한다.
func visitLog(file *os.File, item storedLog, cutoff time.Time, valid func([]byte) error) (retentionSummary, error) {
	var summary retentionSummary
	reader := bufio.NewReaderSize(io.NewSectionReader(file, 0, item.size), maxEventBytes)
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return summary, nil
		}
		if err != nil || len(line) > maxEventBytes {
			return summary, fileFailure("log_invalid", nil)
		}
		var event encodedEvent
		if err := json.Unmarshal(line, &event, json.RejectUnknownMembers(true)); err != nil || event.Event.Validate() != nil || event.Kind != item.kind || event.InstanceID != item.instance {
			return summary, fileFailure("log_invalid", nil)
		}
		if !event.Time.After(cutoff) {
			summary.expired++
		} else {
			summary.valid++
			summary.validBytes += int64(len(line))
			if valid != nil {
				if err := valid(line); err != nil {
					return summary, err
				}
			}
		}
	}
}

func (files *Files) inspectLog(item storedLog, cutoff time.Time) (retentionSummary, error) {
	dir, err := files.logDirectory(item.instance)
	if err != nil {
		return retentionSummary{}, err
	}
	defer dir.Close()
	file, err := openRegular(dir, item.name, false)
	if err != nil {
		return retentionSummary{}, err
	}
	defer file.Close()
	return visitLog(file, item, cutoff, nil)
}

func (files *Files) compact(item storedLog, summary retentionSummary, cutoff time.Time) (compactErr error) {
	logs, err := files.inventory()
	if err != nil {
		return err
	}
	badKinds, badInstances, badTotal := files.policy.violations(logs, item.instance, item.kind, summary.validBytes, 1)
	if len(badKinds) != 0 || len(badInstances) != 0 || badTotal {
		return fileFailure("cleanup_capacity", nil)
	}
	dir, err := files.logDirectory(item.instance)
	if err != nil {
		return err
	}
	defer dir.Close()
	source, err := openRegular(dir, item.name, false)
	if err != nil {
		return err
	}
	defer source.Close()
	name := ".compact." + string(item.kind) + "." + uuid.NewV7().String()
	target, err := createStorageFile(dir, name)
	if err != nil {
		return err
	}
	defer func() {
		if target != nil {
			if err := target.Close(); err != nil {
				compactErr = errors.Join(compactErr, fileFailure("close_failed", err))
			}
		}
		if compactErr != nil {
			if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				compactErr = errors.Join(compactErr, fileFailure("cleanup_failed", err))
			}
		}
	}()
	_, err = visitLog(source, item, cutoff, func(line []byte) error {
		n, err := target.Write(line)
		if err != nil || n != len(line) {
			return fileFailure("cleanup_write_failed", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := target.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	if err := sameEntry(dir, name, target, false); err != nil {
		return err
	}
	err = target.Close()
	target = nil
	if err != nil {
		return fileFailure("close_failed", err)
	}
	if err := sameEntry(dir, item.name, source, false); err != nil {
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), item.name); err != nil {
		return fileFailure("cleanup_failed", err)
	}
	if err := dir.Sync(); err != nil {
		return fileFailure("sync_failed", err)
	}
	return nil
}

func (files *Files) maintainLocked(now time.Time) error {
	logs, err := files.inventory()
	if err != nil {
		return err
	}
	for _, item := range logs {
		if item.temporary {
			if err := files.removeLog(item); err != nil {
				return err
			}
		}
	}
	if err := files.makeRoom("", "", 0, 0); err != nil {
		return err
	}
	logs, err = files.inventory()
	if err != nil {
		return err
	}
	cutoff := now.Add(-files.policy.retention)
	for _, item := range logs {
		if !item.active || item.size == 0 {
			continue
		}
		err := files.withIdleSlot(item.instance, func() error {
			if err := files.repairPartial(item, now); err != nil {
				return err
			}
			// 복구가 크기를 바꿨으므로 이전 목록의 크기로 행을 읽지 않는다.
			directory, err := files.logDirectory(item.instance)
			if err != nil {
				return err
			}
			var stat unix.Stat_t
			err = unix.Fstatat(int(directory.Fd()), item.name, &stat, unix.AT_SYMLINK_NOFOLLOW)
			directory.Close()
			if err != nil {
				return fileFailure("inventory_failed", err)
			}
			item.size = stat.Size
			summary, err := files.inspectLog(item, cutoff)
			if err != nil {
				return err
			}
			dir, err := files.logDirectory(item.instance)
			if err != nil {
				return err
			}
			file, err := openRegular(dir, item.name, false)
			dir.Close()
			if err != nil {
				return err
			}
			info, err := file.Stat()
			file.Close()
			if err != nil {
				return fileFailure("inventory_failed", err)
			}
			day := info.ModTime().UTC().Format("2006-01-02")
			if item.instance == files.source.instance && files.policy.days[item.kind] != "" {
				day = files.policy.days[item.kind]
			}
			if summary.expired != 0 || item.size >= files.policy.activeBytes || day != now.Format("2006-01-02") {
				return files.rotate(item, now)
			}
			if item.instance == files.source.instance {
				files.policy.days[item.kind] = day
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	logs, err = files.inventory()
	if err != nil {
		return err
	}
	for _, item := range logs {
		if item.active || item.temporary {
			continue
		}
		if item.quarantined {
			if !item.stamp.After(cutoff) {
				if err := files.removeLog(item); err != nil {
					return err
				}
			}
			continue
		}
		summary, err := files.inspectLog(item, cutoff)
		if err != nil {
			return err
		}
		if summary.valid == 0 {
			if err := files.removeLog(item); err != nil {
				return err
			}
		} else if summary.expired != 0 {
			if err := files.compact(item, summary, cutoff); err != nil {
				return err
			}
		}
	}
	if err := files.makeRoom("", "", 0, 0); err != nil {
		return err
	}
	files.policy.lastSweep = now
	return nil
}

func (files *Files) maintain() error {
	ctx := files.ioContext
	if ctx == nil {
		ctx = context.Background()
	}
	return files.withStorageLock(ctx, func() error { return files.maintainLocked(files.policy.now().UTC()) })
}

func (files *Files) beforeAppend(kind Kind, size int, now time.Time) error {
	policy := files.policy
	if len(files.active) != len(fileKinds) {
		return fileFailure("file_unavailable", nil)
	}
	if int64(size) >= policy.activeBytes {
		return fileFailure("event_capacity", nil)
	}
	if policy.lastSweep.IsZero() || now.Sub(policy.lastSweep) >= policy.interval || now.Before(policy.lastSweep) {
		if err := files.maintainLocked(now); err != nil {
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
	day := policy.days[kind]
	if day == "" {
		day = info.ModTime().UTC().Format("2006-01-02")
	}
	if info.Size() != 0 && (info.Size()+int64(size) >= policy.activeBytes || day != now.Format("2006-01-02")) {
		name, _ := kind.Filename()
		if err := files.rotate(storedLog{instance: files.source.instance, name: name, kind: kind, active: true, size: info.Size()}, now); err != nil {
			return err
		}
		if err := files.maintainLocked(now); err != nil {
			return err
		}
	}
	if err := files.makeRoom(files.source.instance, kind, int64(size), 0); err != nil {
		return err
	}
	policy.days[kind] = now.Format("2006-01-02")
	return nil
}
