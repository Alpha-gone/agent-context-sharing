package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSnapshotCandidateAccessFitsSmallPoolIntegration은 실제 후보와 cold 임베딩을 가진
// 세 채널이 조정 연결과 읽기 연결 셋을 모두 잡은 뒤에도 추가 연결을 기다리지 않는지 확인한다.
func TestSnapshotCandidateAccessFitsSmallPoolIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	value := testSourceContext(t, graphID, actorID, "api://snapshot-access/"+graphID.String())
	value.Body = "snapshot access reservation"
	source, err := database.CreateContext(t.Context(), graphID, value, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanupIndexTasks(t, database, source.ID)
	readyIndexTasks(t, database, source.ID)
	vector := make([]float64, embeddingDimension(t, database))
	vector[0] = 1
	indexed, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, func(context.Context, IndexTask) IndexTaskResult {
		return IndexTaskResult{Embedding: vector, ModelID: "snapshot-access-test"}
	})
	if err != nil || !indexed.Succeeded {
		t.Fatalf("시험 임베딩 색인 = %+v, %v", indexed, err)
	}
	oldAccess := time.Now().UTC().Add(-time.Hour)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET storage_tier = 'cold', last_accessed_at = $2 WHERE context_id = $1`, source.ID.String(), oldAccess); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	snapshot, err := database.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close(context.WithoutCancel(ctx))
	scoped := snapshot.Context(ctx)
	current := time.Now().UTC()
	channels := []func(context.Context) ([]SearchCandidate, error){
		func(ctx context.Context) ([]SearchCandidate, error) {
			return database.KeywordCandidates(ctx, graphID, "reservation", current, 5)
		},
		func(ctx context.Context) ([]SearchCandidate, error) {
			return database.TimeCandidates(ctx, graphID, current, 5)
		},
		func(ctx context.Context) ([]SearchCandidate, error) {
			return database.SemanticCandidates(ctx, graphID, "snapshot-access-test", vector, current, 5)
		},
	}
	ready := make(chan error, len(channels))
	start := make(chan struct{})
	errs := make(chan error, len(channels))
	var group sync.WaitGroup
	for _, channel := range channels {
		group.Go(func() {
			readContext, release, err := database.enterReadScope(scoped)
			ready <- err
			if err != nil {
				errs <- err
				return
			}
			defer func() {
				if err := release(); err != nil {
					t.Errorf("읽기 종료와 접근 기록: %v", err)
				}
			}()
			// 3개의 실제 읽기 트랜잭션이 모두 열린 뒤 채널의 조회·조립·접근 기록을 실행한다.
			<-start
			candidates, err := channel(readContext)
			if err == nil && (len(candidates) != 1 || candidates[0].Context.ID != source.ID) {
				err = fmt.Errorf("검색 후보 = %+v, want %s 한 개", candidates, source.ID)
			}
			errs <- err
		})
	}
	for range channels {
		if err := <-ready; err != nil {
			t.Error(err)
		}
	}
	if acquired := database.pool.Stat().AcquiredConns(); acquired != snapshotConnections {
		t.Errorf("동시에 확보한 실제 풀 연결 = %d, want %d", acquired, snapshotConnections)
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("실제 후보를 가진 스냅숏 채널: %v", err)
		}
	}
	if t.Failed() {
		return
	}
	if tier := embeddingTier(t, database, source.ID); tier != embeddingTierHot {
		t.Fatalf("검색 뒤 임베딩 계층 = %q, want hot", tier)
	}
	var accessed time.Time
	if err := database.pool.QueryRow(t.Context(), `SELECT last_accessed_at FROM public.context_embedding WHERE context_id = $1`, source.ID.String()).Scan(&accessed); err != nil {
		t.Fatal(err)
	}
	if !accessed.After(oldAccess) {
		t.Fatalf("검색 접근 시각이 갱신되지 않았다: %v", accessed)
	}
	snapshot.scope.mu.Lock()
	peak := snapshot.scope.peak
	snapshot.scope.mu.Unlock()
	if peak != snapshotConnections {
		t.Fatalf("동시 읽기 연결 최대 = %d, want %d", peak, snapshotConnections)
	}
	if acquired := database.pool.Stat().AcquiredConns(); acquired != 1 {
		t.Fatalf("채널 종료 뒤 실제 풀 연결 = %d, want 조정 연결 1개", acquired)
	}
	if !database.reservations.TryAcquire(1) {
		// 예약 4개가 유지되므로 여분 예약이 없어야 한다.
		return
	}
	database.reservations.Release(1)
	t.Fatal("스냅숏이 연결 예약을 너무 일찍 반납했다")
}

// TestSnapshotAccessKeepsReadSlotIntegration은 읽기 연결을 반환한 뒤에도 접근 기록이
// 끝나기 전에는 새 읽기가 그 자리를 차지하지 않는지 실제 풀 점유와 함께 확인한다.
func TestSnapshotAccessKeepsReadSlotIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	tracer := &snapshotAccessTracer{ready: make(chan struct{}, 1), proceed: make(chan struct{})}
	installSnapshotAccessTracer(t, database, tracer)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	snapshot, err := database.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close(context.WithoutCancel(ctx))
	scoped := snapshot.Context(ctx)
	releases := make([]func() error, 0, 3)
	for range 3 {
		readContext, release, err := database.enterReadScope(scoped)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = release() })
		releases = append(releases, release)
		if len(releases) == 1 {
			if err := database.touchEmbeddings(readContext, newTestID(t), []model.ID{newTestID(t)}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		}
	}
	done := make(chan error, 1)
	go func() { done <- releases[0]() }()
	select {
	case <-tracer.ready:
	case <-ctx.Done():
		t.Fatal("접근 기록 연결을 확보하지 못했다")
	}
	if acquired := database.pool.Stat().AcquiredConns(); acquired != snapshotConnections {
		t.Errorf("조정·남은 읽기·접근 기록의 실제 연결 = %d, want %d", acquired, snapshotConnections)
	}
	if slots := len(snapshot.scope.reads); slots != snapshotConnections-1 {
		t.Errorf("접근 기록 중 유지한 읽기 자리 = %d, want %d", slots, snapshotConnections-1)
	}
	waiting, stop := context.WithTimeout(scoped, 200*time.Millisecond)
	defer stop()
	if _, release, err := database.enterReadScope(waiting); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "스냅숏 읽기 자리 대기") {
		_ = release()
		t.Errorf("접근 기록 중 네 번째 읽기 = %v, want 자리 대기 취소", err)
	}
	close(tracer.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, release, err := database.enterReadScope(scoped)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	for _, release := range releases[1:] {
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSnapshotAccessFailurePropagatesIntegration은 접근 기록 취소를 성공 후보로 숨기지
// 않고 읽기 호출의 오류로 전달하며, 실패한 읽기의 연결과 자리가 반환되는지 확인한다.
func TestSnapshotAccessFailurePropagatesIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	installSnapshotAccessTracer(t, database, &snapshotAccessTracer{fail: true})
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "api://snapshot-access-failure/"+graphID.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanupIndexTasks(t, database, source.ID)
	snapshot, err := database.BeginReadSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close(context.WithoutCancel(t.Context()))
	scoped := snapshot.Context(t.Context())
	t.Run("keyword", func(t *testing.T) {
		candidates, err := database.KeywordCandidates(scoped, graphID, source.Body, time.Now().UTC(), 5)
		if !errors.Is(err, context.Canceled) || candidates != nil {
			t.Fatalf("접근 기록 실패의 키워드 후보 = %+v, %v", candidates, err)
		}
	})
	t.Run("time", func(t *testing.T) {
		candidates, err := database.TimeCandidates(scoped, graphID, time.Now().UTC(), 5)
		if !errors.Is(err, context.Canceled) || candidates != nil {
			t.Fatalf("접근 기록 실패의 후보 = %+v, %v", candidates, err)
		}
	})
	t.Run("hop", func(t *testing.T) {
		hop, err := database.HopContextsFrom(scoped, graphID, []model.Context{source}, 0, "both", nil, 0)
		if !errors.Is(err, context.Canceled) || len(hop.Contexts) != 0 {
			t.Fatalf("접근 기록 실패의 홉 결과 = %+v, %v", hop, err)
		}
	})
	t.Run("origins", func(t *testing.T) {
		origins, err := database.ContextOriginKinds(scoped, graphID, []model.ID{source.ID})
		if !errors.Is(err, context.Canceled) || origins != nil {
			t.Fatalf("접근 기록 실패의 출처 = %+v, %v", origins, err)
		}
	})
	t.Run("request_cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(scoped)
		defer cancel()
		readContext, release, err := database.enterReadScope(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = release() })
		if err := database.touchEmbeddings(readContext, graphID, []model.ID{source.ID}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := release(); !errors.Is(err, context.Canceled) {
			t.Fatalf("취소된 읽기의 접근 기록 = %v", err)
		}
	})
	if acquired := database.pool.Stat().AcquiredConns(); acquired != 1 || len(snapshot.scope.reads) != 0 {
		t.Fatalf("기록 실패 후 연결 = %d, 읽기 자리 = %d", acquired, len(snapshot.scope.reads))
	}
}

type snapshotAccessTracer struct {
	ready   chan struct{}
	proceed chan struct{}
	fail    bool
}

func (tracer *snapshotAccessTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(strings.TrimSpace(data.SQL), "UPDATE public.context_embedding AS embedding") {
		return ctx
	}
	if tracer.fail {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		return ctx
	}
	tracer.ready <- struct{}{}
	select {
	case <-tracer.proceed:
	case <-ctx.Done():
	}
	return ctx
}

func (*snapshotAccessTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func installSnapshotAccessTracer(t *testing.T, database *Store, tracer *snapshotAccessTracer) {
	t.Helper()
	config := database.pool.Config()
	database.pool.Close()
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	database.pool = pool
}
