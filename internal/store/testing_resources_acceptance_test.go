package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"github.com/jackc/pgx/v5/pgxpool"
)

type test05SearchStore struct {
	*Store
	mu     sync.Mutex
	scopes []*readScope
}

func (database *test05SearchStore) WithReadSnapshot(ctx context.Context, read func(context.Context) error) (model.ReadStats, error) {
	return database.Store.WithReadSnapshot(ctx, func(scoped context.Context) error {
		database.mu.Lock()
		database.scopes = append(database.scopes, scoped.Value(readScopeKey{}).(*readScope))
		database.mu.Unlock()
		return read(scoped)
	})
}

type test05Embedder struct{ vector []float64 }

func (provider test05Embedder) Embed(context.Context, string) ([]float64, error) {
	return provider.vector, nil
}
func (test05Embedder) ModelID() string { return "test05" }

func TestTEST05MixedRequestsRecoverSmallPoolIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	configuration := database.pool.Config()
	database.pool.Close()
	configuration.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	database.pool = pool
	database.reservations, err = newReservations(4)
	if err != nil {
		t.Fatal(err)
	}
	graph, actor, source := boundaryGraph(t, database)
	createDerived := func() model.Context {
		t.Helper()
		value, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	replayed, conflicting := createDerived(), createDerived()
	vector := make([]float64, embeddingDimension(t, database))
	vector[0] = 1
	indexOnce(t, database, graph, source.ID, "test05", vector)
	tracked := &test05SearchStore{Store: database}
	service, err := search.New(tracked, test05Embedder{vector}, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 10, FoldThreshold: 0.95, Consistency: search.ConsistencySnapshot}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 20*time.Second)
	defer stop()
	anchor, err := database.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var releases []func() error
	releaseAnchor := sync.OnceFunc(func() {
		for _, release := range releases {
			if err := release(); err != nil {
				t.Error(err)
			}
		}
		if _, err := anchor.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(releaseAnchor)
	for range 3 {
		_, release, err := database.enterReadScope(anchor.Context(ctx))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	before := pool.Stat()
	if before.MaxConns() != 4 || before.AcquiredConns() != 4 || len(anchor.scope.reads) != 3 {
		t.Fatal("조정 연결과 읽기 세 자리로 실제 최소 풀을 채우지 못했다")
	}
	cause := errors.New("TEST-05 혼합 요청 대기 취소")
	queued, cancelQueued := context.WithCancelCause(ctx)
	defer cancelQueued(cause)
	type outcome struct {
		kind     string
		canceled bool
		err      error
	}
	const perKind = 8
	started := make(chan struct{}, 4*perKind)
	results := make(chan outcome, 4*perKind)
	var workers sync.WaitGroup
	var applied atomic.Int32
	check := func(ctx context.Context) error {
		_, _, err := database.EffectiveGrade(ctx, graph, actor)
		return err
	}
	apply := func(ctx context.Context) ([]byte, error) {
		applied.Add(1)
		next := replayed
		next.Body, next.Version = "멱등성 단일 적용", next.Version+1
		_, err := database.UpdateContext(ctx, graph, replayed.Version, next)
		return []byte(`{"applied":true}`), err
	}
	// 실패 시에도 대기 취소·선행 읽기 종료·작업 회수를 DB 정리보다 먼저 수행한다.
	t.Cleanup(func() { stop(); cancelQueued(cause); releaseAnchor(); workers.Wait() })
	for _, kind := range []string{"search", "replay", "conflict", "periodic"} {
		for i := range perKind {
			workers.Go(func() {
				request := ctx
				canceled := i%2 == 0
				if canceled {
					request = queued
				}
				started <- struct{}{}
				var err error
				switch kind {
				case "search":
					var flow model.ContextFlow
					flow, err = service.Flow(request, model.FlowInput{GraphID: graph, WorkContext: source.Body})
					if err == nil && len(flow.Contexts) == 0 {
						err = errors.New("실제 검색 결과 누락")
					}
				case "replay":
					_, err = database.ExecuteWrite(request, actor, "test05_shared_key", "node_update", []byte(`{"body":"멱등성 단일 적용"}`), check, apply)
				case "conflict":
					next := conflicting
					next.Body, next.Version = fmt.Sprintf("동시 판 갱신 %d", i), next.Version+1
					_, err = database.UpdateContext(request, graph, conflicting.Version, next)
				case "periodic":
					var acquired bool
					_, acquired, err = database.WithTryAdvisoryLock(request, 9500500+int64(i), func(ctx context.Context) (int, error) {
						return database.ExpireGrace(ctx, time.Now().UTC())
					})
					if err == nil && !acquired {
						err = errors.New("서로 다른 주기 작업의 잠금 확보 실패")
					}
				}
				results <- outcome{kind, canceled, err}
			})
		}
	}
	for range 4 * perKind {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	cancelQueued(cause)
	for range 2 * perKind {
		select {
		case result := <-results:
			if !result.canceled || !errors.Is(result.err, cause) || !errors.Is(result.err, context.Canceled) {
				t.Fatalf("취소 대기 요청의 원인 유실: %+v", result)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	afterCancel := pool.Stat()
	if afterCancel.AcquiredConns() != 4 || afterCancel.AcquireCount() != before.AcquireCount() || afterCancel.CanceledAcquireCount() != before.CanceledAcquireCount() || applied.Load() != 0 {
		t.Fatal("예약 대기 중 연결 획득 또는 업무 적용이 발생했다")
	}
	releaseAnchor()
	workers.Wait()
	close(results)
	counts := map[string]int{}
	conflicts := 0
	for result := range results {
		counts[result.kind]++
		if result.kind == "conflict" {
			if _, ok := errors.AsType[VersionConflictError](result.err); ok {
				conflicts++
				continue
			}
		}
		if result.canceled || result.err != nil {
			t.Errorf("선행 해제 뒤 요청 실패: %+v", result)
		}
	}
	for _, kind := range []string{"search", "replay", "conflict", "periodic"} {
		if counts[kind] != perKind/2 {
			t.Errorf("%s 완료 요청 수 = %d", kind, counts[kind])
		}
	}
	if conflicts != 3 || applied.Load() != 1 {
		t.Fatalf("판 충돌 %d, 멱등성 적용 %d", conflicts, applied.Load())
	}
	tracked.mu.Lock()
	scopes := append([]*readScope{anchor.scope}, tracked.scopes...)
	tracked.mu.Unlock()
	if len(scopes) != 5 {
		t.Fatalf("실제 성공 검색 스냅숏 수 = %d", len(scopes)-1)
	}
	for _, scope := range scopes {
		assertConnectionRecovery(t, database, scope, 0)
	}
	var idle, locks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state LIKE 'idle in transaction%'`).Scan(&idle); err != nil || idle != 0 {
		t.Fatalf("활성 트랜잭션 미회복: %d %v", idle, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&locks); err != nil || locks != 0 {
		t.Fatalf("자문 잠금 미반환: %d %v", locks, err)
	}
	for _, target := range []model.Context{replayed, conflicting} {
		value, err := database.Context(ctx, graph, target.ID)
		if err != nil || value.Version != target.Version+1 {
			t.Fatalf("최종 단일 판 갱신: %+v %v", value, err)
		}
	}
	assertConnectionRecovery(t, database, nil, 0)
}
