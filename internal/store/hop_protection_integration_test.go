package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// waitHopResources는 pgx의 지연 연결 정리를 허용하되 유한 시간 안에 누수를 검출한다.
// 스냅숏 조정 연결과 시험 잠금 연결은 want에 포함한다.
func waitHopResources(t *testing.T, database *Store, snapshot *ReadSnapshot, want int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		acquired := database.pool.Stat().AcquiredConns()
		slots, active := 0, 0
		if snapshot != nil {
			slots = len(snapshot.scope.reads)
			snapshot.scope.mu.Lock()
			active = snapshot.scope.active
			snapshot.scope.mu.Unlock()
		}
		if acquired == want && slots == 0 && (snapshot == nil || active == 1) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("자원 미회복: acquired=%d want=%d read-slots=%d active=%d", acquired, want, slots, active)
		case <-ticker.C:
		}
	}
}

func callProtectedHop(ctx context.Context, database *Store, graph model.ID, hub model.Context, entry string, limit int) (HopResult, error) {
	if entry == "single" {
		return database.HopContexts(ctx, graph, hub.ID, 1, "in", []string{"derived_from"}, limit)
	}
	return database.HopContextsFrom(ctx, graph, []model.Context{hub}, 1, "in", []string{"derived_from"}, limit)
}

func TestHopOwnBudgetAndConnectionIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub := createHopHub(t, database, 1)
	for _, mode := range []string{"pool", "snapshot"} {
		for _, entry := range []string{"single", "from"} {
			for _, interruption := range []string{"cancel", "deadline", "own-budget"} {
				// 실제 30초 소진은 시작 조회(single)와 후속 후보 조회(from)를 각각 막는다.
				// 나머지 경로의 내부 예산은 아래 질의 마감 대조 시험에서도 직접 관찰한다.
				if interruption == "own-budget" && ((mode == "pool") != (entry == "single")) {
					continue
				}
				t.Run(mode+"/"+entry+"/"+interruption, func(t *testing.T) {
					base := database.pool.Stat().AcquiredConns()
					ctx := t.Context()
					var snapshot *ReadSnapshot
					if mode == "snapshot" {
						var err error
						snapshot, err = database.BeginReadSnapshot(ctx)
						if err != nil {
							t.Fatal(err)
						}
						defer func() {
							if snapshot != nil {
								snapshot.Close(context.WithoutCancel(t.Context()))
							}
						}()
						ctx = snapshot.Context(ctx)
					}
					before := database.pool.Stat().AcquiredConns()
					tx, err := database.pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(context.WithoutCancel(t.Context()))
					if _, err := tx.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{database.graphName, "Context"}.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
						t.Fatal(err)
					}
					// 내부 예산을 제거한 회귀도 유한 시간 안에 끝나도록 40초 감시 마감을 둔다.
					ctx, guard := context.WithTimeout(ctx, 40*time.Second)
					defer guard()
					want := context.DeadlineExceeded
					if interruption == "deadline" {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
						defer cancel()
					}
					if interruption == "cancel" {
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						defer cancel()
						stop := time.AfterFunc(100*time.Millisecond, cancel)
						defer stop.Stop()
						want = context.Canceled
					}
					started := time.Now()
					result, err := callProtectedHop(ctx, database, graph, hub, entry, 2)
					elapsed := time.Since(started)
					if !errors.Is(err, want) || !reflect.DeepEqual(result, HopResult{}) {
						t.Fatalf("중단의 빈 결과·원인: %+v, %v", result, err)
					}
					if interruption == "own-budget" {
						if elapsed < 29*time.Second || elapsed > 35*time.Second {
							t.Fatalf("내부 30초 예산: %s", elapsed)
						}
					} else if elapsed > 5*time.Second {
						t.Fatalf("호출자의 짧은 중단이 우선하지 않았다: %s", elapsed)
					}
					waitHopResources(t, database, snapshot, before+1)
					if err := tx.Rollback(t.Context()); err != nil {
						t.Fatal(err)
					}
					waitHopResources(t, database, snapshot, before)
					resume := t.Context()
					if snapshot != nil {
						resume = snapshot.Context(resume)
					}
					if _, err := callProtectedHop(resume, database, graph, hub, entry, 2); err != nil {
						t.Fatalf("중단 뒤 재사용: %v", err)
					}
					waitHopResources(t, database, snapshot, before)
					t.Logf("elapsed=%s acquired=%d read-slots=0", elapsed, before)
					if snapshot != nil {
						snapshot.Close(context.WithoutCancel(t.Context()))
						snapshot = nil
					}
					waitHopResources(t, database, nil, base)
				})
			}
		}
	}
}

type hopBudgetTraceKey struct{}

type hopBudgetObservation struct {
	deadline time.Time
	kind     string
}

type hopBudgetTracer struct {
	mu           sync.Mutex
	observations []hopBudgetObservation
	cancelKind   string
}

func (tracer *hopBudgetTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(hopBudgetTraceKey{}) != tracer {
		return ctx
	}
	kind := ""
	switch {
	case strings.Contains(data.SQL, "RETURN source.context_id, target.context_id"):
		kind = "references"
	case strings.Contains(data.SQL, "cypher("):
		kind = "graph"
	case strings.HasPrefix(strings.TrimSpace(data.SQL), "UPDATE public.context_embedding AS embedding"):
		kind = "access"
	default:
		return ctx // 트랜잭션 정리는 취소 없이 수행하므로 마감 대조 대상이 아니다.
	}
	deadline, _ := ctx.Deadline()
	tracer.mu.Lock()
	first := len(tracer.observations) == 0
	tracer.observations = append(tracer.observations, hopBudgetObservation{deadline: deadline, kind: kind})
	tracer.mu.Unlock()
	if kind == tracer.cancelKind {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return canceled
	}
	if first {
		// 시작 조회 뒤 예산을 새로 시작하면 동일 마감 검사가 반드시 달라지게 한다.
		timer := time.NewTimer(75 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*hopBudgetTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func installHopBudgetTracer(t *testing.T, database *Store, tracer *hopBudgetTracer) {
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

func TestHopSharedBudgetIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub := createHopHub(t, database, 1)
	tracer := new(hopBudgetTracer)
	installHopBudgetTracer(t, database, tracer)
	for _, mode := range []string{"pool", "snapshot"} {
		for _, entry := range []string{"single", "from"} {
			for _, limit := range []int{0, 2} {
				for _, budget := range []time.Duration{40 * time.Second, 3 * time.Second} {
					t.Run(fmt.Sprintf("%s/%s/limit=%d/caller=%s", mode, entry, limit, budget), func(t *testing.T) {
						tracer.mu.Lock()
						tracer.observations = nil
						tracer.mu.Unlock()
						base := t.Context()
						if mode == "snapshot" {
							snapshot, err := database.BeginReadSnapshot(base)
							if err != nil {
								t.Fatal(err)
							}
							defer snapshot.Close(context.WithoutCancel(base))
							base = snapshot.Context(base)
						}
						ctx, cancel := context.WithTimeout(context.WithValue(base, hopBudgetTraceKey{}, tracer), budget)
						defer cancel()
						callerDeadline, _ := ctx.Deadline()
						started := time.Now()
						if _, err := callProtectedHop(ctx, database, graph, hub, entry, limit); err != nil {
							t.Fatal(err)
						}
						tracer.mu.Lock()
						observations := slices.Clone(tracer.observations)
						tracer.mu.Unlock()
						if len(observations) < 4 {
							t.Fatalf("조립 질의 관찰 부족: %d", len(observations))
						}
						first := observations[0].deadline
						if budget < 30*time.Second {
							if !first.Equal(callerDeadline) {
								t.Fatal("호출자의 더 짧은 마감을 바꿨다")
							}
						} else if remaining := first.Sub(started); remaining < 29*time.Second || remaining > 31*time.Second {
							t.Fatalf("첫 조회의 내부 30초 예산 누락: %s", remaining)
						}
						kinds := make(map[string]bool)
						for _, observation := range observations {
							if !observation.deadline.Equal(first) {
								t.Fatalf("%s 질의에서 공통 예산을 다시 시작하거나 누락했다", observation.kind)
							}
							kinds[observation.kind] = true
						}
						if !kinds["graph"] || !kinds["references"] || !kinds["access"] {
							t.Fatalf("시작·후보·참조·접근 조립의 관찰 누락: %v", kinds)
						}
						t.Logf("shared-deadline queries=%d caller=%s limit=%d", len(observations), budget, limit)
					})
				}
			}
		}
	}
}

func TestHopAssemblyCancellationIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub := createHopHub(t, database, 1)
	for _, mode := range []string{"pool", "snapshot"} {
		for _, stage := range []string{"references", "access"} {
			t.Run(mode+"/"+stage, func(t *testing.T) {
				tracer := &hopBudgetTracer{cancelKind: stage}
				installHopBudgetTracer(t, database, tracer)
				ctx := t.Context()
				var snapshot *ReadSnapshot
				if mode == "snapshot" {
					var err error
					snapshot, err = database.BeginReadSnapshot(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer snapshot.Close(context.WithoutCancel(ctx))
					ctx = snapshot.Context(ctx)
				}
				before := database.pool.Stat().AcquiredConns()
				ctx = context.WithValue(ctx, hopBudgetTraceKey{}, tracer)
				result, err := callProtectedHop(ctx, database, graph, hub, "from", 2)
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, HopResult{}) {
					t.Fatalf("%s 조립 중 취소의 빈 결과: %+v, %v", stage, result, err)
				}
				waitHopResources(t, database, snapshot, before)
				tracer.mu.Lock()
				observations := slices.Clone(tracer.observations)
				tracer.mu.Unlock()
				if len(observations) == 0 || observations[len(observations)-1].kind != stage {
					t.Fatalf("조립 중단 지점 미관찰: %s", stage)
				}
			})
		}
	}
}
