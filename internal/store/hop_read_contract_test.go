package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestHopCanceledBeforeReadPreservesCause(t *testing.T) {
	graph, start := newTestID(t), newTestID(t)
	for _, entry := range []string{"single", "from"} {
		for _, interruption := range []string{"cancel", "deadline"} {
			t.Run(entry+"/"+interruption, func(t *testing.T) {
				cause := errors.New("홉 호출 전 중단 원인")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				want := context.Canceled
				if interruption == "deadline" {
					var end context.CancelFunc
					ctx, end = context.WithDeadlineCause(ctx, time.Now().Add(-time.Second), cause)
					defer end()
					want = context.DeadlineExceeded
				} else {
					cancel(cause)
				}
				// 연결조차 준비하지 않은 저장소로 DB 접근 전에 중단됨을 확인한다.
				result, err := callProtectedHop(ctx, new(Store), graph, model.Context{ID: start, GraphID: graph}, entry, 2)
				if !errors.Is(err, want) || !errors.Is(err, cause) || !reflect.DeepEqual(result, HopResult{}) {
					t.Fatalf("중단 원인과 빈 결과: %+v, %v", result, err)
				}
			})
		}
	}
}

type hopFailureTraceKey struct{}

type hopFailureTracer struct {
	stage       string
	cancel      context.CancelCauseFunc
	cause       error
	observed    bool
	cleanupLive bool
}

func (tracer *hopFailureTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(hopFailureTraceKey{}) != tracer {
		return ctx
	}
	query := strings.TrimSpace(data.SQL)
	rollback := strings.EqualFold(query, "rollback")
	if rollback {
		deadline, ok := ctx.Deadline()
		tracer.cleanupLive = ctx.Err() == nil && ok && time.Until(deadline) > 0 && time.Until(deadline) <= 5*time.Second
	}
	if tracer.observed {
		return ctx
	}
	match := (tracer.stage == "graph" && strings.Contains(query, "cypher(")) ||
		(tracer.stage == "access" && strings.HasPrefix(query, "UPDATE public.context_embedding AS embedding")) ||
		(tracer.stage == "rollback" && rollback)
	if !match {
		return ctx
	}
	tracer.observed = true
	if rollback {
		// 요청은 유효한 채로 두고 실제 읽기 트랜잭션의 종료만 실패시킨다.
		failed, cancel := context.WithCancel(ctx)
		cancel()
		return failed
	}
	tracer.cancel(tracer.cause)
	return ctx
}

func (*hopFailureTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestHopReadFailuresReturnEmptyAndReleaseIntegration(t *testing.T) {
	for _, mode := range []string{"pool", "snapshot"} {
		for _, entry := range []string{"single", "from"} {
			for _, stage := range []string{"graph", "rollback", "access"} {
				t.Run(mode+"/"+entry+"/"+stage, func(t *testing.T) {
					database := newSmallPoolStore(t)
					graph, hub := createHopHub(t, database, 1)
					config := database.pool.Config()
					database.pool.Close()
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					tracer := &hopFailureTracer{stage: stage, cancel: cancel, cause: errors.New("홉 실행 중 취소 원인")}
					config.ConnConfig.Tracer = tracer
					pool, err := pgxpool.NewWithConfig(t.Context(), config)
					if err != nil {
						t.Fatal(err)
					}
					database.pool = pool
					var snapshot *ReadSnapshot
					if mode == "snapshot" {
						snapshot, err = database.BeginReadSnapshot(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						defer snapshot.Close(t.Context())
						ctx = snapshot.Context(ctx)
					}
					before := database.pool.Stat().AcquiredConns()
					result, err := callProtectedHop(context.WithValue(ctx, hopFailureTraceKey{}, tracer), database, graph, hub, entry, 2)
					if !tracer.observed || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, HopResult{}) {
						t.Fatalf("%s 실패의 빈 결과: observed=%t result=%+v err=%v", stage, tracer.observed, result, err)
					}
					if stage != "rollback" && !errors.Is(err, tracer.cause) {
						t.Fatalf("호출자의 취소 원인 누락: %v", err)
					}
					if !tracer.cleanupLive {
						t.Fatal("읽기 종료에 취소 독립적인 5초 정리 예산을 적용하지 않았다")
					}
					waitHopResources(t, database, snapshot, before)
					if snapshot != nil {
						if _, err := snapshot.Close(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					assertConnectionRecovery(t, database, nil, 0)
					if _, err := callProtectedHop(t.Context(), database, graph, hub, entry, 2); err != nil {
						t.Fatalf("실패 뒤 새 홉: %v", err)
					}
				})
			}
		}
	}
}

func TestHopReadWaitBudgetIntegration(t *testing.T) {
	for _, mode := range []string{"pool", "snapshot"} {
		for _, entry := range []string{"single", "from"} {
			t.Run(mode+"/"+entry, func(t *testing.T) {
				database := newSmallPoolStore(t)
				graph, hub := createHopHub(t, database, 0)
				ctx := t.Context()
				var snapshot *ReadSnapshot
				var releases []func()
				defer func() {
					for _, release := range releases {
						release()
					}
				}()
				if mode == "snapshot" {
					var err error
					snapshot, err = database.BeginReadSnapshot(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer snapshot.Close(t.Context())
					ctx = snapshot.Context(ctx)
					for range snapshotConnections - 1 {
						_, finish, err := database.enterReadScope(ctx)
						if err != nil {
							t.Fatal(err)
						}
						releases = append(releases, func() {
							if err := finish(); err != nil {
								t.Error(err)
							}
						})
					}
				} else {
					for range database.pool.Stat().MaxConns() {
						conn, err := database.pool.Acquire(ctx)
						if err != nil {
							t.Fatal(err)
						}
						releases = append(releases, conn.Release)
					}
				}
				cause := errors.New("홉 자리 대기 마감 원인")
				waiting, cancel := context.WithTimeoutCause(ctx, 100*time.Millisecond, cause)
				defer cancel()
				result, err := callProtectedHop(waiting, database, graph, hub, entry, 2)
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) || !reflect.DeepEqual(result, HopResult{}) {
					t.Fatalf("자리 대기의 마감·원인·빈 결과: %+v %v", result, err)
				}
				if database.pool.Stat().AcquiredConns() != snapshotConnections {
					t.Fatal("대기 요청이 기존 점유 연결을 변경했다")
				}
				for _, release := range releases {
					release()
				}
				releases = nil
				if snapshot != nil {
					waitHopResources(t, database, snapshot, 1)
					if _, err := snapshot.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestStandaloneHopReadModeIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	ctx, finish, err := database.enterHopReadScope(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	var isolation, readOnly string
	if err := database.reader(ctx).QueryRow(ctx, "SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')").Scan(&isolation, &readOnly); err != nil {
		t.Fatal(err)
	}
	if isolation != "repeatable read" || readOnly != "on" || database.pool.Stat().AcquiredConns() != 1 {
		t.Fatalf("독립 홉 읽기: %s %s acquired=%d", isolation, readOnly, database.pool.Stat().AcquiredConns())
	}
	if !database.reservations.TryAcquire(snapshotConnections) {
		t.Fatal("독립 홉이 별도 연결 예약을 소비했다")
	}
	database.reservations.Release(snapshotConnections)
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}
