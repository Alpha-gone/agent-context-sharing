//go:build linux || darwin

package diagnostic

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBatchCapacityAndLockRefresh(t *testing.T) {
	directory := obs02Directory(t)
	files := obs04Files(t, directory, "batch_slot")
	other := obs04Files(t, directory, "other_slot")
	if err := files.maintain(); err != nil {
		t.Fatal(err)
	}
	if err := other.maintain(); err != nil {
		t.Fatal(err)
	}
	var batch []queuedLine
	expected := make(map[Kind][]byte)
	for range 12 {
		for _, kind := range fileKinds {
			line := obs04Line(t, files, kind, obs04Now)
			batch = append(batch, queuedLine{kind, line})
			expected[kind] = append(expected[kind], line...)
		}
	}
	before := files.inventoryScans
	for _, err := range files.appendBatch(batch) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if scans := files.inventoryScans - before; scans != 1 {
		t.Fatalf("inventory scans = %d, want 1", scans)
	}
	for kind, want := range expected {
		got, err := os.ReadFile(filepath.Join(directory, files.source.instance, string(kind)+".jsonl"))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("kind %s: bytes/order changed: %v", kind, err)
		}
	}
	// 잠금 해제 뒤 다른 기록자가 사용한 용량을 다시 수집한다.
	line := obs04Line(t, other, Request, obs04Now)
	if err := other.appendLine(Request, line); err != nil {
		t.Fatal(err)
	}
	var used int64
	for _, log := range obs04Inventory(t, files) {
		used += log.size
	}
	files.policy.totalBytes = used
	result := files.appendBatch([]queuedLine{batch[0]})
	if failureCode(result[0]) != "capacity_exceeded" {
		t.Fatalf("stale capacity: %v", result)
	}
}

func TestBatchRotationAndExpiredIsolation(t *testing.T) {
	directory := obs02Directory(t)
	files := obs04Files(t, directory, "batch_slot")
	if err := files.maintain(); err != nil {
		t.Fatal(err)
	}
	line := obs04Line(t, files, Request, obs04Now)
	files.policy.activeBytes = int64(len(line) * 3)
	expired := obs04Line(t, files, Auth, obs04Now.Add(-8*24*time.Hour))
	batch := []queuedLine{{Request, line}, {Auth, expired}, {Request, line}, {Request, line}, {Request, line}}
	results := files.appendBatch(batch)
	for i, err := range results {
		if i == 1 {
			if failureCode(err) != "event_expired" {
				t.Fatal(results)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var recorded int
	for _, item := range obs04Inventory(t, files) {
		if item.kind != Request {
			continue
		}
		if item.size >= files.policy.activeBytes {
			t.Fatalf("rotation exceeded: %d", item.size)
		}
		data, err := os.ReadFile(filepath.Join(directory, item.instance, item.name))
		if err != nil {
			t.Fatal(err)
		}
		recorded += bytes.Count(data, []byte{'\n'})
	}
	if recorded != 4 {
		t.Fatalf("recorded %d", recorded)
	}
}

func TestBatchReservationsUntilCompletion(t *testing.T) {
	source := testSource(t)
	entered, release := make(chan int, 2), make(chan struct{})
	writer := func(batch []queuedLine) []error {
		select {
		case entered <- len(batch):
		default:
		}
		<-release
		return make([]error, len(batch))
	}
	recorder := startRecorder(source, queueLimits{maxQueuedEvents, maxQueuedBytes, maxEventBytes}, nil, func() error { return nil }, nil, newFailureOutput(source, os.Stderr, time.Now), flushBudget, writer)
	event := obs03Event(t, source, Request)
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("emit")
	}
	<-entered
	for range 200 {
		if !recorder.Emit(t.Context(), event) {
			t.Fatal("emit")
		}
	}
	stats := recorder.Stats()
	if stats.QueuedEvents != 201 || stats.Written != 0 || !stats.Writing {
		t.Fatal(stats)
	}
	close(release)
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats = recorder.Stats()
	if stats.Written != 201 || stats.QueuedEvents != 0 || stats.Dropped != 0 {
		t.Fatal(stats)
	}
}

func TestBatchExpiryBoundaryPreservesFreshEvent(t *testing.T) {
	directory := obs02Directory(t)
	files := obs04Files(t, directory, "expiry_slot")
	if err := files.maintain(); err != nil {
		t.Fatal(err)
	}
	old := obs04Line(t, files, Request, obs04Now.Add(-7*24*time.Hour+time.Millisecond))
	fresh := obs04Line(t, files, Request, obs04Now)
	batch := []queuedLine{{Request, old}, {Request, fresh}}
	calls := 0
	files.policy.now = func() time.Time {
		calls++
		// 각 행의 첫 검증 뒤 flush 전에 오래된 행만 보존 경계를 넘긴다.
		if calls <= len(batch) {
			return obs04Now
		}
		return obs04Now.Add(2 * time.Millisecond)
	}
	results := files.appendBatch(batch)
	if failureCode(results[0]) != "event_expired" || results[1] != nil {
		t.Fatalf("expired/fresh results = %v", results)
	}
	got, err := os.ReadFile(filepath.Join(directory, files.source.instance, "request.jsonl"))
	if err != nil || !bytes.Equal(got, fresh) {
		t.Fatalf("fresh event was lost or expired event persisted: %v", err)
	}
}
