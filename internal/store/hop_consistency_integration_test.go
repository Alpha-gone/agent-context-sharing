package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type hopReadBarrier struct {
	query  string
	stage  string
	ready  chan struct{}
	resume chan struct{}
	once   sync.Once
}

type hopReadBarrierKey struct{}

type hopReadBarrierTracer struct{}

func (*hopReadBarrierTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	barrier, _ := ctx.Value(hopReadBarrierKey{}).(*hopReadBarrier)
	if barrier != nil && strings.Contains(data.SQL, barrier.query) && strings.Contains(data.SQL, barrier.stage) {
		barrier.once.Do(func() {
			close(barrier.ready)
			select {
			case <-barrier.resume:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}

func (*hopReadBarrierTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// 후보 선택 뒤 본문 조회 전에 별도 트랜잭션의 폐기 커밋을 완료한다.
func TestHopConcurrentDiscardIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	config := database.pool.Config()
	database.pool.Close()
	config.ConnConfig.Tracer = new(hopReadBarrierTracer)
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	database.pool = pool
	for _, mode := range []string{"pool", "snapshot"} {
		for _, entry := range []string{"single", "from"} {
			for _, limit := range []int{0, 2} {
				t.Run(fmt.Sprintf("%s/%s/limit=%d", mode, entry, limit), func(t *testing.T) {
					graph, hub := createHopHub(t, database, 0)
					value := testDerivedContext(t, graph, hub.CreatedBy)
					if _, err := database.CreateContext(t.Context(), graph, value, []model.ID{hub.ID}); err != nil {
						t.Fatal(err)
					}
					before, err := database.HopContexts(t.Context(), graph, hub.ID, 1, "in", []string{"derived_from"}, limit)
					if err != nil {
						t.Fatal(err)
					}
					leaf := before.Contexts[1]
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					var snapshot *ReadSnapshot
					if mode == "snapshot" {
						snapshot, err = database.BeginReadSnapshot(ctx)
						if err != nil {
							t.Fatal(err)
						}
						defer snapshot.Close(context.WithoutCancel(ctx))
						ctx = snapshot.Context(ctx)
					}
					stage := "RETURN node ORDER BY node.context_id"
					if limit == 0 {
						stage = "ORDER BY source.context_id, target.context_id"
					}
					barrier := &hopReadBarrier{query: leaf.ID.String(), stage: stage, ready: make(chan struct{}), resume: make(chan struct{})}
					type outcome struct {
						result HopResult
						err    error
					}
					done := make(chan outcome, 1)
					go func() {
						result, err := callProtectedHop(context.WithValue(ctx, hopReadBarrierKey{}, barrier), database, graph, hub, entry, limit)
						done <- outcome{result, err}
					}()
					select {
					case <-barrier.ready:
					case <-ctx.Done():
						t.Fatal("본문 조회 동기화 대기: ", ctx.Err())
					}
					if _, err := database.DiscardContext(ctx, graph, leaf.ID, nil, WriteLimits{}); err != nil {
						t.Fatal(err)
					}
					close(barrier.resume)
					var got outcome
					select {
					case got = <-done:
					case <-ctx.Done():
						t.Fatal("홉 완료 대기: ", ctx.Err())
					}
					if got.err != nil {
						t.Fatalf("시작 노드가 존재하는 동시 폐기: err=%v not_found=%v empty=%v", got.err, errors.Is(got.err, ErrNotFound), reflect.DeepEqual(got.result, HopResult{}))
					}
					compareHopResults(t, "동시 폐기 전 커밋 상태", before, got.result)
					want := int32(0)
					if snapshot != nil {
						want = 1
					}
					waitHopResources(t, database, snapshot, want)
					after, err := database.HopContexts(t.Context(), graph, hub.ID, 1, "in", []string{"derived_from"}, 2)
					if err != nil || len(after.Contexts) != 1 || len(after.Edges) != 0 || after.Truncated {
						t.Fatalf("폐기 뒤 새 조회: %+v, %v", after, err)
					}
				})
			}
		}
	}
}

func TestHopStartReadBoundaryIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub := createHopHub(t, database, 0)
	snapshot, err := database.BeginReadSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close(context.WithoutCancel(t.Context()))
	if _, err := database.DiscardContext(t.Context(), graph, hub.ID, nil, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"single", "from"} {
		for _, limit := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/limit=%d", entry, limit), func(t *testing.T) {
				// 검색은 기존 상태를 유지하고 독립된 새 조회는 폐기를 인식한다.
				got, err := callProtectedHop(snapshot.Context(t.Context()), database, graph, hub, entry, limit)
				if err != nil || len(got.Contexts) != 1 || got.Contexts[0].DeletedAt != nil {
					t.Fatalf("검색의 시작 상태: %+v, %v", got, err)
				}
				for _, start := range []model.Context{hub, {ID: newTestID(t), GraphID: graph}} {
					got, err := callProtectedHop(t.Context(), database, graph, start, entry, limit)
					if !errors.Is(err, ErrNotFound) || !reflect.DeepEqual(got, HopResult{}) {
						t.Fatalf("폐기·부재 시작 노드: %+v, %v", got, err)
					}
				}
				waitHopResources(t, database, snapshot, 1)
			})
		}
	}
}

func TestStandaloneHopFitsSmallPoolIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, hub := createHopHub(t, database, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 12)
	for i := range 12 {
		go func() {
			entry := "single"
			if i%2 == 1 {
				entry = "from"
			}
			result, err := callProtectedHop(ctx, database, graph, hub, entry, 2)
			if err == nil && (len(result.Contexts) != 2 || len(result.Edges) != 1 || result.Truncated) {
				err = fmt.Errorf("최소 풀 홉 결과: %+v", result)
			}
			done <- err
		}()
	}
	for range 12 {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Fatal("최소 풀 홉 완료 대기: ", ctx.Err())
		}
	}
	waitHopResources(t, database, nil, 0)
}
