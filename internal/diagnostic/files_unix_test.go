//go:build linux || darwin

package diagnostic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func obs02Directory(t *testing.T) string {
	t.Helper()
	base := os.Getenv("ACS_OBS02_TEST_ROOT")
	if base == "" {
		var err error
		base, err = filepath.Abs("../..")
		if err != nil {
			t.Fatal(err)
		}
	}
	owned, err := os.MkdirTemp(base, ".obs02-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(owned); err != nil {
			t.Error(err)
		}
	})
	return filepath.Join(owned, "logs")
}

func obs02Prepare(t *testing.T, directory, instance string) *Files {
	t.Helper()
	source, err := NewSource(instance)
	if err != nil {
		t.Fatal(err)
	}
	files, err := PrepareFiles(t.Context(), directory, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	return files
}

func obs02Append(t *testing.T, files *Files) {
	t.Helper()
	err := files.withStorageLock(t.Context(), func() error {
		for _, kind := range fileKinds {
			event := validEvent(t, kind)
			event.InstanceID, event.BootID = files.source.instance, files.source.boot
			data, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err = files.active[kind].Write(append(data, '\n')); err != nil {
				return err
			}
			if err = files.active[kind].Sync(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOBS02PrepareAppendRestart(t *testing.T) {
	directory := obs02Directory(t)
	first := obs02Prepare(t, directory, "slot_a")
	obs02Append(t, first)
	boot := first.source.boot
	lockStat, err := os.Stat(filepath.Join(directory, "slot_a", writerLockName))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{directory, filepath.Join(directory, "slot_a")} {
		stat, err := os.Stat(path)
		if err != nil || stat.Mode().Perm() != 0o700 {
			t.Fatalf("디렉터리 권한: %v", err)
		}
	}
	for _, kind := range fileKinds {
		file := first.active[kind]
		stat, err := file.Stat()
		if err != nil || stat.Mode().Perm() != 0o600 {
			t.Fatal("파일 권한")
		}
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		if err != nil || flags&unix.O_APPEND == 0 {
			t.Fatal("append 플래그")
		}
		flags, err = unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("자식 실행 fd 누출")
		}
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second := obs02Prepare(t, directory, "slot_a")
	if boot == second.source.boot {
		t.Fatal("boot 재사용")
	}
	obs02Append(t, second)
	newLock, err := os.Stat(filepath.Join(directory, "slot_a", writerLockName))
	if err != nil || !os.SameFile(lockStat, newLock) {
		t.Fatal("잠금 inode 교체")
	}
	for _, kind := range fileKinds {
		name, _ := kind.Filename()
		data, err := os.ReadFile(filepath.Join(directory, "slot_a", name))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
		if len(lines) != 2 {
			t.Fatal("기존 기록 손실")
		}
		for index, line := range lines {
			var event Event
			if err = json.Unmarshal(line, &event); err != nil || event.Validate() != nil || event.Kind != kind {
				t.Fatal("독립 이벤트 해석")
			}
			expected := boot
			if index == 1 {
				expected = second.source.boot
			}
			if event.BootID != expected {
				t.Fatal("재기동 전후 기록 혼합")
			}
		}
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	if err = second.withStorageLock(t.Context(), func() error { t.Fatal("종료 뒤 실행"); return nil }); !errors.Is(err, os.ErrClosed) {
		t.Fatal("종료 상태")
	}
}

func TestOBS02RejectsExistingUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(string) error
	}{
		{"root_mode", func(p string) error { return os.Chmod(p, 0o750) }},
		{"slot_mode", func(p string) error { return os.Chmod(filepath.Join(p, "slot_a"), 0o755) }},
		{"file_mode", func(p string) error { return os.Chmod(filepath.Join(p, "slot_a", "system.jsonl"), 0o644) }},
		{"read_only", func(p string) error { return os.Chmod(filepath.Join(p, "slot_a", "system.jsonl"), 0o400) }},
		{"writer_mode", func(p string) error { return os.Chmod(filepath.Join(p, "slot_a", writerLockName), 0o644) }},
		{"storage_mode", func(p string) error { return os.Chmod(filepath.Join(p, storageLockName), 0o644) }},
		{"symlink", func(p string) error {
			target := filepath.Join(p, "slot_a", "system.jsonl")
			if err := os.Rename(target, target+".saved"); err != nil {
				return err
			}
			return os.Symlink("system.jsonl.saved", target)
		}},
		{"hardlink", func(p string) error {
			return os.Link(filepath.Join(p, "slot_a", "system.jsonl"), filepath.Join(p, "external"))
		}},
		{"directory", func(p string) error {
			target := filepath.Join(p, "slot_a", "system.jsonl")
			if err := os.Rename(target, target+".saved"); err != nil {
				return err
			}
			return os.Mkdir(target, 0o700)
		}},
		{"fifo", func(p string) error {
			target := filepath.Join(p, "slot_a", "system.jsonl")
			if err := os.Rename(target, target+".saved"); err != nil {
				return err
			}
			return unix.Mkfifo(target, 0o600)
		}},
		{"writer_symlink", func(p string) error {
			target := filepath.Join(p, "slot_a", writerLockName)
			if err := os.Rename(target, target+".saved"); err != nil {
				return err
			}
			return os.Symlink(writerLockName+".saved", target)
		}},
		{"storage_symlink", func(p string) error {
			target := filepath.Join(p, storageLockName)
			if err := os.Rename(target, target+".saved"); err != nil {
				return err
			}
			return os.Symlink(storageLockName+".saved", target)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := obs02Directory(t)
			files := obs02Prepare(t, directory, "slot_a")
			obs02Append(t, files)
			if err := files.Close(); err != nil {
				t.Fatal(err)
			}
			baseline, err := os.ReadFile(filepath.Join(directory, "slot_a", "request.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if err = tc.mutate(directory); err != nil {
				t.Fatal(err)
			}
			source, _ := NewSource("slot_a")
			if next, err := PrepareFiles(t.Context(), directory, source); err == nil {
				next.Close()
				t.Fatal("보호되지 않은 경로 수락")
			}
			after, err := os.ReadFile(filepath.Join(directory, "slot_a", "request.jsonl"))
			if err != nil || !bytes.Equal(baseline, after) {
				t.Fatal("실패로 기존 기록이 바뀌었다")
			}
		})
	}
}

func TestOBS02RejectsPathsAndOwners(t *testing.T) {
	for _, path := range []string{"", ".", "/", "/tmp/logs", "/private/tmp/logs", "/dev/shm/logs", "/var/folders/logs", "/var/log/../logs", "/var//logs", "/var/logs/", "/var/public/logs", "/var/Assets/logs", "/var/log\\s", "/var/log\ns"} {
		t.Run(fmt.Sprintf("path_%x", path), func(t *testing.T) {
			source, _ := NewSource("slot_a")
			if _, err := PrepareFiles(t.Context(), path, source); err == nil {
				t.Fatal("잘못된 경로 수락")
			}
		})
	}
	for _, scope := range []string{"root", "slot", "parent"} {
		t.Run(scope+"_symlink", func(t *testing.T) {
			dir := obs02Directory(t)
			files := obs02Prepare(t, dir, "slot_a")
			files.Close()
			target := dir
			if scope == "slot" {
				target = filepath.Join(dir, "slot_a")
			}
			if scope == "parent" {
				target = filepath.Dir(dir)
			}
			// 부모 전체를 옮기는 대신 부모를 가리키는 별도 경로를 만든다.
			alias := target + "_alias"
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove(alias) })
			if scope == "slot" {
				if err := os.Rename(target, target+"_saved"); err != nil {
					t.Fatal(err)
				}
				os.Remove(alias)
				if err := os.Symlink(target+"_saved", target); err != nil {
					t.Fatal(err)
				}
			}
			path := alias
			if scope == "slot" {
				path = dir
			}
			if scope == "parent" {
				path = filepath.Join(alias, "logs")
			}
			source, _ := NewSource("slot_a")
			if f, err := PrepareFiles(t.Context(), path, source); err == nil {
				f.Close()
				t.Fatal("심볼릭 경로 수락")
			}
		})
	}
	stat := unix.Stat_t{Uid: uint32(os.Geteuid()) + 1, Mode: unix.S_IFREG | 0o600, Nlink: 1}
	if err := checkStat(&stat, false); err == nil {
		t.Fatal("다른 소유자 수락")
	}
	directory := obs02Directory(t)
	source, _ := NewSource("slot_a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PrepareFiles(ctx, directory, source); !errors.Is(err, context.Canceled) {
		t.Fatal("기동 전 취소")
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("취소 뒤 파일 생성")
	}
	failure := fileFailure("file_unavailable", fmt.Errorf("secret path and input"))
	if strings.Contains(failure.Error(), "secret") || !strings.Contains(failure.Error(), "file_unavailable") {
		t.Fatal("오류 원문 노출")
	}
}

func TestOBS02StorageLockBoundsAndSerialization(t *testing.T) {
	directory := obs02Directory(t)
	first := obs02Prepare(t, directory, "slot_a")
	second := obs02Prepare(t, directory, "slot_b")
	var entered, maxEntered atomic.Int32
	var workers sync.WaitGroup
	for _, files := range []*Files{first, second} {
		workers.Go(func() {
			for range 5 {
				if err := files.withStorageLock(t.Context(), func() error {
					n := entered.Add(1)
					if n > maxEntered.Load() {
						maxEntered.Store(n)
					}
					time.Sleep(time.Millisecond)
					entered.Add(-1)
					return nil
				}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	workers.Wait()
	if maxEntered.Load() != 1 {
		t.Fatal("저장소 잠금이 겹쳤다")
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		done <- first.withStorageLock(t.Context(), func() error { close(locked); <-release; return nil })
	}()
	<-locked
	for _, files := range []*Files{first, second} {
		start := time.Now()
		err := files.withStorageLock(t.Context(), func() error { t.Error("기한 뒤 실행"); return nil })
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < 900*time.Millisecond || time.Since(start) > 2*time.Second {
			t.Fatalf("1초 잠금 경계: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := second.withStorageLock(ctx, func() error { t.Error("취소 뒤 실행"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("요청 마감 무시")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := second.withStorageLock(t.Context(), func() error { return nil }); err != nil {
		t.Fatal("잠금 자리 미반환")
	}
}

func TestOBS02DetectsPathReplacement(t *testing.T) {
	for _, name := range []string{storageLockName, "slot_a/" + writerLockName, "slot_a/request.jsonl"} {
		t.Run(name, func(t *testing.T) {
			directory := obs02Directory(t)
			files := obs02Prepare(t, directory, "slot_a")
			path := filepath.Join(directory, name)
			if err := os.Rename(path, path+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := files.withStorageLock(t.Context(), func() error { t.Error("바뀐 경로에서 실행"); return nil }); err == nil {
				t.Fatal("inode 교체 미검출")
			}
			data, _ := os.ReadFile(path)
			if string(data) != "unchanged" {
				t.Fatal("교체된 파일 변경")
			}
		})
	}
}

func TestOBS02FailedPreparationReleasesLocks(t *testing.T) {
	directory := obs02Directory(t)
	files := obs02Prepare(t, directory, "slot_a")
	files.Close()
	path := filepath.Join(directory, "slot_a", "system.jsonl")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	source, _ := NewSource("slot_a")
	if next, err := PrepareFiles(t.Context(), directory, source); err == nil {
		next.Close()
		t.Fatal("권한 오류 수락")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered := obs02Prepare(t, directory, "slot_a")
	obs02Append(t, recovered)
}

// 자식은 같은 시험 실행 파일로 실제 OS 잠금을 보유한다. 준비 완료 뒤 stdin 종료까지 유지한다.
func TestOBS02ChildProcess(t *testing.T) {
	mode := os.Getenv("ACS_OBS02_CHILD")
	if mode == "" {
		return
	}
	source, _ := NewSource("child_slot")
	files, err := PrepareFiles(t.Context(), os.Getenv("ACS_OBS02_DIRECTORY"), source)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	obs02Append(t, files)
	wait := func() error { fmt.Println("READY"); _, err := io.Copy(io.Discard, os.Stdin); return err }
	if mode == "storage" {
		if err := files.withStorageLock(t.Context(), wait); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	}
}

func obs02Child(t *testing.T, directory, mode string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOBS02ChildProcess$", "-test.timeout=12s")
	cmd.Env = append(os.Environ(), "ACS_OBS02_CHILD="+mode, "ACS_OBS02_DIRECTORY="+directory)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	scanner := bufio.NewScanner(out)
	if !scanner.Scan() || scanner.Text() != "READY" {
		input.Close()
		cmd.Wait()
		t.Fatalf("자식 기동 실패: %s %s", scanner.Text(), stderr.String())
	}
	return cmd, input
}

func TestOBS02ProcessLocksAndExitRecovery(t *testing.T) {
	directory := obs02Directory(t)
	child, input := obs02Child(t, directory, "writer")
	source, _ := NewSource("child_slot")
	if files, err := PrepareFiles(t.Context(), directory, source); err == nil {
		files.Close()
		t.Fatal("중복 프로세스 수락")
	} else if failure, ok := errors.AsType[*FileError](err); !ok || failure.Code != "instance_busy" {
		t.Fatal(err)
	}
	input.Close()
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	files := obs02Prepare(t, directory, "child_slot")
	files.Close()
	child, input = obs02Child(t, directory, "storage")
	start := time.Now()
	source, _ = NewSource("other_slot")
	if files, err := PrepareFiles(t.Context(), directory, source); err == nil {
		files.Close()
		t.Fatal("공용 잠금 무시")
	} else if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatal("기동 잠금 예산", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	input.Close()
	files = obs02Prepare(t, directory, "child_slot")
	obs02Append(t, files)
	for _, kind := range fileKinds {
		name, _ := kind.Filename()
		data, err := os.ReadFile(filepath.Join(directory, "child_slot", name))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
		if len(lines) != 3 {
			t.Fatal("실제 프로세스 재기동 기록 손실")
		}
		boots := map[string]bool{}
		for _, line := range lines {
			var event Event
			if err := json.Unmarshal(line, &event); err != nil || event.Validate() != nil || boots[event.BootID] {
				t.Fatal("프로세스 boot 구분")
			}
			boots[event.BootID] = true
		}
	}
}

// TestPrepareWaitsForIdleSlotMaintenance는 다른 instance가 저장소 잠금 안에서 비활성 슬롯의
// 기록자 잠금을 잠시 쥔 동안 그 슬롯의 instance가 기동하면, instance_busy로 실패하지 않고
// 정리가 끝난 뒤 기록자 잠금을 얻는지 확인한다. 실행 중인 기록자의 거부는
// TestOBS02ProcessLocksAndExitRecovery가 확인한다.
func TestPrepareWaitsForIdleSlotMaintenance(t *testing.T) {
	directory := obs02Directory(t)
	obs02Prepare(t, directory, "idle_slot").Close()
	storage, err := os.OpenFile(filepath.Join(directory, storageLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	writer, err := os.OpenFile(filepath.Join(directory, "idle_slot", writerLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := unix.Flock(int(storage.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(writer.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	source, err := NewSource("idle_slot")
	if err != nil {
		t.Fatal(err)
	}
	type prepared struct {
		files *Files
		err   error
	}
	done := make(chan prepared, 1)
	go func() {
		files, err := PrepareFiles(t.Context(), directory, source)
		done <- prepared{files, err}
	}()
	select {
	case result := <-done:
		if result.files != nil {
			result.files.Close()
		}
		t.Fatalf("슬롯 정리 중 기동이 기다리지 않았다: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	// 정리 경로와 같이 기록자 잠금을 먼저 놓고 저장소 잠금을 놓는다.
	if err := unix.Flock(int(writer.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(storage.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil {
		t.Fatalf("슬롯 정리 뒤 기동 = %v", result.err)
	}
	if err := result.files.Close(); err != nil {
		t.Fatal(err)
	}
}
