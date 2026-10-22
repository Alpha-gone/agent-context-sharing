//go:build linux || darwin

package diagnostic

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type obs07Writer func([]byte) (int, error)

func (write obs07Writer) Write(data []byte) (int, error) { return write(data) }

func obs07Wait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("비동기 정리가 끝나지 않았다")
	}
}

func TestOBS07FailureThrottleCumulativeAndSafeCodes(t *testing.T) {
	// 출력 작업자를 시작하지 않아 시험 시계의 정확한 경계를 직접 대조한다.
	output := &failureOutput{source: testSource(t), wake: make(chan struct{}, 1)}
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	output.add(Request, "capacity_exceeded", true)
	rows, _ := output.pending(now)
	if len(rows) != 1 || rows[0].Counts[0] != (failureCount{Request, 1}) {
		t.Fatal(rows)
	}
	output.add(Auth, "capacity_exceeded", true)
	output.add(Request, "capacity_exceeded", true)
	if rows, _ := output.pending(now.Add(failureInterval - time.Nanosecond)); len(rows) != 0 {
		t.Fatal("같은 원인을 30초 전에 출력했다")
	}
	rows, _ = output.pending(now.Add(failureInterval))
	if len(rows) != 1 || rows[0].Dropped != 3 || len(rows[0].Counts) != 2 || rows[0].Counts[0].Count != 2 || rows[0].Counts[1].Count != 1 {
		t.Fatal("억제 중 누락을 잃었다", rows)
	}
	secret := "credential-body-query-uri\n"
	output.add(Kind(secret), secret, true)
	rows, _ = output.pending(now.Add(failureInterval))
	data, _ := json.Marshal(rows)
	if len(rows) != 1 || rows[0].Reason != "write_failed" || rows[0].Counts[0].Kind != "" || bytes.Contains(data, []byte(secret)) {
		t.Fatal("허용하지 않은 원문을 복제했다")
	}
	if failureCode(fileFailure(secret, errors.New(secret))) != "write_failed" {
		t.Fatal("외부 FileError 원인을 그대로 출력했다")
	}
	output.stop()
	rows, stop := output.pending(now.Add(failureInterval))
	if !stop || len(rows) != 1 || rows[0].Reason != "shutdown" || rows[0].Dropped != 4 {
		t.Fatal("종료 요약이 누적 누락을 잃었다", rows)
	}
}

func TestOBS07FailureRowsRemainBounded(t *testing.T) {
	source, err := NewSource(strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	output := &failureOutput{source: source, stopping: true, dropped: ^uint64(0), failures: ^uint64(0)}
	for i := range output.states {
		output.states[i].version = 1
		for j := range output.states[i].counts {
			output.states[i].counts[j] = ^uint64(0)
		}
	}
	rows, _ := output.pending(time.Now())
	for _, row := range rows {
		data, err := json.Marshal(row)
		if err != nil || len(data)+1 > maxEventBytes {
			t.Fatal("최대 누적 값의 stderr 행이 상한을 넘었다", len(data), err)
		}
	}
}

func TestOBS07ActualStderrCollectionAndRecoveryAccounting(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	collected := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(read); collected <- data }()
	source := testSource(t)
	output := newFailureOutput(source, write, time.Now)
	var calls int
	secret := "private-token-body-uri"
	recorder := startRecorder(source, queueLimits{8, maxQueuedBytes, maxEventBytes}, func(Kind, []byte) error {
		calls++
		if calls == 1 {
			return fileFailure("rotation_failed", errors.New(secret))
		}
		return nil
	}, func() error { return nil }, nil, output, flushBudget)
	for _, kind := range []Kind{Management, Management} {
		recorder.Emit(t.Context(), obs03Event(t, source, kind))
	}
	if err := obs03Close(t, recorder); err == nil {
		t.Fatal("선행 실패를 숨겼다")
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	data := <-collected
	if bytes.Contains(data, []byte(secret)) || len(data) == 0 {
		t.Fatal("stderr 미수집 또는 원문 누출")
	}
	var summary bool
	for row := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
		var event failureRow
		if err := json.Unmarshal(row, &event); err != nil {
			t.Fatal(err)
		}
		if event.Instance != source.instance || event.Boot != source.boot {
			t.Fatal("출처를 잃었다")
		}
		if event.Reason == "shutdown" {
			summary = event.Dropped == 1
		}
	}
	stats := recorder.Stats()
	if !summary || stats.Written != 1 || stats.Dropped != 1 || stats.StderrFailures != 0 {
		t.Fatal(stats)
	}
}

func TestOBS07SharedFiveSecondDeadlineAndLateWrite(t *testing.T) {
	source := testSource(t)
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	output := newFailureOutput(source, io.Discard, time.Now)
	recorder := startRecorder(source, queueLimits{8, maxQueuedBytes, maxEventBytes}, func(Kind, []byte) error {
		calls.Add(1)
		close(entered)
		<-release
		return nil
	}, func() error { close(closed); return nil }, nil, output, flushBudget)
	recorder.Emit(t.Context(), obs03Event(t, source, Request))
	<-entered
	for _, kind := range []Kind{Auth, Index} {
		recorder.Emit(t.Context(), obs03Event(t, source, kind))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if err := recorder.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if recorder.Stats().Dropped != 0 {
		t.Fatal("호출자 취소를 flush 마감으로 바꿨다")
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- recorder.Close(context.Background()) }()
	}
	for range 2 {
		if err := <-results; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 4900*time.Millisecond || elapsed > 7*time.Second {
		t.Fatal("5초 종료 경계", elapsed)
	}
	stats := recorder.Stats()
	if stats.Accepted != 3 || stats.Written != 0 || stats.Dropped != 3 || stats.QueuedBytes != 0 || stats.QueuedEvents != 0 || !stats.Writing || len(stats.Losses) != 3 {
		t.Fatal(stats)
	}
	close(release)
	obs07Wait(t, closed)
	obs07Wait(t, recorder.done)
	stats = recorder.Stats()
	if stats.Written != 0 || stats.Dropped != 3 || stats.Writing || calls.Load() != 1 {
		t.Fatal("마감 뒤 누락을 성공으로 바꿨다", stats)
	}
}

func TestOBS07BlockedStderrDoesNotBlockEmitOrClose(t *testing.T) {
	source := testSource(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	output := newFailureOutput(source, obs07Writer(func(data []byte) (int, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return len(data), nil
	}), time.Now)
	recorder := startRecorder(source, queueLimits{8, maxQueuedBytes, maxEventBytes}, func(Kind, []byte) error { return fileFailure("file_unavailable", nil) }, func() error { return nil }, nil, output, 100*time.Millisecond)
	recorder.Emit(t.Context(), obs03Event(t, source, Request))
	<-entered
	done := make(chan struct{})
	go func() { recorder.Emit(context.Background(), obs03Event(t, source, Auth)); close(done) }()
	obs07Wait(t, done)
	start := time.Now()
	if err := recorder.Close(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("stderr가 종료 마감을 연장했다")
	}
	close(release)
	obs07Wait(t, output.done)
}

func TestOBS07StderrFailureDoesNotRecurse(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "short"}[short], func(t *testing.T) {
			source := testSource(t)
			output := newFailureOutput(source, obs07Writer(func(data []byte) (int, error) {
				if short {
					return len(data) - 1, nil
				}
				return 0, errors.New("secret")
			}), time.Now)
			recorder := startRecorder(source, queueLimits{4, maxQueuedBytes, maxEventBytes}, func(Kind, []byte) error { return errors.New("secret") }, func() error { return nil }, nil, output, flushBudget)
			recorder.Emit(t.Context(), obs03Event(t, source, Request))
			if err := obs03Close(t, recorder); err == nil || strings.Contains(err.Error(), "stderr_failed") == false {
				t.Fatal(err)
			}
			if n := recorder.Stats().StderrFailures; n < 1 || n > 2 {
				t.Fatal("stderr 오류를 재귀 출력했다", n)
			}
		})
	}
}

func TestOBS07PartialStartupAndRetention(t *testing.T) {
	for _, instance := range []string{"test_slot", "old_slot"} {
		t.Run(instance, func(t *testing.T) {
			directory := obs02Directory(t)
			files := obs04Files(t, directory, instance)
			line := obs04Line(t, files, Request, obs04Now)
			secret := []byte("{\"body\":\"secret-token-query-uri")
			obs04Raw(t, files, Request, append(bytes.Clone(line), secret...), true, obs04Now)
			if err := files.Close(); err != nil {
				t.Fatal(err)
			}
			source := testSource(t)
			var stderr bytes.Buffer
			output := newFailureOutput(source, &stderr, time.Now)
			recorder, err := prepareRecorderOutput(t.Context(), directory, source, obs04Policy(), output)
			if err != nil {
				t.Fatal(err)
			}
			if err := obs03Close(t, recorder); err != nil {
				t.Fatal(err)
			}
			var data []byte
			names, err := filepath.Glob(filepath.Join(directory, instance, "request*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				part, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, part...)
			}
			if !bytes.Equal(data, line) {
				t.Fatal("정상 접두 행을 바꿨다", err)
			}
			markers, _ := filepath.Glob(filepath.Join(directory, instance, ".partial.*"))
			if len(markers) != 1 {
				t.Fatal(markers)
			}
			marker, err := os.ReadFile(markers[0])
			if err != nil {
				t.Fatal(err)
			}
			var decoded partialMarker
			if err := json.Unmarshal(marker, &decoded); err != nil || decoded.Bytes != int64(len(secret)) || decoded.Kind != Request {
				t.Fatal(decoded, err)
			}
			info, _ := os.Stat(markers[0])
			if info.Mode().Perm() != 0o600 || bytes.Contains(marker, secret) || bytes.Contains(stderr.Bytes(), secret) {
				t.Fatal("격리 기록 보호 실패")
			}
			cleanup := obs04Files(t, directory, "test_slot")
			if err := cleanup.withStorageLock(t.Context(), func() error { return cleanup.maintainLocked(obs04Now.Add(8 * 24 * time.Hour)) }); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(markers[0]); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("격리 표식이 보존 상한을 우회했다", err)
			}
		})
	}
}

func TestOBS07PartialWriteRecoveryKeepsOriginalLoss(t *testing.T) {
	directory := obs02Directory(t)
	files := obs04Files(t, directory, "test_slot")
	source := files.source
	var stderr bytes.Buffer
	output := newFailureOutput(source, &stderr, time.Now)
	files.notice = func(kind Kind, code string) { output.add(kind, code, false) }
	var calls int
	recorder := startRecorder(source, queueLimits{4, maxQueuedBytes, maxEventBytes}, func(kind Kind, line []byte) error {
		calls++
		if calls == 1 {
			// 실제 파일에는 반쪽 행만 남긴 뒤 syscall의 짧은 쓰기 결과를 전달한다.
			if _, err := files.active[kind].Write(line[:len(line)/2]); err != nil {
				return err
			}
			return fileFailure("partial_write", io.ErrShortWrite)
		}
		return files.appendLine(kind, line)
	}, files.Close, nil, output, flushBudget)
	for range 2 {
		event := obs03Event(t, source, Request)
		event.Time = obs04Now
		if !recorder.Emit(t.Context(), event) {
			t.Fatal("이벤트 수락 실패")
		}
	}
	if err := obs03Close(t, recorder); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	stats := recorder.Stats()
	if stats.Written != 1 || stats.Dropped != 1 || stats.Accepted != 2 || !bytes.Contains(stderr.Bytes(), []byte("partial_recovered")) {
		t.Fatal(stats, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(directory, "test_slot", "request.jsonl"))
	var event Event
	if err != nil || bytes.Count(data, []byte{'\n'}) != 1 || json.Unmarshal(data, &event) != nil || event.Validate() != nil {
		t.Fatal("복구 후 독립 JSON 행이 아니다", err)
	}
}

func TestOBS07PartialFailureKeepsOriginal(t *testing.T) {
	for _, scenario := range []string{"capacity", "oversized", "complete_invalid"} {
		t.Run(scenario, func(t *testing.T) {
			directory := obs02Directory(t)
			files := obs04Files(t, directory, "slot")
			line := obs04Line(t, files, Request, obs04Now)
			tail := []byte("partial")
			if scenario == "oversized" {
				tail = bytes.Repeat([]byte{'x'}, maxEventBytes)
			}
			if scenario == "complete_invalid" {
				tail = []byte("invalid\npartial")
			}
			original := append(bytes.Clone(line), tail...)
			obs04Raw(t, files, Request, original, true, obs04Now)
			if scenario == "capacity" {
				files.policy.kindFiles = 1
			}
			err := files.appendLine(Request, line)
			if err == nil {
				t.Fatal("복구 불가능한 행을 변경했다")
			}
			data, readErr := os.ReadFile(filepath.Join(directory, "slot", "request.jsonl"))
			if readErr != nil || !bytes.Equal(data, original) {
				t.Fatal("실패가 원본을 변경했다", readErr)
			}
		})
	}
}
