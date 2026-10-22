//go:build linux || darwin

package diagnostic

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"
)

func obs08ReadEvents(t *testing.T, directory, slot string, kind Kind) ([]Event, []byte) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(directory, slot, string(kind)+"*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	var all []byte
	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("진단 파일 권한", err)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, data...)
		for row := range bytes.SplitSeq(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
			if len(row) == 0 {
				continue
			}
			var event Event
			if len(row)+1 > maxEventBytes || json.Unmarshal(row, &event) != nil || event.Validate() != nil || event.Kind != kind || event.InstanceID != slot {
				t.Fatal("분류·독립 JSON·출처·크기 위반")
			}
			events = append(events, event)
		}
	}
	return events, all
}

func obs08WriteFlow(t *testing.T, recorder *Recorder, source Source) (Trace, Trace) {
	t.Helper()
	run := NewTestRun()
	request := NewRequestTrace().WithTestRun(run)
	flow, err := request.WithVerifiedFlow(uuid.NewV7().String())
	if err != nil {
		t.Fatal(err)
	}
	job := TraceFromContext(ResumeJob(WithTrace(t.Context(), flow), flow.CorrelationID()))
	independent := Trace{}.NewJob().WithTestRun(run)
	for _, kind := range fileKinds {
		reporter, err := NewReporter(kind, recorder, source)
		if err != nil {
			t.Fatal(err)
		}
		trace := flow
		if kind == Index {
			trace = job
		}
		if kind == System {
			trace = independent
		}
		for _, level := range []string{"INFO", "WARN", "ERROR"} {
			result, reason := "completed", "completed"
			if level == "WARN" {
				result, reason = "rejected", "invalid_argument"
			}
			if level == "ERROR" {
				result, reason = "failed", "store_unavailable"
			}
			if !reporter.RecordFields(WithTrace(t.Context(), trace), level, "write", result, reason, trace.flow != "", Fields{}) {
				t.Fatal("흐름 이벤트 거부", kind, level)
			}
		}
	}
	return flow, job
}

func TestOBS08FilesFlowsRestartSlotAndSafety(t *testing.T) {
	directory := obs02Directory(t)
	var allBytes []byte
	var boots []string
	canaries := []string{"Bearer obs08-private-token", "body:obs08-private-context", "work_context:obs08-private-query", "https://private.invalid/?token=obs08-private-uri"}
	for boot := range 2 {
		source, err := NewSource("blue")
		if err != nil {
			t.Fatal(err)
		}
		boots = append(boots, source.boot)
		var stderr bytes.Buffer
		output := newFailureOutput(source, &stderr, time.Now)
		recorder, err := prepareRecorderOutput(t.Context(), directory, source, defaultStoragePolicy(), output)
		if err != nil {
			t.Fatal(err)
		}
		flow, job := obs08WriteFlow(t, recorder, source)
		for i, secret := range canaries {
			event := obs03Event(t, source, Request)
			if i%2 == 0 {
				event.Kind = Kind(secret)
			} else {
				event.Fields.Request.Route = secret
			}
			if recorder.Emit(t.Context(), event) {
				t.Fatal("비밀 표본 수락")
			}
		}
		if err := recorder.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		stats := recorder.Stats()
		if stats.Accepted != 18 || stats.Written != 18 || stats.Dropped != uint64(len(canaries)) {
			t.Fatal(stats)
		}
		allBytes = append(allBytes, stderr.Bytes()...)
		for _, kind := range fileKinds {
			events, data := obs08ReadEvents(t, directory, source.instance, kind)
			allBytes = append(allBytes, data...)
			if len(events) != (boot+1)*3 {
				t.Fatal("재시작 append 누락", kind, len(events))
			}
			var levels []string
			for _, event := range events {
				if event.BootID != source.boot {
					continue
				}
				levels = append(levels, event.Level)
				if event.TestRunID != flow.TestRunID() {
					t.Fatal("시험 흐름 분리 실패")
				}
				if kind == System {
					if event.CorrelationID != "" || event.JobID == "" {
						t.Fatal("요청 없는 시스템 흐름을 요청으로 위장했다")
					}
				} else if event.CorrelationID != flow.CorrelationID() {
					t.Fatal("요청 연결 누락")
				}
				if kind == Index && (event.JobID != job.JobID() || event.FlowID != "") {
					t.Fatal("비동기 작업 연결 누락")
				}
				if kind != Index && kind != System && event.FlowID != flow.flow {
					t.Fatal("확인한 계열 연결 누락")
				}
			}
			if !slices.Equal(levels, []string{"INFO", "WARN", "ERROR"}) {
				t.Fatal(levels)
			}
		}
	}
	if boots[0] == boots[1] {
		t.Fatal("재기동 boot 재사용")
	}
	green, err := NewSource("green")
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(t.Context(), directory, green)
	if err != nil {
		t.Fatal(err)
	}
	obs08WriteFlow(t, recorder, green)
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []string{"blue", "green"} {
		info, err := os.Stat(filepath.Join(directory, slot))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatal("슬롯 디렉터리 권한", err)
		}
		for _, kind := range fileKinds {
			events, data := obs08ReadEvents(t, directory, slot, kind)
			allBytes = append(allBytes, data...)
			want := 3
			if slot == "blue" {
				want = 6
			}
			if len(events) != want {
				t.Fatal("슬롯 교체가 이전 기록을 바꿨다", slot, kind)
			}
		}
	}
	for _, secret := range canaries {
		if bytes.Contains(allBytes, []byte(secret)) {
			t.Fatal("파일 또는 수집한 stderr 원문 누출")
		}
	}
}

func TestOBS08RealLockFlushLossAndRestart(t *testing.T) {
	directory := obs02Directory(t)
	source := testSource(t)
	var stderr bytes.Buffer
	output := newFailureOutput(source, &stderr, time.Now)
	recorder, err := prepareRecorderOutput(t.Context(), directory, source, defaultStoragePolicy(), output)
	if err != nil {
		t.Fatal(err)
	}
	other := obs02Prepare(t, directory, "locker")
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	go func() {
		finished <- other.withStorageLock(context.Background(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	for range 12 {
		if !recorder.Emit(t.Context(), obs03Event(t, source, Request)) {
			t.Fatal("유한 대기열 수락 실패")
		}
	}
	start := time.Now()
	if err := recorder.Close(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > 7*time.Second {
		t.Fatal("실제 잠금 경합이 종료를 연장했다")
	}
	obs07Wait(t, recorder.done)
	obs07Wait(t, output.done)
	stats := recorder.Stats()
	if stats.Accepted != 12 || stats.Written != 0 || stats.Dropped != 12 || stats.QueuedEvents != 0 || stats.QueuedBytes != 0 || stats.Writing {
		t.Fatal(stats)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("storage_lock_timeout")) || !bytes.Contains(stderr.Bytes(), []byte("shutdown_timeout")) {
		t.Fatal("파일·종료 누락 원인이 수집되지 않았다")
	}
	var terminal bool
	for row := range bytes.SplitSeq(bytes.TrimSpace(stderr.Bytes()), []byte{'\n'}) {
		var notice failureRow
		if err := json.Unmarshal(row, &notice); err != nil {
			t.Fatal(err)
		}
		if notice.Reason == "shutdown" {
			terminal = notice.Dropped == 12
		}
	}
	if !terminal {
		t.Fatal("최종 누적 누락 미보고")
	}
	once.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	next := testSource(t)
	restarted, err := NewRecorder(t.Context(), directory, next)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Emit(t.Context(), obs03Event(t, next, Request)) {
		t.Fatal("회복 뒤 수락 실패")
	}
	if err := restarted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := obs08ReadEvents(t, directory, next.instance, Request)
	if len(events) != 1 || events[0].BootID != next.boot {
		t.Fatal("누락 이벤트를 재생하거나 새 boot를 잃었다")
	}
}

// 서로 다른 컨테이너에서 같은 시험 소유 볼륨을 마운트해 실행한다.
func TestOBS08ContainerPersistence(t *testing.T) {
	directory, phase := os.Getenv("ACS_OBS08_PERSIST_ROOT"), os.Getenv("ACS_OBS08_PHASE")
	if directory == "" {
		return
	}
	slot, want := "blue", 3
	if phase == "restart" {
		want = 6
	} else if phase == "replacement" {
		slot = "green"
	} else if phase != "first" {
		t.Fatal("알 수 없는 인수 회차")
	}
	var priorBoot string
	if phase != "first" {
		events, _ := obs08ReadEvents(t, directory, "blue", Request)
		if len(events) == 0 {
			t.Fatal("이전 컨테이너 로그 유실")
		}
		priorBoot = events[0].BootID
	}
	source, err := NewSource(slot)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(t.Context(), directory, source)
	if err != nil {
		t.Fatal(err)
	}
	obs08WriteFlow(t, recorder, source)
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.boot == priorBoot {
		t.Fatal("컨테이너 재생성이 boot를 재사용했다")
	}
	for _, kind := range fileKinds {
		events, _ := obs08ReadEvents(t, directory, slot, kind)
		if len(events) != want {
			t.Fatal("볼륨 append·출처·교체 실패", phase, kind, len(events))
		}
		if phase == "replacement" {
			old, _ := obs08ReadEvents(t, directory, "blue", kind)
			if len(old) != 6 {
				t.Fatal("이전 슬롯 로그 유실")
			}
		}
	}
}
