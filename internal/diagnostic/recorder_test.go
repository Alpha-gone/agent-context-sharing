//go:build linux || darwin

package diagnostic

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"
)

func obs03Event(t *testing.T, source Source, kind Kind) Event {
	t.Helper()
	event := validEvent(t, kind)
	event.InstanceID, event.BootID = source.instance, source.boot
	return event
}

func obs03Close(t *testing.T, recorder *Recorder) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return recorder.Close(ctx)
}

func TestOBS03EncodingLimitsAndOptionalOmission(t *testing.T) {
	event := validEvent(t, Auth)
	event.IdentityVerified = true
	event.FlowID, event.JobID, event.TestRunID = uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String()
	event.Fields = Fields{Identity: IdentityFields{AccountID: uuid.NewV7().String(), ClientID: strings.Repeat("c", 64)}, Auth: AuthFields{ValidationField: "refresh_token"}}
	full, _, ok := encodeLine(event, maxEventBytes)
	if !ok || len(full) > 16<<10 || bytes.Count(full, []byte{'\n'}) != 1 || full[len(full)-1] != '\n' {
		t.Fatal("개행 포함 이벤트 한도를 지키지 않았다")
	}
	if line, _, ok := encodeLine(event, len(full)); !ok || !bytes.Equal(line, full) {
		t.Fatal("경계 크기 이벤트가 축소됐다")
	}
	short, _, ok := encodeLine(event, len(full)-1)
	if !ok || len(short) >= len(full) {
		t.Fatal("선택 필드를 생략하지 않았다")
	}
	var decoded encodedEvent
	if err := json.Unmarshal(short, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.FieldsOmitted || decoded.Fields != (Fields{}) || decoded.Event.Validate() != nil {
		t.Fatal("생략 표시 또는 유효 외피를 잃었다")
	}
	want := event
	want.Fields = Fields{}
	if decoded.Event != want {
		t.Fatal("축소 중 필수 외피·흐름을 변경했다")
	}
	if line, _, ok := encodeLine(event, len(short)); !ok || !bytes.Equal(line, short) {
		t.Fatal("축소된 이벤트의 정확한 경계를 거부했다")
	}
	if line, reason, ok := encodeLine(event, len(short)-1); ok || line != nil || reason != eventTooLarge {
		t.Fatal("필수 외피가 넘칠 때 잘린 이벤트를 만들었다")
	}
	if bytes.Contains(full, []byte("fields_omitted")) {
		t.Fatal("축소하지 않은 이벤트에 생략 표시를 남겼다")
	}
}

func TestOBS03RejectsNewlinesOversizedAndMalformedFields(t *testing.T) {
	cases := map[string]func(*Event){
		"newline":             func(e *Event) { e.Fields.Request.Route = "mcp\nsecret" },
		"carriage_return":     func(e *Event) { e.ReasonCode = "completed\rsecret" },
		"oversized":           func(e *Event) { e.Fields.Request.Tool = strings.Repeat("secret", maxEventBytes) },
		"wrong_kind_fields":   func(e *Event) { e.Fields.Auth.ValidationField = "body" },
		"unverified_identity": func(e *Event) { e.Fields.Identity.AccountID = uuid.NewV7().String() },
		"unknown_kind":        func(e *Event) { e.Kind = "secret" },
		"utc":                 func(e *Event) { e.Time = e.Time.In(time.FixedZone("other", 0)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			event := validEvent(t, Request)
			mutate(&event)
			if line, reason, ok := encodeLine(event, maxEventBytes); ok || line != nil || reason != invalidEvent {
				t.Fatal("잘못된 필드를 생략으로 숨겼다")
			}
		})
	}
}

func TestOBS03QueueCountIncludesInFlightAndDoesNotWaitForIO(t *testing.T) {
	source := testSource(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	recorder := newRecorder(source, queueLimits{maxQueuedEvents, maxQueuedBytes, maxEventBytes}, func(_ Kind, _ []byte) error {
		once.Do(func() { close(entered) })
		<-release
		calls.Add(1)
		return nil
	}, func() error { return nil })
	t.Cleanup(func() { obs03Close(t, recorder) })
	defer close(release)
	event := obs03Event(t, source, Request)
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("첫 이벤트를 거부했다")
	}
	<-entered
	for range 1023 {
		if !recorder.Emit(t.Context(), event) {
			t.Fatal("1024건 전에 포화됐다")
		}
	}
	result := make(chan bool, 1)
	go func() { result <- recorder.Emit(context.Background(), event) }()
	select {
	case accepted := <-result:
		if accepted {
			t.Fatal("1024건을 넘겨 수락했다")
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue가 파일 I/O를 기다린다")
	}
	line, _, _ := encodeLine(event, maxEventBytes)
	stats := recorder.Stats()
	if stats.QueuedEvents != 1024 || stats.QueuedBytes != len(line)*1024 || !stats.Writing || stats.Accepted != 1024 || stats.Dropped != 1 || calls.Load() != 0 {
		t.Fatalf("포화 상태: %+v", stats)
	}
	if len(stats.Losses) != 1 || stats.Losses[0] != (DropCount{Request, "queue_full", 1}) {
		t.Fatal("포화 누락의 종류·원인이 사라졌다")
	}
}

func TestOBS03QueueByteLimitIsIndependentAndRecovers(t *testing.T) {
	source := testSource(t)
	event := obs03Event(t, source, Search)
	line, _, _ := encodeLine(event, maxEventBytes)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	recorder := newRecorder(source, queueLimits{1024, len(line) * 2, maxEventBytes}, func(_ Kind, _ []byte) error {
		once.Do(func() { close(entered); <-release })
		return nil
	}, func() error { return nil })
	defer func() { obs03Close(t, recorder) }()
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("첫 이벤트 거부")
	}
	<-entered
	if !recorder.Emit(t.Context(), event) || recorder.Emit(t.Context(), event) {
		t.Fatal("건수 상한 이전의 바이트 경계를 지키지 않았다")
	}
	stats := recorder.Stats()
	if stats.QueuedEvents != 2 || stats.QueuedBytes != len(line)*2 || stats.Dropped != 1 {
		t.Fatalf("바이트 포화: %+v", stats)
	}
	close(release)
	// 다음 파일 호출을 관찰하는 대신 공개 통계에서 예약 반환을 확인한다.
	deadline := time.Now().Add(3 * time.Second)
	for recorder.Stats().QueuedEvents != 0 {
		if time.Now().After(deadline) {
			t.Fatal("바이트 예약을 반환하지 않았다")
		}
		time.Sleep(time.Millisecond)
	}
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("소진 후 다시 수락하지 않았다")
	}
	if err := obs03Close(t, recorder); err != nil {
		t.Fatal(err)
	}
	stats = recorder.Stats()
	if stats.Accepted != 3 || stats.Written != 3 || stats.Dropped != 1 || stats.QueuedBytes != 0 || stats.QueuedEvents != 0 {
		t.Fatalf("회복 후 집계: %+v", stats)
	}
}

func TestOBS03FIFOAndOwnedSerializedSnapshot(t *testing.T) {
	source := testSource(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var lines [][]byte
	var once sync.Once
	recorder := newRecorder(source, queueLimits{8, maxQueuedBytes, maxEventBytes}, func(_ Kind, line []byte) error {
		once.Do(func() { close(entered); <-release })
		lines = append(lines, bytes.Clone(line))
		return nil
	}, func() error { return nil })
	event := obs03Event(t, source, Search)
	event.Fields.Search.Candidates = 1
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("첫 이벤트 거부")
	}
	<-entered
	for i := uint64(2); i <= 8; i++ {
		event.Fields.Search.Candidates = i
		if !recorder.Emit(t.Context(), event) {
			t.Fatal("후속 이벤트 거부")
		}
	}
	event.Fields.Search.Candidates = 999
	close(release)
	if err := obs03Close(t, recorder); err != nil {
		t.Fatal(err)
	}
	for i, line := range lines {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil || event.Fields.Search.Candidates != uint64(i+1) {
			t.Fatalf("FIFO·값 복사가 깨졌다: %d, %v", i, err)
		}
	}
	if len(lines) != 8 {
		t.Fatal("수락한 행이 사라졌다")
	}
}

func TestOBS03ConcurrentEmitCloseAndLossAccounting(t *testing.T) {
	source := testSource(t)
	var active, maxActive, writes, closes atomic.Int64
	recorder := newRecorder(source, queueLimits{16, maxQueuedBytes, maxEventBytes}, func(_ Kind, line []byte) error {
		current := active.Add(1)
		if current > maxActive.Load() {
			maxActive.Store(current)
		}
		defer active.Add(-1)
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return err
		}
		writes.Add(1)
		return nil
	}, func() error { closes.Add(1); return nil })
	var wg sync.WaitGroup
	for i := range 16 {
		kind := fileKinds[i%len(fileKinds)]
		event := obs03Event(t, source, kind)
		wg.Go(func() {
			for range 100 {
				recorder.Emit(context.Background(), event)
				_ = recorder.Stats()
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			if err := obs03Close(t, recorder); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := obs03Close(t, recorder); err != nil {
		t.Fatal(err)
	}
	stats := recorder.Stats()
	if stats.Accepted+stats.Dropped != 1600 || stats.Written != stats.Accepted || stats.Written != uint64(writes.Load()) || stats.QueuedEvents != 0 || stats.QueuedBytes != 0 || stats.Writing || maxActive.Load() > 1 || closes.Load() != 1 {
		t.Fatalf("동시 실행 집계: %+v", stats)
	}
	var losses uint64
	for _, loss := range stats.Losses {
		losses += loss.Count
	}
	if losses != stats.Dropped {
		t.Fatal("종류·원인별 누락 합계가 다르다")
	}
}

func TestOBS03WriteFailureStaysLostAfterRecovery(t *testing.T) {
	source := testSource(t)
	failure := errors.New("private failure")
	var calls int
	recorder := newRecorder(source, queueLimits{8, maxQueuedBytes, maxEventBytes}, func(_ Kind, _ []byte) error {
		calls++
		if calls == 1 {
			return failure
		}
		return nil
	}, func() error { return nil })
	for _, kind := range []Kind{Index, Management} {
		if !recorder.Emit(t.Context(), obs03Event(t, source, kind)) {
			t.Fatal("이벤트 거부")
		}
	}
	if err := obs03Close(t, recorder); !errors.Is(err, failure) {
		t.Fatal("수락 후 기록 실패를 숨겼다")
	}
	stats := recorder.Stats()
	if stats.Accepted != 2 || stats.Written != 1 || stats.Dropped != 1 || len(stats.Losses) != 1 || stats.Losses[0] != (DropCount{Index, "write_failed", 1}) {
		t.Fatalf("복구가 과거 누락을 성공으로 바꿨다: %+v", stats)
	}
}

func TestOBS03RingReusePreservesOrderAndCloseFailure(t *testing.T) {
	source := testSource(t)
	failure := errors.New("close failure")
	written := make(chan uint64, 1)
	recorder := newRecorder(source, queueLimits{3, maxQueuedBytes, maxEventBytes}, func(_ Kind, line []byte) error {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return err
		}
		written <- event.Fields.Search.Candidates
		return nil
	}, func() error { return failure })
	defer obs03Close(t, recorder)
	for i := uint64(1); i <= 32; i++ {
		event := obs03Event(t, source, Search)
		event.Fields.Search.Candidates = i
		if !recorder.Emit(t.Context(), event) {
			t.Fatal("반환한 대기 자리를 재사용하지 못했다")
		}
		select {
		case got := <-written:
			if got != i {
				t.Fatal("재사용 후 기록 순서가 다르다")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("기록 루프가 멈췄다")
		}
	}
	if err := obs03Close(t, recorder); !errors.Is(err, failure) {
		t.Fatal("파일 종료 실패를 숨겼다")
	}
	stats := recorder.Stats()
	if stats.Written != 32 || stats.Dropped != 0 || stats.QueuedEvents != 0 || stats.QueuedBytes != 0 {
		t.Fatalf("대기 자리 재사용 집계: %+v", stats)
	}
}

func TestOBS03InvalidForeignAndOversizedEnqueueLosses(t *testing.T) {
	source := testSource(t)
	event := obs03Event(t, source, Request)
	line, _, _ := encodeLine(event, maxEventBytes)
	for _, tc := range []struct {
		name   string
		limit  int
		mutate func(*Event)
		reason string
	}{
		{"invalid", maxEventBytes, func(e *Event) { e.Fields.Request.Route = "secret\n" }, "invalid_event"},
		{"source", maxEventBytes, func(e *Event) { e.BootID = uuid.NewV7().String() }, "source_mismatch"},
		{"instance", maxEventBytes, func(e *Event) { e.InstanceID = "another_slot" }, "source_mismatch"},
		{"size", len(line) - 1, func(e *Event) {}, "event_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := newRecorder(source, queueLimits{8, maxQueuedBytes, tc.limit}, func(_ Kind, _ []byte) error {
				t.Error("거부한 이벤트를 파일로 전달했다")
				return nil
			}, func() error { return nil })
			candidate := event
			tc.mutate(&candidate)
			if recorder.Emit(t.Context(), candidate) {
				t.Fatal("잘못된 이벤트를 수락했다")
			}
			if err := obs03Close(t, recorder); err != nil {
				t.Fatal(err)
			}
			stats := recorder.Stats()
			if stats.Accepted != 0 || stats.Dropped != 1 || len(stats.Losses) != 1 || stats.Losses[0] != (DropCount{Request, tc.reason, 1}) {
				t.Fatalf("거부 집계: %+v", stats)
			}
		})
	}
}

func TestOBS03CloseDeadlineLeavesOwnedDrainAndRejectsNewEvents(t *testing.T) {
	source := testSource(t)
	entered, release := make(chan struct{}), make(chan struct{})
	recorder := newRecorder(source, queueLimits{4, maxQueuedBytes, maxEventBytes}, func(_ Kind, _ []byte) error {
		close(entered)
		<-release
		return nil
	}, func() error { return nil })
	event := obs03Event(t, source, Request)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !recorder.Emit(ctx, event) {
		t.Fatal("커밋 후 취소를 기록하지 못한다")
	}
	<-entered
	if err := recorder.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("호출자 마감을 무시했다")
	}
	if recorder.Emit(t.Context(), event) {
		t.Fatal("종료 후 새 이벤트를 수락했다")
	}
	stats := recorder.Stats()
	if stats.QueuedEvents != 1 || !stats.Writing || stats.Dropped != 1 {
		t.Fatal("진행 중 기록을 성공·누락으로 미리 바꿨다")
	}
	close(release)
	if err := obs03Close(t, recorder); err != nil {
		t.Fatal(err)
	}
	stats = recorder.Stats()
	if stats.Written != 1 || stats.Dropped != 1 || stats.QueuedEvents != 0 {
		t.Fatal("종료 경합의 수명·집계가 다르다")
	}
}

func TestOBS03ActualFilesKindsLevelsRestartAndReporter(t *testing.T) {
	directory := obs02Directory(t)
	var boots []string
	for range 2 {
		source := testSource(t)
		boots = append(boots, source.boot)
		recorder, err := NewRecorder(t.Context(), directory, source)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range fileKinds {
			reporter, err := NewReporter(kind, recorder, source)
			if err != nil {
				t.Fatal(err)
			}
			for _, level := range []string{"INFO", "WARN", "ERROR"} {
				if !reporter.RecordDiagnostic(testContext(t), level, "write", "completed", "completed") {
					t.Fatal("유효 이벤트를 수락하지 않았다")
				}
			}
		}
		if err := obs03Close(t, recorder); err != nil {
			t.Fatal(err)
		}
		stats := recorder.Stats()
		if stats.Written != 18 || stats.Dropped != 0 || stats.Accepted != 18 {
			t.Fatalf("실제 파일 집계: %+v", stats)
		}
	}
	for _, kind := range fileKinds {
		path := filepath.Join(directory, "test_slot", string(kind)+".jsonl")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rows := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
		if len(rows) != 6 {
			t.Fatalf("%s: 실제 행 %d", kind, len(rows))
		}
		for i, row := range rows {
			var event Event
			if err := json.Unmarshal(row, &event); err != nil || event.Validate() != nil || event.Kind != kind || event.BootID != boots[i/3] || len(row)+1 > maxEventBytes {
				t.Fatalf("독립 JSON·종류·boot: %s/%d %v", kind, i, err)
			}
			if event.Level != []string{"INFO", "WARN", "ERROR"}[i%3] {
				t.Fatal("심각도가 종류 분류를 바꿨다")
			}
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("기록 후 권한이 다르다")
		}
	}
}

func TestOBS03ActualFileRefusesTruncatedTail(t *testing.T) {
	directory := obs02Directory(t)
	files := obs02Prepare(t, directory, "test_slot")
	if _, err := files.active[Request].WriteString("{\"partial\":"); err != nil {
		t.Fatal(err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	source := testSource(t)
	recorder, err := NewRecorder(t.Context(), directory, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []Kind{Request, Request, Auth} {
		if !recorder.Emit(t.Context(), obs03Event(t, source, kind)) {
			t.Fatal("수락 실패")
		}
	}
	if err := obs03Close(t, recorder); err == nil || strings.Contains(err.Error(), directory) {
		t.Fatalf("부분 행 오류를 숨기거나 경로를 노출했다: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(directory, source.instance, "request.jsonl"))
	if err != nil || string(data) != "{\"partial\":" {
		t.Fatal("잘린 행에 다음 JSON을 이어 썼다")
	}
	stats := recorder.Stats()
	if stats.Written != 1 || stats.Dropped != 2 || stats.QueuedEvents != 0 {
		t.Fatalf("부분 행 누락: %+v", stats)
	}
}

func TestOBS03ActualStorageLockFailureDoesNotBlockEnqueue(t *testing.T) {
	directory := obs02Directory(t)
	source := testSource(t)
	recorder, err := NewRecorder(t.Context(), directory, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { obs03Close(t, recorder) })
	other := obs02Prepare(t, directory, "other_slot")
	locked, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	go func() {
		finished <- other.withStorageLock(context.Background(), func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	accepted := make(chan bool, 1)
	go func() { accepted <- recorder.Emit(context.Background(), obs03Event(t, source, Request)) }()
	select {
	case ok := <-accepted:
		if !ok {
			t.Fatal("잠금 대기를 큐 수락 실패로 바꿨다")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("enqueue가 OS 잠금을 기다린다")
	}
	deadline := time.Now().Add(3 * time.Second)
	for recorder.Stats().Dropped != 1 {
		if time.Now().After(deadline) {
			t.Fatal("실제 잠금 시간 초과를 누락 집계하지 않았다")
		}
		time.Sleep(time.Millisecond)
	}
	once.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !recorder.Emit(t.Context(), obs03Event(t, source, Request)) {
		t.Fatal("잠금 회복 후 수락하지 않았다")
	}
	if err := obs03Close(t, recorder); err == nil {
		t.Fatal("앞선 잠금 실패를 성공으로 바꿨다")
	}
	stats := recorder.Stats()
	if stats.Written != 1 || stats.Dropped != 1 || stats.QueuedEvents != 0 || stats.QueuedBytes != 0 {
		t.Fatalf("실제 잠금 회복 집계: %+v", stats)
	}
	data, err := os.ReadFile(filepath.Join(directory, source.instance, "request.jsonl"))
	if err != nil || bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatal("실패한 이벤트를 재생하거나 회복 후 이벤트를 잃었다")
	}
}
