//go:build linux || darwin

package diagnostic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOPS04MaintenanceStorageScopeIntegration(t *testing.T) {
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_URL이 필요합니다")
		}
		t.Skip("TEST_DATABASE_URL이 없습니다")
	}
	first, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	second, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	lease := func(ctx context.Context, pool *pgxpool.Pool, key int64, run func(context.Context) (int, error)) (int, bool, error) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return 0, false, err
		}
		defer conn.Release()
		var held bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&held); err != nil {
			return 0, false, err
		}
		if !held {
			return 0, false, nil
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			if _, err := conn.Exec(cleanup, `SELECT pg_advisory_unlock($1)`, key); err != nil {
				t.Error(err)
			}
		}()
		count, err := run(ctx)
		return count, true, err
	}
	rootA, rootB := obs02Directory(t), obs02Directory(t)
	source, err := NewSource("ops04")
	if err != nil {
		t.Fatal(err)
	}
	makeRecorder := func(root string) *Recorder {
		t.Helper()
		r, err := prepareRecorder(t.Context(), root, source, obs04Policy())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := r.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		return r
	}
	a, b := makeRecorder(rootA), makeRecorder(rootB)
	if a.upkeep.scope == b.upkeep.scope {
		t.Fatal("다른 로그 저장소의 실행권을 구분하지 못했습니다")
	}
	otherSource, err := NewSource("ops04_other")
	if err != nil {
		t.Fatal(err)
	}
	otherSlot, err := prepareRecorder(t.Context(), rootA, otherSource, obs04Policy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := otherSlot.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if otherSlot.upkeep.scope == a.upkeep.scope {
		t.Fatal("같은 저장소의 다른 인스턴스 실행권이 같습니다")
	}
	removedA, removedB := 0, 0
	a.SetMaintenanceGuard(func(ctx context.Context, key int64, run func() (int, error)) (int, bool, error) {
		count, held, err := lease(ctx, second, key, func(context.Context) (int, error) { return run() })
		removedA += count
		return count, held, err
	})
	b.SetMaintenanceGuard(func(ctx context.Context, key int64, run func() (int, error)) (int, bool, error) {
		count, held, err := lease(ctx, second, key, func(context.Context) (int, error) { return run() })
		removedB += count
		return count, held, err
	})
	stale := func(root string) string {
		t.Helper()
		at := obs04Now.Add(-8 * 24 * time.Hour)
		event := obs03Event(t, source, Request)
		event.Time = at
		line, _, ok := encodeLine(event, maxEventBytes)
		if !ok {
			t.Fatal("이벤트 직렬화")
		}
		name := filepath.Join(root, source.instance, "request."+at.Format(rotationTimeLayout)+"."+uuid.NewV7().String()+".jsonl")
		if err := os.WriteFile(name, line, 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	pathA, pathB := stale(rootA), stale(rootB)
	_, held, err := lease(t.Context(), first, a.upkeep.scope, func(context.Context) (int, error) {
		a.maintain()
		b.maintain()
		return 0, nil
	})
	if err != nil || !held || removedA != 0 || removedB != 1 {
		t.Fatal("저장소별 실행권 경합·건수", held, removedA, removedB, err)
	}
	if _, err := os.Stat(pathA); err != nil {
		t.Fatal("잠금을 얻지 못한 파일을 지웠습니다", err)
	}
	if _, err := os.Stat(pathB); !os.IsNotExist(err) {
		t.Fatal("다른 저장소 정리가 전역 잠금 때문에 제외됐습니다", err)
	}
	a.maintain()
	if removedA != 1 {
		t.Fatal("건너뛴 뒤 다음 정리 회차", removedA)
	}
	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Fatal("보존이 끝난 파일을 정리하지 못했습니다", err)
	}
	scope := a.upkeep.scope
	if err := a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(rootA, "..", filepath.Base(rootA))
	reopened := makeRecorder(alias)
	if reopened.upkeep.scope != scope {
		t.Fatal("같은 저장소 재시작에서 실행권이 바뀌었습니다")
	}
	if a.Stats().MaintenanceFailures != 0 || b.Stats().MaintenanceFailures != 0 {
		t.Fatal("실행권 건너뜀을 파일 장애로 셌습니다")
	}
}

func TestOPS04MaintenanceCancellationDoesNotHideFailure(t *testing.T) {
	cause := errors.New("private cleanup failure")
	for _, test := range []struct {
		err  error
		only bool
	}{
		{context.Canceled, true},
		{fmt.Errorf("wrapped: %w", errors.Join(context.Canceled, context.Canceled)), true},
		{errors.Join(context.Canceled, cause), false},
		{fmt.Errorf("wrapped: %w", errors.Join(context.Canceled, cause)), false},
		{context.DeadlineExceeded, false},
	} {
		if onlyMaintenanceCancellation(test.err) != test.only {
			t.Fatal("의도적인 취소와 동반 실패를 구분하지 못했습니다", test.only)
		}
	}
	source, err := NewSource("ops04_failure")
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := prepareRecorder(t.Context(), obs02Directory(t), source, obs04Policy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recorder.Close(context.Background()) })
	started, done := make(chan struct{}), make(chan struct{})
	recorder.SetMaintenanceGuard(func(ctx context.Context, _ int64, _ func() (int, error)) (int, bool, error) {
		close(started)
		<-ctx.Done()
		return 0, true, errors.Join(ctx.Err(), cause)
	})
	go func() { recorder.maintain(); close(done) }()
	<-started
	if err := recorder.StopMaintenance(t.Context()); !errors.Is(err, cause) {
		t.Fatal("작업자 정리가 동반 실패를 반환하지 않았습니다", err)
	}
	<-done
	if err := recorder.Close(t.Context()); !errors.Is(err, cause) {
		t.Fatal("동반 정리 실패 원인을 보존하지 않았습니다", err)
	}
	if recorder.Stats().MaintenanceFailures != 1 {
		t.Fatal("동반 정리 실패를 누락했습니다")
	}
}

func TestOPS04StopMaintenanceKeepsRecording(t *testing.T) {
	source, err := NewSource("ops04_stop")
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := prepareRecorder(t.Context(), obs02Directory(t), source, obs04Policy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := recorder.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	started, released, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	recorder.SetMaintenanceGuard(func(ctx context.Context, _ int64, _ func() (int, error)) (int, bool, error) {
		close(started)
		<-ctx.Done()
		<-released
		return 0, true, ctx.Err()
	})
	go func() { recorder.maintain(); close(done) }()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := recorder.StopMaintenance(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("정리 대기 마감", err)
	}
	close(released)
	<-done
	if err := recorder.StopMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	recorder.maintain() // 중단한 실행권을 다시 호출하면 started 중복 close로 실패한다.
	event := obs03Event(t, source, Request)
	event.Time = obs04Now
	if !recorder.Emit(t.Context(), event) {
		t.Fatal("시간 기반 정리 중단 뒤 기록을 거부했습니다")
	}
	if err := recorder.Close(t.Context()); err != nil {
		t.Fatal("정리의 의도적 취소를 종료 실패로 셌습니다", err)
	}
	if stats := recorder.Stats(); stats.Written != 1 || stats.MaintenanceFailures != 0 {
		t.Fatal("정리와 기록 수명의 분리", stats)
	}
}
