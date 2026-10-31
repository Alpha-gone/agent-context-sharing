package diagnostic

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"testing"
	"time"
)

// 실제 격리 볼륨을 제공하는 test-linux.sh에서만 약 1 GiB의 합성 파일을 만든다.
func TestRetentionNearCapacityTwoInstancesLinux(t *testing.T) {
	if os.Getenv("ACS_OBS02_TEST_ROOT") == "" {
		t.Skip("격리 볼륨에서만 대용량 인수 실행")
	}
	dir := obs02Directory(t)
	first, second := obs04Files(t, dir, "first"), obs04Files(t, dir, "second")
	var total int64
	for _, files := range []*Files{first, second} {
		for _, kind := range fileKinds {
			line := obs04Line(t, files, kind, obs04Now)
			padded := append(bytes.TrimSuffix(line, []byte{'\n'}), bytes.Repeat([]byte{' '}, maxEventBytes-len(line))...)
			padded = append(padded, '\n')
			data := bytes.Repeat(padded, (10<<20)/len(padded)-1)
			for range 8 {
				obs04Raw(t, files, kind, data, false, obs04Now)
				total += int64(len(data))
			}
		}
	}
	if total < defaultStoragePolicy().totalBytes*9/10 {
		t.Fatal("대용량 표본이 총상한 근처가 아니다", total)
	}
	coldStart := time.Now()
	if err := first.maintain(); err != nil {
		t.Fatal(err)
	}
	if err := second.maintain(); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(coldStart)
	coldHold := max(first.maxStorageHold, second.maxStorageHold)
	before := first.retentionScannedBytes + second.retentionScannedBytes
	first.maxStorageHold, second.maxStorageHold = 0, 0
	warmStart := time.Now()
	for range 5 {
		if err := first.maintain(); err != nil {
			t.Fatal(err)
		}
		if err := second.maintain(); err != nil {
			t.Fatal(err)
		}
	}
	warm := time.Since(warmStart)
	t.Logf("volume_bytes=%d cold_scan=%s cold_max_lock_hold=%s warm_10_sweeps=%s warm_inspected_bytes=%d", total, cold, coldHold, warm, first.retentionScannedBytes+second.retentionScannedBytes-before)
	if delta := first.retentionScannedBytes + second.retentionScannedBytes - before; delta != 0 {
		t.Fatalf("변하지 않은 회전 파일 재해석: %d bytes", delta)
	}
	type result struct {
		elapsed time.Duration
		err     error
	}
	done := make(chan result, 2)
	newRows := make(map[*Files]map[Kind][]byte)
	for _, files := range []*Files{first, second} {
		lines := make(map[Kind][]byte)
		maximum := 0
		for _, kind := range fileKinds {
			lines[kind] = obs04Line(t, files, kind, obs04Now)
			maximum = max(maximum, len(lines[kind]))
		}
		newRows[files] = lines
		files.policy.activeBytes = int64(maximum * 2)
		for _, kind := range fileKinds {
			if err := files.appendLine(kind, lines[kind]); err != nil {
				t.Fatal(err)
			}
		}
		go func() {
			started := time.Now()
			for range 2 {
				for _, kind := range fileKinds {
					if err := files.appendLine(kind, lines[kind]); err != nil {
						done <- result{time.Since(started), err}
						return
					}
				}
			}
			done <- result{time.Since(started), nil}
		}()
	}
	var longest time.Duration
	for range 2 {
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatal("다중 인스턴스 회전·기록 누락", got.err)
			}
			longest = max(longest, got.elapsed)
		case <-time.After(30 * time.Second):
			t.Fatal("다중 인스턴스 기록 정체")
		}
	}
	if delta := first.retentionScannedBytes + second.retentionScannedBytes - before; delta > 1<<20 {
		t.Fatal("새로 회전한 소량 파일 외의 재해석", delta)
	}
	for _, files := range []*Files{first, second} {
		for _, kind := range fileKinds {
			events, _ := obs08ReadEvents(t, dir, files.source.instance, kind)
			var expected Event
			if err := json.Unmarshal(newRows[files][kind], &expected); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, event := range events {
				if event.CorrelationID == expected.CorrelationID {
					count++
				}
			}
			if count != 3 {
				t.Fatal("회전 중 새 이벤트 유실", kind, count)
			}
		}
	}
	t.Logf("volume_bytes=%d cold_scan=%s cold_max_lock_hold=%s warm_10_sweeps=%s warm_max_lock_hold=%s two_instance_writes=%s inspected_delta=%d", total, cold, coldHold, warm, max(first.maxStorageHold, second.maxStorageHold), longest, first.retentionScannedBytes+second.retentionScannedBytes-before)
}
