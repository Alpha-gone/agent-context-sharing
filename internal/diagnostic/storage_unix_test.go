//go:build linux || darwin

package diagnostic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"
)

var obs04Now = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func obs04Policy() *storagePolicy {
	policy := defaultStoragePolicy()
	policy.now = func() time.Time { return obs04Now }
	return policy
}

func obs04Files(t *testing.T, directory, instance string) *Files {
	t.Helper()
	files := obs02Prepare(t, directory, instance)
	files.policy = obs04Policy()
	return files
}

func obs04Line(t *testing.T, files *Files, kind Kind, at time.Time) []byte {
	t.Helper()
	event := obs03Event(t, files.source, kind)
	event.Time = at
	line, _, ok := encodeLine(event, maxEventBytes)
	if !ok {
		t.Fatal("이벤트 준비 실패")
	}
	return line
}

func obs04Raw(t *testing.T, files *Files, kind Kind, data []byte, active bool, at time.Time) string {
	t.Helper()
	name := string(kind) + ".jsonl"
	if !active {
		name = string(kind) + "." + at.Format(rotationTimeLayout) + "." + uuid.NewV7().String() + ".jsonl"
	}
	file := files.active[kind]
	if !active {
		var err error
		file, err = createStorageFile(files.slot, name)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	return name
}

func obs04Inventory(t *testing.T, files *Files) []storedLog {
	t.Helper()
	var logs []storedLog
	if err := files.withStorageLock(t.Context(), func() error { var err error; logs, err = files.inventory(); return err }); err != nil {
		t.Fatal(err)
	}
	return logs
}

func obs04Data(t *testing.T, directory, instance, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, instance, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOBS04SizeRotationAndActiveIncludedCount(t *testing.T) {
	for _, boundary := range []string{"size", "count", "kind_bytes"} {
		t.Run(boundary, func(t *testing.T) {
			dir := obs02Directory(t)
			files := obs04Files(t, dir, "slot")
			line := obs04Line(t, files, Request, obs04Now)
			files.policy.activeBytes = int64(len(line) * 2)
			if boundary == "count" {
				files.policy.kindFiles = 2
			}
			if boundary == "kind_bytes" {
				files.policy.kindBytes = int64(len(line) * 2)
			}
			for range 3 {
				if err := files.appendLine(Request, line); err != nil {
					t.Fatal(err)
				}
			}
			logs := obs04Inventory(t, files)
			var count int
			var size int64
			for _, log := range logs {
				if log.kind != Request {
					continue
				}
				count++
				size += log.size
				if log.size >= files.policy.activeBytes {
					t.Fatal("도달 전에 회전하지 않았다")
				}
				if log.active && log.name != "request.jsonl" {
					t.Fatal("활성 basename 변경")
				}
				data := obs04Data(t, dir, "slot", log.name)
				if !bytes.Equal(data, line) {
					t.Fatal("회전이 행을 변경했다")
				}
			}
			want := 3
			if boundary != "size" {
				want = 2
			}
			if count != want || size != int64(want*len(line)) {
				t.Fatalf("활성 포함 개수/바이트: %d %d", count, size)
			}
		})
	}
}

func TestOBS04UTCDateRotationAndRestartAppend(t *testing.T) {
	dir := obs02Directory(t)
	files := obs04Files(t, dir, "slot")
	clock := obs04Now
	files.policy.now = func() time.Time { return clock }
	for _, kind := range fileKinds {
		if err := files.appendLine(kind, obs04Line(t, files, kind, clock)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "slot", string(kind)+".jsonl")
		if err := os.Chtimes(path, clock, clock); err != nil {
			t.Fatal(err)
		}
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	files = obs04Files(t, dir, "slot")
	files.policy.now = func() time.Time { return clock }
	for _, kind := range fileKinds {
		if err := files.appendLine(kind, obs04Line(t, files, kind, clock)); err != nil {
			t.Fatal(err)
		}
	}
	if len(obs04Inventory(t, files)) != 6 {
		t.Fatal("같은 날짜의 재시작이 회전했다")
	}
	clock = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	if err := files.appendLine(Request, obs04Line(t, files, Request, clock)); err != nil {
		t.Fatal(err)
	}
	logs := obs04Inventory(t, files)
	if len(logs) != 12 {
		t.Fatalf("날짜 변경: %d", len(logs))
	}
	for _, log := range logs {
		data := obs04Data(t, dir, "slot", log.name)
		if !log.active && bytes.Count(data, []byte{'\n'}) != 2 {
			t.Fatal("이전 날짜의 두 기동 행을 잃었다")
		}
		info, err := os.Stat(filepath.Join(dir, "slot", log.name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("회전 후 권한")
		}
	}
}

func TestOBS04RetentionBoundaryAndMixedFilePreservesValidBytes(t *testing.T) {
	dir := obs02Directory(t)
	files := obs04Files(t, dir, "slot")
	expired := obs04Line(t, files, Request, obs04Now.Add(-7*24*time.Hour))
	validA := obs04Line(t, files, Request, obs04Now.Add(-7*24*time.Hour+time.Nanosecond))
	validB := obs04Line(t, files, Request, obs04Now.Add(-time.Hour))
	data := bytes.Join([][]byte{validA, expired, validB, expired}, nil)
	name := obs04Raw(t, files, Request, data, false, obs04Now.Add(-time.Hour))
	oldInfo, _ := os.Stat(filepath.Join(dir, "slot", name))
	if err := files.maintain(); err != nil {
		t.Fatal(err)
	}
	if got := obs04Data(t, dir, "slot", name); !bytes.Equal(got, append(bytes.Clone(validA), validB...)) {
		t.Fatal("혼합 정리가 유효 행·순서를 바꿨다")
	}
	newInfo, _ := os.Stat(filepath.Join(dir, "slot", name))
	if os.SameFile(oldInfo, newInfo) || newInfo.Mode().Perm() != 0o600 {
		t.Fatal("보호된 임시 교체가 아니다")
	}
	for _, log := range obs04Inventory(t, files) {
		if log.temporary {
			t.Fatal("임시 파일 잔여")
		}
	}
}

func TestOBS04CleanupSpaceFailurePreservesOriginal(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(strconv.FormatBool(exact), func(t *testing.T) {
			dir := obs02Directory(t)
			files := obs04Files(t, dir, "slot")
			expired := obs04Line(t, files, Request, obs04Now.Add(-8*24*time.Hour))
			valid := obs04Line(t, files, Request, obs04Now)
			original := append(bytes.Clone(expired), valid...)
			name := obs04Raw(t, files, Request, original, false, obs04Now)
			files.policy.totalBytes = int64(len(original) + len(valid))
			if !exact {
				files.policy.totalBytes--
			}
			err := files.maintain()
			if !exact {
				failure, ok := errors.AsType[*FileError](err)
				if !ok || failure.Code != "cleanup_capacity" {
					t.Fatalf("임시 공간 예약 실패: %v", err)
				}
				if !bytes.Equal(obs04Data(t, dir, "slot", name), original) {
					t.Fatal("공간 부족이 원본을 변경했다")
				}
			} else if err != nil || !bytes.Equal(obs04Data(t, dir, "slot", name), valid) {
				t.Fatal("정확한 임시 공간 경계를 거부했다", err)
			}
		})
	}
}

func TestOBS04ExpiredLiveAndInactiveActiveFiles(t *testing.T) {
	dir := obs02Directory(t)
	a := obs04Files(t, dir, "slot_a")
	b := obs04Files(t, dir, "slot_b")
	line := obs04Line(t, a, Auth, obs04Now.Add(-8*24*time.Hour))
	obs04Raw(t, a, Auth, line, true, obs04Now)
	info, _ := os.Stat(filepath.Join(dir, "slot_a", "auth.jsonl"))
	if err := b.maintain(); err != nil {
		t.Fatal(err)
	}
	if got := obs04Data(t, dir, "slot_a", "auth.jsonl"); !bytes.Equal(got, line) {
		t.Fatal("다른 기록자의 활성 파일을 변경했다")
	}
	if err := a.maintain(); err != nil {
		t.Fatal(err)
	}
	if len(obs04Data(t, dir, "slot_a", "auth.jsonl")) != 0 {
		t.Fatal("자기 활성 파일 만료 정리 실패")
	}
	newInfo, _ := os.Stat(filepath.Join(dir, "slot_a", "auth.jsonl"))
	if os.SameFile(info, newInfo) {
		t.Fatal("활성 파일을 회전하지 않고 잘랐다")
	}
	obs04Raw(t, a, Auth, line, true, obs04Now)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.maintain(); err != nil {
		t.Fatal(err)
	}
	if len(obs04Data(t, dir, "slot_a", "auth.jsonl")) != 0 {
		t.Fatal("이전 슬롯의 만료 활성 파일이 남았다")
	}
}

func TestOBS04InstanceCapacityEvictsOldestRotatedOnly(t *testing.T) {
	dir := obs02Directory(t)
	files := obs04Files(t, dir, "slot")
	request := obs04Line(t, files, Request, obs04Now)
	auth := obs04Line(t, files, Auth, obs04Now)
	files.policy.activeBytes = int64(len(request) * 2)
	files.policy.instanceBytes = int64(len(request) + len(auth))
	for range 2 {
		if err := files.appendLine(Request, request); err != nil {
			t.Fatal(err)
		}
	}
	if err := files.appendLine(Auth, auth); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, log := range obs04Inventory(t, files) {
		total += log.size
		if !log.active {
			t.Fatal("가장 오래된 회전 파일을 정리하지 않았다")
		}
	}
	if total != files.policy.instanceBytes {
		t.Fatal("인스턴스의 정확한 바이트 경계")
	}
	before := obs04Data(t, dir, "slot", "request.jsonl")
	if err := files.appendLine(System, obs04Line(t, files, System, obs04Now)); err == nil {
		t.Fatal("활성 파일만 남은 상한을 우회했다")
	}
	if !bytes.Equal(obs04Data(t, dir, "slot", "request.jsonl"), before) {
		t.Fatal("상한 실패가 활성 파일을 삭제·변경했다")
	}
}

func TestOBS04StartupCleansOldSlotsAndOldestCount(t *testing.T) {
	dir := obs02Directory(t)
	old := obs04Files(t, dir, "old_slot")
	var names []string
	for i := range 11 {
		line := obs04Line(t, old, Request, obs04Now.Add(-time.Hour))
		names = append(names, obs04Raw(t, old, Request, line, false, obs04Now.Add(time.Duration(i-12)*time.Hour)))
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	source, _ := NewSource("new_slot")
	recorder, err := prepareRecorder(t.Context(), dir, source, obs04Policy())
	if err != nil {
		t.Fatal(err)
	}
	if err := obs03Close(t, recorder); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		_, err := os.Stat(filepath.Join(dir, "old_slot", name))
		if i < 2 && !errors.Is(err, os.ErrNotExist) || i >= 2 && err != nil {
			t.Fatalf("정렬·이전 슬롯: %d %v", i, err)
		}
	}
}

func TestOBS04StartupRejectsMalformedAndUnknownLogWithoutDisclosure(t *testing.T) {
	for _, bad := range []string{"complete_invalid", "unknown", "symlink"} {
		t.Run(bad, func(t *testing.T) {
			dir := obs02Directory(t)
			files := obs04Files(t, dir, "slot")
			var path string
			if bad == "complete_invalid" {
				obs04Raw(t, files, Request, []byte("{\"secret\":\n"), true, obs04Now)
				path = filepath.Join(dir, "slot", "request.jsonl")
			} else {
				path = filepath.Join(dir, "slot", "secret-input")
				if bad == "unknown" {
					if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink("/outside-secret", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := files.Close(); err != nil {
				t.Fatal(err)
			}
			source, _ := NewSource("slot")
			if recorder, err := prepareRecorder(t.Context(), dir, source, obs04Policy()); err == nil {
				obs03Close(t, recorder)
				t.Fatal("잘못된 저장소를 수락했다")
			} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), dir) {
				t.Fatal("원문·경로 누출", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("실패가 기존 파일을 삭제했다")
			}
		})
	}
}

func TestOBS04OrphanTemporaryCleanupKeepsOriginal(t *testing.T) {
	dir := obs02Directory(t)
	files := obs04Files(t, dir, "slot")
	line := obs04Line(t, files, Request, obs04Now)
	name := obs04Raw(t, files, Request, line, false, obs04Now)
	orphan := ".compact.request." + uuid.NewV7().String()
	temp, err := createStorageFile(files.slot, orphan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temp.WriteString("partial"); err != nil {
		t.Fatal(err)
	}
	temp.Close()
	if err := files.maintain(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "slot", orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("중단된 임시 파일 잔여")
	}
	if !bytes.Equal(obs04Data(t, dir, "slot", name), line) {
		t.Fatal("임시 파일 정리가 원본을 변경했다")
	}
}

func TestOBS04ExclusiveRotationNeverOverwritesDestination(t *testing.T) {
	files := obs04Files(t, obs02Directory(t), "slot")
	a, err := createStorageFile(files.slot, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := createStorageFile(files.slot, "b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a.WriteString("first")
	b.WriteString("second")
	if err := renameExclusive(int(files.slot.Fd()), "a", "b"); err == nil {
		t.Fatal("기존 회전 이름을 덮어썼다")
	}
	var data [6]byte
	if n, err := b.ReadAt(data[:], 0); n != 6 || err != nil || string(data[:]) != "second" {
		t.Fatal("대상 파일이 바뀌었다")
	}
}

func TestOBS04IdleMaintenanceTickAndFailureAccounting(t *testing.T) {
	dir := obs02Directory(t)
	files := obs04Files(t, dir, "slot")
	var clock atomic.Int64
	clock.Store(obs04Now.UnixNano())
	files.policy.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	ticks := make(chan time.Time, 1)
	completed := make(chan error, 2)
	upkeep := &recorderUpkeep{run: func() error { err := files.maintain(); completed <- err; return err }, ticks: ticks, stop: func() {}}
	recorder := newRecorder(files.source, queueLimits{1024, maxQueuedBytes, maxEventBytes}, files.appendLine, files.Close, upkeep)
	t.Cleanup(func() { obs03Close(t, recorder) })
	event := obs03Event(t, files.source, Request)
	event.Time = obs04Now
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("수락 실패")
	}
	deadline := time.Now().Add(3 * time.Second)
	for recorder.Stats().Written != 1 {
		if time.Now().After(deadline) {
			t.Fatal("기록 지연")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Store(obs04Now.Add(7*24*time.Hour + time.Minute).UnixNano())
	ticks <- obs04Now
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("유휴 정리 미실행")
	}
	if len(obs04Data(t, dir, "slot", "request.jsonl")) != 0 {
		t.Fatal("유휴 만료 기록 잔여")
	}
	path := filepath.Join(dir, "slot", "unknown")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ticks <- obs04Now
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("정리 실패를 성공으로 반환했다")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("정리 실패 관찰 지연")
	}
	deadline = time.Now().Add(3 * time.Second)
	for recorder.Stats().MaintenanceFailures != 1 {
		if time.Now().After(deadline) {
			t.Fatal("정리 실패 미집계")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := obs03Close(t, recorder); err == nil {
		t.Fatal("주기 정리 실패를 숨겼다")
	}
	if stats := recorder.Stats(); stats.Dropped != 0 || stats.Written != 1 {
		t.Fatal("정리 실패를 가짜 이벤트 누락으로 집계했다")
	}
}

func TestOBS04ExpiredIncomingEventIsDropped(t *testing.T) {
	files := obs04Files(t, obs02Directory(t), "slot")
	line := obs04Line(t, files, Request, obs04Now.Add(-7*24*time.Hour))
	if err := files.appendLine(Request, line); err == nil {
		t.Fatal("이미 보존 만료된 새 이벤트를 기록했다")
	}
	info, err := files.active[Request].Stat()
	if err != nil || info.Size() != 0 {
		t.Fatal("만료 이벤트가 파일을 변경했다")
	}
}

func TestOBS04HugeStoredSizesCannotOverflowCapacity(t *testing.T) {
	policy := obs04Policy()
	logs := []storedLog{{instance: "slot", kind: Request, size: 1 << 62}, {instance: "slot", kind: Request, size: 1 << 62}, {instance: "slot", kind: Auth, size: 1 << 62}}
	kinds, instances, total := policy.violations(logs, "slot", Request, maxEventBytes, 1)
	if !total || !instances["slot"] || !kinds["slot/request"] || !kinds["slot/auth"] {
		t.Fatal("정수 범위 초과로 용량 검사를 우회했다")
	}
}

func TestOBS04ConcurrentProcessesReserveGlobalCapacity(t *testing.T) {
	dir := obs02Directory(t)
	// 시각·슬롯 길이를 고정해 두 프로세스의 행 길이도 같다.
	source, _ := NewSource("slot_a")
	event := obs03Event(t, source, Request)
	event.Time = obs04Now
	line, _, _ := encodeLine(event, maxEventBytes)
	capacity := len(line) * 8
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	type child struct {
		cmd    *exec.Cmd
		start  func()
		done   chan error
		output *bytes.Buffer
	}
	var children []child
	for _, slot := range []string{"slot_a", "slot_b"} {
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestOBS04WriterProcessHelper$", "-test.timeout=20s")
		cmd.Env = append(os.Environ(), "ACS_OBS04_CHILD="+slot, "ACS_OBS04_ROOT="+dir, "ACS_OBS04_CAPACITY="+strconv.Itoa(capacity))
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var output, stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() || scanner.Text() != "READY" {
			cmd.Wait()
			t.Fatalf("프로세스 준비 실패: %s", stderr.String())
		}
		done := make(chan error, 1)
		go func() {
			for scanner.Scan() {
				output.WriteString(scanner.Text())
			}
			err := scanner.Err()
			if waitErr := cmd.Wait(); waitErr != nil {
				err = waitErr
			}
			output.Write(stderr.Bytes())
			done <- err
		}()
		children = append(children, child{cmd: cmd, start: func() { stdin.Write([]byte{'x'}); stdin.Close() }, done: done, output: &output})
	}
	for _, child := range children {
		child.start()
	}
	for _, child := range children {
		if err := <-child.done; err != nil {
			t.Fatalf("자식 프로세스: %v %s", err, child.output.String())
		}
	}
	var total int64
	var lines int
	for _, slot := range []string{"slot_a", "slot_b"} {
		for _, kind := range fileKinds {
			data := obs04Data(t, dir, slot, string(kind)+".jsonl")
			total += int64(len(data))
			lines += bytes.Count(data, []byte{'\n'})
			for _, row := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
				if len(row) == 0 {
					continue
				}
				var event Event
				if json.Unmarshal(row, &event) != nil || event.InstanceID != slot {
					t.Fatal("프로세스 출처·JSON 혼합")
				}
			}
		}
	}
	if total != int64(capacity) || lines != 8 {
		t.Fatalf("공유 예약 한도: %d/%d, %d행", total, capacity, lines)
	}
}

func TestOBS04WriterProcessHelper(t *testing.T) {
	slot := os.Getenv("ACS_OBS04_CHILD")
	if slot == "" {
		return
	}
	capacity, err := strconv.ParseInt(os.Getenv("ACS_OBS04_CAPACITY"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	policy := obs04Policy()
	policy.totalBytes = capacity
	source, _ := NewSource(slot)
	recorder, err := prepareRecorder(context.Background(), os.Getenv("ACS_OBS04_ROOT"), source, policy)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	var start [1]byte
	if _, err := os.Stdin.Read(start[:]); err != nil {
		t.Fatal(err)
	}
	event := obs03Event(t, source, Request)
	event.Time = obs04Now
	for range 16 {
		if !recorder.Emit(t.Context(), event) {
			t.Fatal("대기 수락 실패")
		}
	}
	err = obs03Close(t, recorder)
	stats := recorder.Stats()
	if stats.Written+stats.Dropped != 16 || stats.MaintenanceFailures != 0 {
		t.Fatal("자식 집계 오류")
	}
	if stats.Dropped != 0 && err == nil {
		t.Fatal("총상한 누락을 숨겼다")
	}
}
