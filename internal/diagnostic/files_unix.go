//go:build linux || darwin

package diagnostic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

const (
	storageLockName   = ".storage.lock"
	writerLockName    = ".writer.lock"
	storageLockBudget = time.Second
)

// FileError는 경로·OS 오류 원문을 Error 문자열에 포함하지 않는다.
// Code는 준비/잠금 경계의 고정 원인이고 Unwrap은 내부 오류 판정용이다.
type FileError struct {
	Code  string
	cause error
}

func (failure *FileError) Error() string         { return "진단 파일 처리 실패: " + failure.Code }
func (failure *FileError) Unwrap() error         { return failure.cause }
func fileFailure(code string, cause error) error { return &FileError{Code: code, cause: cause} }

var fileKinds = [...]Kind{Request, Auth, Search, Index, Management, System}

// Files는 준비된 파일과 잠금의 수명을 소유한다. 파일 fd·경로를 업무 패키지에
// 공개하지 않는다. 정책 판단·예약·변경은 공유 잠금 안에서 실행하며
// 불변 회전 파일 읽기는 재검증을 전제로 잠금을 양보한다. 대기열·인코딩은 Recorder가 담당한다.
type Files struct {
	usageCache     []storedLog
	inventoryScans uint64
	// 로컬 gate 아래 연속 잠금 점유와 JSON 해석 대상 바이트를 계측한다.
	storageHeldSince      time.Time
	maxStorageHold        time.Duration
	storageHeld           bool
	retentionCache        map[logKey]retainedLog
	retentionScannedBytes uint64
	removed               uint64
	parent                *os.File
	root                  *os.File
	slot                  *os.File
	writerLock            *os.File
	storageLock           *os.File
	active                map[Kind]*os.File
	rootName              string
	source                Source
	gate                  chan struct{}
	closed                bool
	policy                *storagePolicy
	notice                func(Kind, string)
	ioContext             context.Context
	cancelIO              context.CancelFunc
}

// PrepareFiles는 요청 수신 전에 호출한다. 실패 시 열린 fd와 잠금을 반환하며
// 기존 파일은 덮어쓰거나 권한을 변경하지 않는다. 부모 디렉터리는 운영자가 준비한다.
func PrepareFiles(ctx context.Context, directory string, source Source) (_ *Files, prepareErr error) {
	if err := ctx.Err(); err != nil {
		return nil, fileFailure("canceled", err)
	}
	if !validDirectoryPath(directory) || !validInstance(source.instance) || !validID(source.boot) {
		return nil, fileFailure("path_invalid", nil)
	}
	files := &Files{source: source, rootName: filepath.Base(directory), active: make(map[Kind]*os.File), gate: make(chan struct{}, 1)}
	files.gate <- struct{}{}
	defer func() {
		if prepareErr != nil {
			prepareErr = errors.Join(prepareErr, files.Close())
		}
	}()
	var err error
	files.parent, err = openParent(directory)
	if err != nil {
		return nil, err
	}
	files.root, err = openDirectory(files.parent, files.rootName, true)
	if err != nil {
		return nil, err
	}
	files.slot, err = openDirectory(files.root, source.instance, true)
	if err != nil {
		return nil, err
	}
	files.writerLock, err = openRegular(files.slot, writerLockName, true)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(files.writerLock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fileFailure("instance_busy", err)
	}
	files.storageLock, err = openRegular(files.root, storageLockName, true)
	if err != nil {
		return nil, err
	}
	err = files.withStorageLock(ctx, func() error {
		// 모든 파일을 먼저 검사하므로 뒤쪽의 잘못된 기존 파일 때문에 앞 파일을 바꾸지 않는다.
		for _, kind := range fileKinds {
			name, _ := kind.Filename()
			if err := checkEntry(files.slot, name, true); err != nil {
				return err
			}
		}
		for _, kind := range fileKinds {
			if err := ctx.Err(); err != nil {
				return fileFailure("canceled", err)
			}
			name, _ := kind.Filename()
			file, err := openRegular(files.slot, name, true)
			if err != nil {
				return err
			}
			files.active[kind] = file
			if err = file.Sync(); err != nil {
				return fileFailure("sync_failed", err)
			}
		}
		for _, directory := range []*os.File{files.slot, files.root, files.parent} {
			if err := directory.Sync(); err != nil {
				return fileFailure("sync_failed", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func validDirectoryPath(directory string) bool {
	if !filepath.IsAbs(directory) || directory == "/" || filepath.Clean(directory) != directory || strings.Contains(directory, "\\") || strings.ContainsFunc(directory, unicode.IsControl) {
		return false
	}
	for _, temporary := range []string{"/tmp", "/var/tmp", "/private/tmp", "/private/var/tmp", "/var/folders", "/private/var/folders", "/dev/shm"} {
		if directory == temporary || strings.HasPrefix(directory, temporary+"/") {
			return false
		}
	}
	for component := range strings.SplitSeq(directory, "/") {
		if slices.Contains([]string{"assets", "public", "static"}, strings.ToLower(component)) {
			return false
		}
	}
	return true
}

func openParent(directory string) (*os.File, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileFailure("directory_unavailable", err)
	}
	current := os.NewFile(uintptr(fd), "/")
	for component := range strings.SplitSeq(strings.TrimPrefix(filepath.Dir(directory), "/"), "/") {
		if component == "" {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstat(int(current.Fd()), &stat); err != nil {
			current.Close()
			return nil, fileFailure("directory_unavailable", err)
		}
		if (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) || stat.Mode&0o022 != 0 {
			current.Close()
			return nil, fileFailure("parent_unsafe", nil)
		}
		next, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		current.Close()
		if err != nil {
			return nil, fileFailure("path_invalid", err)
		}
		current = os.NewFile(uintptr(next), component)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(current.Fd()), &stat); err != nil {
		current.Close()
		return nil, fileFailure("directory_unavailable", err)
	}
	if (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) || stat.Mode&0o022 != 0 {
		current.Close()
		return nil, fileFailure("parent_unsafe", nil)
	}
	return current, nil
}

func openDirectory(parent *os.File, name string, create bool) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) && create {
		if err = unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fileFailure("directory_unavailable", err)
		}
		fd, err = unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fileFailure("path_invalid", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err = checkFD(file, true); err == nil {
		err = checkFilesystem(fd)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func checkStat(stat *unix.Stat_t, directory bool) error {
	if stat.Uid != uint32(os.Geteuid()) {
		return fileFailure("ownership_invalid", nil)
	}
	mode, kind := uint32(0o600), uint32(unix.S_IFREG)
	if directory {
		mode, kind = 0o700, unix.S_IFDIR
	}
	if uint32(stat.Mode)&unix.S_IFMT != kind || !directory && stat.Nlink != 1 {
		return fileFailure("file_invalid", nil)
	}
	if uint32(stat.Mode)&0o7777 != mode {
		return fileFailure("permissions_invalid", nil)
	}
	return nil
}

func checkFD(file *os.File, directory bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fileFailure("file_unavailable", err)
	}
	return checkStat(&stat, directory)
}

func checkEntry(directory *os.File, name string, allowMissing bool) error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if allowMissing && errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fileFailure("file_unavailable", err)
	}
	return checkStat(&stat, false)
}

func openRegular(directory *os.File, name string, create bool) (*os.File, error) {
	if err := checkEntry(directory, name, create); err != nil {
		return nil, err
	}
	flags := unix.O_RDWR | unix.O_APPEND | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(int(directory.Fd()), name, flags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		fd, err = unix.Openat(int(directory.Fd()), name, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
		if errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat(int(directory.Fd()), name, flags, 0)
		}
	}
	if err != nil {
		return nil, fileFailure("file_unavailable", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err = checkFD(file, false); err == nil {
		err = checkFilesystem(fd)
	}
	if err == nil {
		err = sameEntry(directory, name, file, false)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func sameEntry(directory *os.File, name string, file *os.File, isDirectory bool) error {
	var entry, opened unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileFailure("path_changed", err)
	}
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return fileFailure("file_unavailable", err)
	}
	if entry.Dev != opened.Dev || entry.Ino != opened.Ino {
		return fileFailure("path_changed", nil)
	}
	return checkStat(&entry, isDirectory)
}

func (files *Files) checkBindings() error {
	for _, item := range []struct {
		dir       *os.File
		name      string
		file      *os.File
		directory bool
	}{
		{files.parent, files.rootName, files.root, true}, {files.root, files.source.instance, files.slot, true},
		{files.slot, writerLockName, files.writerLock, false}, {files.root, storageLockName, files.storageLock, false},
	} {
		if err := sameEntry(item.dir, item.name, item.file, item.directory); err != nil {
			return err
		}
	}
	for kind, file := range files.active {
		name, _ := kind.Filename()
		if err := sameEntry(files.slot, name, file, false); err != nil {
			return err
		}
	}
	return nil
}

// withStorageLock의 callback 안에서 정책·쓰기 예약·변경을 함께 수행한다.
// 불변 회전 파일의 해석만 withoutStorageLock으로 양보하고 재검증한다.
// 1초 예산은 로컬 gate와 프로세스 간 잠금 획득에만 적용하며 fs I/O를 강제 중단하지 않는다.
func (files *Files) withStorageLock(ctx context.Context, operation func() error) (operationErr error) {
	lockCtx, cancel := context.WithTimeout(ctx, storageLockBudget)
	defer cancel()
	select {
	case <-lockCtx.Done():
		return fileFailure("storage_lock_timeout", lockCtx.Err())
	case <-files.gate:
	}
	defer func() { files.gate <- struct{}{} }()
	if files.closed {
		return fileFailure("closed", os.ErrClosed)
	}
	if err := files.lockStorage(lockCtx); err != nil {
		return err
	}
	defer func() {
		if err := files.unlockStorage(); err != nil {
			operationErr = errors.Join(operationErr, fileFailure("storage_unlock_failed", err))
		}
	}()
	if err := lockCtx.Err(); err != nil {
		return fileFailure("storage_lock_timeout", err)
	}
	if err := files.checkBindings(); err != nil {
		return err
	}
	return operation()
}

// Close는 파일을 닫은 뒤 잠금을 해제한다. 잠금 파일은 재기동에도 같은 inode를 사용한다.
func (files *Files) Close() error {
	<-files.gate
	defer func() { files.gate <- struct{}{} }()
	if files.closed {
		return nil
	}
	files.closed = true
	if files.cancelIO != nil {
		files.cancelIO()
	}
	var failures []error
	for _, file := range files.active {
		if err := file.Close(); err != nil {
			failures = append(failures, fileFailure("close_failed", err))
		}
	}
	for _, file := range []*os.File{files.storageLock, files.writerLock, files.slot, files.root, files.parent} {
		if file != nil {
			if err := file.Close(); err != nil {
				failures = append(failures, fileFailure("close_failed", err))
			}
		}
	}
	return errors.Join(failures...)
}

func (files *Files) lockStorage(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fileFailure("storage_lock_timeout", err)
		}
		err := unix.Flock(int(files.storageLock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return fileFailure("storage_lock_failed", err)
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fileFailure("storage_lock_timeout", ctx.Err())
		case <-timer.C:
		}
	}

	files.storageHeld = true
	files.storageHeldSince = time.Now()
	return nil
}

func (files *Files) unlockStorage() error {
	files.usageCache = nil
	if !files.storageHeld {
		return nil
	}
	if err := unix.Flock(int(files.storageLock.Fd()), unix.LOCK_UN); err != nil {
		return err
	}
	files.maxStorageHold = max(files.maxStorageHold, time.Since(files.storageHeldSince))
	files.storageHeld = false
	return nil
}

// 로컬 gate는 유지한다. 실패 시에도 재획득을 시도하고, 미획득 상태에서는 변경하지 않는다.
func (files *Files) withoutStorageLock(read func() error) error {
	if err := files.unlockStorage(); err != nil {
		return fileFailure("storage_unlock_failed", err)
	}
	readErr := read()
	ctx := files.ioContext
	if ctx == nil {
		ctx = context.Background()
	}
	lockCtx, cancel := context.WithTimeout(ctx, storageLockBudget)
	defer cancel()
	if err := files.lockStorage(lockCtx); err != nil {
		return errors.Join(readErr, err)
	}
	return errors.Join(readErr, files.checkBindings())
}
