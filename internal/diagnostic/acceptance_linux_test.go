package diagnostic

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// 필터·프로세스 한도는 원상 복구할 수 없는 효과가 있어 반드시 자식에 한정한다.
func TestOBS08LinuxWriteErrors(t *testing.T) {
	for _, mode := range []string{"enospc", "erofs", "partial", "sync"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOBS08LinuxFaultHelper$", "-test.timeout=25s")
			cmd.Env = append(os.Environ(), "ACS_OBS08_FAULT="+mode)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if out, err := cmd.Output(); err != nil {
				t.Fatalf("Linux 파일 장애 자식 실패: %v\n%s\n%s", err, out, stderr.Bytes())
			}
			var cause string
			for row := range bytes.SplitSeq(bytes.TrimSpace(stderr.Bytes()), []byte{'\n'}) {
				var notice failureRow
				if err := json.Unmarshal(row, &notice); err != nil {
					t.Fatal("실제 stderr가 JSON 행이 아니다", err)
				}
				if notice.Reason != "shutdown" {
					cause = notice.Reason
				}
			}
			want := map[string]string{"enospc": "storage_full", "erofs": "storage_read_only", "partial": "partial_recovered", "sync": "sync_failed"}[mode]
			if cause != want && !bytes.Contains(stderr.Bytes(), []byte(`"reason_code":"`+want+`"`)) {
				t.Fatal("커널 원인이 stderr에 전달되지 않았다", mode, stderr.String())
			}
		})
	}
}

func TestOBS08LinuxFaultHelper(t *testing.T) {
	mode := os.Getenv("ACS_OBS08_FAULT")
	if mode == "" {
		return
	}
	directory := obs02Directory(t)
	files := obs04Files(t, directory, "test_slot")
	if err := files.withStorageLock(t.Context(), func() error { return files.maintainLocked(obs04Now) }); err != nil {
		t.Fatal(err)
	}
	source := files.source
	output := newFailureOutput(source, os.Stderr, time.Now)
	files.notice = func(kind Kind, code string) { output.add(kind, code, false) }
	recorder := startRecorder(source, queueLimits{4, maxQueuedBytes, maxEventBytes}, files.appendLine, files.Close, nil, output, flushBudget, files.appendBatch)
	defer recorder.Close(context.Background())
	event := obs03Event(t, source, Request)
	event.Time = obs04Now
	line, _, ok := encodeLine(event, maxEventBytes)
	if !ok {
		t.Fatal("시험 이벤트 인코딩")
	}
	oldFile := files.active[Request]
	var limit unix.Rlimit
	var wanted error
	if mode == "partial" {
		if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
			t.Fatal(err)
		}
		signal.Ignore(unix.SIGXFSZ)
		defer signal.Reset(unix.SIGXFSZ)
		defer unix.Setrlimit(unix.RLIMIT_FSIZE, &limit)
		if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: uint64(len(line) / 2), Max: limit.Max}); err != nil {
			t.Fatal(err)
		}
	} else {
		errno := unix.ENOSPC
		if mode == "erofs" {
			errno = unix.EROFS
		} else if mode == "sync" {
			errno = unix.EIO
		} else if mode != "enospc" {
			t.Fatal("알 수 없는 커널 오류")
		}
		wanted = errno
		if mode == "sync" {
			obs08DenySyscall(t, int(oldFile.Fd()), unix.SYS_FSYNC, errno)
		} else {
			obs08DenyWrite(t, int(oldFile.Fd()), errno)
		}
	}
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("첫 이벤트 수락 실패")
	}
	deadline := time.Now().Add(3 * time.Second)
	for recorder.Stats().Dropped != 1 {
		if time.Now().After(deadline) {
			t.Fatal("실제 커널 쓰기 실패 미집계")
		}
		time.Sleep(time.Millisecond)
	}
	if mode == "partial" {
		info, err := oldFile.Stat()
		if err != nil || info.Size() != int64(len(line)/2) {
			t.Fatal("실제 짧은 쓰기 미재현", err)
		}
		if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
			t.Fatal(err)
		}
	} else {
		// 기존 fd는 살아 있게 두어 필터가 다른 파일의 fd 재사용에 영향을 주지 않는다.
		if err := files.withStorageLock(t.Context(), func() error {
			fd, err := unix.FcntlInt(oldFile.Fd(), unix.F_DUPFD_CLOEXEC, int(oldFile.Fd())+64)
			if err != nil {
				return err
			}
			files.active[Request] = os.NewFile(uintptr(fd), "request.jsonl")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		defer oldFile.Close()
	}
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("복구 이벤트 수락 실패")
	}
	err := recorder.Close(context.Background())
	if err == nil || wanted != nil && !errors.Is(err, wanted) {
		t.Fatal("원래 커널 원인 유실", err)
	}
	stats := recorder.Stats()
	if stats.Accepted != 2 || stats.Written != 1 || stats.Dropped != 1 {
		t.Fatal("복구가 과거 누락을 성공으로 바꿨다", stats)
	}
	events, _ := obs08ReadEvents(t, directory, source.instance, Request)
	wantRows := 1
	if mode == "sync" {
		wantRows = 2
	} // sync 실패의 완전 행은 파일에 남을 수 있다.
	if len(events) != wantRows || events[0].BootID != source.boot {
		t.Fatal("복구 뒤 정상 파일 행이 보존되지 않았다")
	}
}

func obs08DenyWrite(t *testing.T, fd int, errno unix.Errno) {
	t.Helper()
	obs08DenySyscall(t, fd, unix.SYS_WRITE, errno)
}

func obs08DenySyscall(t *testing.T, fd, syscallNumber int, errno unix.Errno) {
	t.Helper()
	var arch uint32
	switch runtime.GOARCH {
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	default:
		t.Fatal("시험 필터의 지원 ABI가 아니다")
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: arch},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 3, K: uint32(syscallNumber)},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(fd)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(errno)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	result, _, err := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if err != 0 || result != 0 {
		t.Fatal("시험 자식의 fd 쓰기 제한을 설치하지 못했다", result, err)
	}
}

func TestRotationCreateFailureRecoveryLinux(t *testing.T) {
	for _, mode := range []string{"enospc", "emfile"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRotationCreateFailureHelperLinux$", "-test.timeout=25s")
			cmd.Env = append(os.Environ(), "ACS_ROTATION_FAULT="+mode)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("회전 장애 자식 실패: %v\n%s", err, out)
			}
		})
	}
}

func TestRotationCreateFailureHelperLinux(t *testing.T) {
	mode := os.Getenv("ACS_ROTATION_FAULT")
	if mode == "" {
		t.Skip("Linux 회전 장애 자식 전용")
	}
	errno := unix.ENOSPC
	if mode == "emfile" {
		errno = unix.EMFILE
	}
	directory := obs02Directory(t)
	files := obs04Files(t, directory, "rotation")
	line := obs04Line(t, files, Request, obs04Now)
	files.policy.activeBytes = int64(len(line) * 2)
	if err := files.appendLine(Request, line); err != nil {
		t.Fatal(err)
	}
	// 회전이 여는 디렉터리의 exclusive 생성만 실패시킨다. 복구는 이미
	// 검증된 slot fd를 사용하므로 장애 해소 뒤의 경로를 같은 자식에서 검증한다.
	var arch uint32
	switch runtime.GOARCH {
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	default:
		t.Fatal("시험 필터의 지원 ABI가 아니다")
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: arch},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 5, K: uint32(unix.SYS_OPENAT)},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 3, K: uint32(files.slot.Fd())},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 32},
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: unix.O_EXCL},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(errno)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	result, _, err := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if err != 0 || result != 0 {
		t.Fatal("회전 생성 장애 필터 설치 실패", result, err)
	}
	var stderr bytes.Buffer
	output := newFailureOutput(files.source, &stderr, time.Now)
	recorder := startRecorder(files.source, queueLimits{maxQueuedEvents, maxQueuedBytes, maxEventBytes}, files.appendLine, files.Close, nil, output, flushBudget, files.appendBatch)
	t.Cleanup(func() { recorder.Close(context.Background()) })
	emit := func(kind Kind, completed uint64) {
		t.Helper()
		event := obs03Event(t, files.source, kind)
		event.Time = obs04Now
		if !recorder.Emit(t.Context(), event) {
			t.Fatal("이벤트 수락 실패")
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			stats := recorder.Stats()
			if stats.Written+stats.Dropped == completed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("기록 완료 대기 초과", stats)
			}
			time.Sleep(time.Millisecond)
		}
	}
	emit(Request, 1)
	if stats := recorder.Stats(); stats.Dropped != 1 {
		t.Fatal("회전 생성 실패 누락 미집계", stats)
	}
	for i, kind := range fileKinds[1:] {
		emit(kind, uint64(i+2))
	}
	emit(Request, 7)
	if err := recorder.Close(context.Background()); !errors.Is(err, errno) {
		t.Fatal("기존 회전 실패 원인 유실", err)
	}
	if stats := recorder.Stats(); stats.Accepted != 7 || stats.Written != 6 || stats.Dropped != 1 {
		t.Fatal("종류 격리 또는 회복 집계 실패", stats)
	}
	if !bytes.Contains(stderr.Bytes(), []byte(`"reason_code":"file_unavailable"`)) {
		t.Fatal("회전 실패 알림 누락", stderr.String())
	}
	for _, kind := range fileKinds {
		events, _ := obs08ReadEvents(t, directory, files.source.instance, kind)
		want := 1
		if kind == Request {
			want = 2
		}
		if len(events) != want {
			t.Fatal("기록 행 유실", kind, len(events))
		}
	}
}
