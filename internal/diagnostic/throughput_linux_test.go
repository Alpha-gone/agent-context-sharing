package diagnostic

import (
	"context"
	"testing"
	"time"
)

// Linux 실제 볼륨의 고정 부하이며 거부된 Emit을 재시도하지 않는다.
func TestDiagnosticThroughputLinux(t *testing.T) {
	source := testSource(t)
	recorder, err := NewRecorder(t.Context(), obs02Directory(t), source)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close(context.Background())
	events := make([]Event, len(fileKinds))
	for i, kind := range fileKinds {
		events[i] = obs03Event(t, source, kind)
		events[i].Time = time.Now().UTC()
	}
	const count = 5000
	start := time.Now()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for base := 0; base < count; base += 50 {
		<-ticker.C
		for i := base; i < base+50; i++ {
			recorder.Emit(t.Context(), events[i%len(events)])
		}
	}
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	stats := recorder.Stats()
	rate := float64(stats.Written) / elapsed.Seconds()
	t.Logf("Linux volume: offered=%d accepted=%d written=%d dropped=%d elapsed=%s throughput=%.0f events/s target>=1500", count, stats.Accepted, stats.Written, stats.Dropped, elapsed, rate)
	if stats.Written != count || stats.Dropped != 0 || rate < 1500 {
		t.Fatalf("throughput target unmet: %+v rate=%.0f", stats, rate)
	}
}

// 생산 속도를 큐의 여유 공간에 맞춰 파일 경로의 포화 처리량을 별도로 측정한다.
func TestDiagnosticSaturatedThroughputLinux(t *testing.T) {
	source := testSource(t)
	recorder, err := NewRecorder(t.Context(), obs02Directory(t), source)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close(context.Background())
	events := make([]Event, len(fileKinds))
	for i, kind := range fileKinds {
		events[i] = obs03Event(t, source, kind)
		events[i].Time = time.Now().UTC()
	}
	const count = 5000
	start := time.Now()
	for base := 0; base < count; base += 100 {
		for recorder.Stats().QueuedEvents > maxQueuedEvents-100 {
			if time.Since(start) > 10*time.Second {
				t.Fatal("saturation deadline")
			}
			time.Sleep(100 * time.Microsecond)
		}
		for i := base; i < base+100; i++ {
			if !recorder.Emit(t.Context(), events[i%len(events)]) {
				t.Fatal("reserved queue unexpectedly full")
			}
		}
	}
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	stats := recorder.Stats()
	rate := float64(stats.Written) / elapsed.Seconds()
	t.Logf("Linux volume saturation: written=%d dropped=%d elapsed=%s throughput=%.0f events/s", stats.Written, stats.Dropped, elapsed, rate)
	if stats.Written != count || stats.Dropped != 0 {
		t.Fatal(stats)
	}
}
