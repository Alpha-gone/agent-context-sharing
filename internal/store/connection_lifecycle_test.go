package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func assertConnectionRecovery(t *testing.T, database *Store, scope *readScope, acquired int32) {
	t.Helper()
	// pgxpool의 세션 폐기는 비동기이므로 통계를 유한 시간 안에 직접 대조한다.
	deadline := time.Now().Add(5 * time.Second)
	for database.pool.Stat().AcquiredConns() != acquired && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := database.pool.Stat().AcquiredConns(); got != acquired {
		t.Errorf("획득 연결 = %d, want %d", got, acquired)
	}
	if scope != nil {
		scope.mu.Lock()
		active := scope.active
		scope.mu.Unlock()
		if active != int(acquired) || len(scope.reads) != 0 {
			t.Errorf("활성 읽기 = %d, 자리 = %d", active, len(scope.reads))
		}
	}
	budget := connectionBudget(database.pool.Stat().MaxConns())
	if !database.reservations.TryAcquire(budget) {
		t.Error("연결 예약이 반환되지 않았다")
	} else {
		database.reservations.Release(budget)
	}
}

func TestConnectionConfigurationBeforeDial(t *testing.T) {
	for _, maximum := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(maximum), func(t *testing.T) {
			_, err := New(t.Context(), fmt.Sprintf("postgres://unused@127.0.0.1:1/unused?pool_max_conns=%d", maximum), "agent_context", nil, nil, "")
			if err == nil || !strings.Contains(err.Error(), "pool_max_conns") {
				t.Fatalf("최소 풀 검사 = %v", err)
			}
		})
	}
	if _, err := New(t.Context(), "invalid", strings.Repeat("a", 64), nil, nil, ""); err == nil || !strings.Contains(err.Error(), "AGE") {
		t.Fatalf("그래프 이름 길이 검사 = %v", err)
	}
}

func TestConnectionBudgetRangeAndCancelCause(t *testing.T) {
	for maximum := int32(4); maximum <= 64; maximum++ {
		if got := connectionBudget(maximum); got != max(4, int64(maximum)-4) || got > int64(maximum) {
			t.Fatalf("예약 총량(%d) = %d", maximum, got)
		}
	}
	reservations, _ := newReservations(4)
	database := &Store{reservations: reservations}
	release, err := database.reserveConnections(t.Context(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cause := errors.New("시험 요청 취소")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	if _, err := database.reserveConnections(ctx, 2); !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("예약 취소 원인 = %v", err)
	}
	release()
	release()
	if !reservations.TryAcquire(4) {
		t.Fatal("취소 뒤 예약이 남았다")
	}
	reservations.Release(4)
}

func TestConnectionPreparationEverySessionIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	connections := make([]*pgxpool.Conn, 0, 4)
	defer func() {
		for _, conn := range connections {
			conn.Release()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 4 {
		conn, err := database.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		var zone, path, value string
		if err := conn.QueryRow(ctx, `SHOW TIME ZONE`).Scan(&zone); err != nil || zone != "UTC" {
			t.Fatalf("UTC 준비 = %s, %v", zone, err)
		}
		if err := conn.QueryRow(ctx, `SHOW search_path`).Scan(&path); err != nil || path != `ag_catalog, "$user", public` {
			t.Fatalf("검색 경로 = %s, %v", path, err)
		}
		if err := conn.QueryRow(ctx, "SELECT v::text FROM cypher('"+database.graphName+"', $$ RETURN 7 $$) AS (v agtype)").Scan(&value); err != nil || value != "7" {
			t.Fatalf("AGE 준비 = %s, %v", value, err)
		}
	}
	if got := database.pool.Stat().AcquiredConns(); got != 4 {
		t.Fatalf("실제 연결 = %d", got)
	}
	for _, conn := range connections {
		conn.Release()
	}
	connections = nil
	assertConnectionRecovery(t, database, nil, 0)
}

func TestConnectionPreparationFailureExcludedIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	// 시험용 DB의 권한 없는 로그인만 사용하여 LOAD 실패를 실제 서버에서 재현한다.
	role := "arch04_" + strings.ReplaceAll(newTestID(t).String(), "-", "")
	if _, err := database.pool.Exec(t.Context(), "CREATE ROLE "+role+" LOGIN PASSWORD 'arch04_synthetic'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := database.pool.Exec(context.Background(), "DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "arch04_synthetic")
	unprepared, err := New(t.Context(), u.String(), database.graphName, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer unprepared.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := unprepared.Ping(ctx); err == nil || !strings.Contains(err.Error(), "연결 준비") {
		t.Fatalf("준비 실패 연결 = %v", err)
	}
	assertConnectionRecovery(t, unprepared, nil, 0)
}

func TestConnectionSnapshotFailuresRecoverIntegration(t *testing.T) {
	for _, scenario := range []string{"import", "query", "cancel", "nested"} {
		t.Run(scenario, func(t *testing.T) {
			database := newSmallPoolStore(t)
			snapshot, err := database.BeginReadSnapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close(t.Context())
			ctx, cancel := context.WithCancelCause(snapshot.Context(t.Context()))
			defer cancel(nil)
			if scenario == "import" {
				snapshot.scope.snapshot = "00000000-00000000-1"
				if _, _, err := database.enterReadScope(ctx); err == nil {
					t.Fatal("무효 스냅숏을 가져왔다")
				}
			} else {
				read, release, err := database.enterReadScope(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "query":
					if _, err := read.Value(readTxKey{}).(pgx.Tx).Exec(read, `SELECT 1/0`); err == nil {
						t.Fatal("DB 오류가 없다")
					}
				case "cancel":
					cause := errors.New("읽기 취소 이유")
					cancel(cause)
					if _, err := read.Value(readTxKey{}).(pgx.Tx).Exec(read, `SELECT 1`); err == nil {
						t.Fatal("취소된 읽기를 실행했다")
					}
				case "nested":
					nested, end, err := database.enterReadScope(read)
					if err != nil || database.reader(nested) != database.reader(read) {
						t.Fatalf("중첩 읽기 = %v", err)
					}
					if err := end(); err != nil {
						t.Fatal(err)
					}
					if len(snapshot.scope.reads) != 1 || database.pool.Stat().AcquiredConns() != 2 {
						t.Fatal("중첩 읽기가 바깥 자원을 변경했다")
					}
				}
				if err := release(); err != nil {
					t.Fatal(err)
				}
				if err := release(); err != nil {
					t.Fatal(err)
				}
			}
			if got := database.pool.Stat().AcquiredConns(); got != 1 || len(snapshot.scope.reads) != 0 {
				t.Fatalf("읽기 종료 후 연결 = %d, 자리 = %d", got, len(snapshot.scope.reads))
			}
			stats, err := snapshot.Close(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if again, err := snapshot.Close(ctx); err != nil || again != stats {
				t.Fatalf("반복 종료 = %+v, %v", again, err)
			}
			assertConnectionRecovery(t, database, snapshot.scope, 0)
		})
	}
}

func TestConnectionReservationWaitDoesNotAcquireIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	release, err := database.reserveConnections(t.Context(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cause := errors.New("예약 대기 만료")
	ctx, cancel := context.WithTimeoutCause(t.Context(), 50*time.Millisecond, cause)
	defer cancel()
	if _, err := database.BeginReadSnapshot(ctx); !errors.Is(err, cause) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("예약 대기 취소 = %v", err)
	}
	if got := database.pool.Stat().AcquiredConns(); got != 0 {
		t.Fatalf("예약 대기가 연결을 획득했다: %d", got)
	}
	release()
	assertConnectionRecovery(t, database, nil, 0)
}

// rollbackCancelTracer는 종료 SQL의 실패를 실제 드라이버 경계에 주입한다.
type rollbackCancelTracer struct{}

func (rollbackCancelTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(data.SQL, "rollback") {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return canceled
	}
	return ctx
}
func (rollbackCancelTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestConnectionSnapshotCloseErrorPreservedIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	config := database.pool.Config()
	database.pool.Close()
	config.ConnConfig.Tracer = rollbackCancelTracer{}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	database.pool = pool
	cause := errors.New("읽기 업무 실패")
	_, err = database.WithReadSnapshot(t.Context(), func(context.Context) error { return cause })
	if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("읽기·종료 오류 = %v", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestConnectionMixedSnapshotPeriodicRecoverIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var group sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 8 {
		group.Go(func() {
			_, err := database.WithReadSnapshot(ctx, func(scoped context.Context) error {
				read, release, err := database.enterReadScope(scoped)
				if err != nil {
					return err
				}
				_, err = read.Value(readTxKey{}).(pgx.Tx).Exec(read, `SELECT pg_sleep(0.005)`)
				return errors.Join(err, release())
			})
			errs <- err
		})
		group.Go(func() {
			_, acquired, err := database.WithTryAdvisoryLock(ctx, int64(4000+i), func(ctx context.Context) (int, error) {
				_, err := database.pool.Exec(ctx, `SELECT pg_sleep(0.005)`)
				return 1, err
			})
			if !acquired && err == nil {
				err = errors.New("고유 시험 잠금을 확보하지 못했다")
			}
			errs <- err
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestConnectionAcquireCancelReturnsBudgetIntegration(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprint(scoped), func(t *testing.T) {
			database := newSmallPoolStore(t)
			var snapshot *ReadSnapshot
			ctx := t.Context()
			maximum := 4
			if scoped {
				var err error
				snapshot, err = database.BeginReadSnapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer snapshot.Close(t.Context())
				ctx = snapshot.Context(ctx)
				maximum--
			}
			connections := make([]*pgxpool.Conn, 0, maximum)
			defer func() {
				for _, conn := range connections {
					conn.Release()
				}
			}()
			for range maximum {
				conn, err := database.pool.Acquire(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				connections = append(connections, conn)
			}
			cause := errors.New("연결 획득 대기 마감")
			waiting, cancel := context.WithTimeoutCause(ctx, 50*time.Millisecond, cause)
			defer cancel()
			var err error
			if scoped {
				_, _, err = database.enterReadScope(waiting)
				if len(snapshot.scope.reads) != 0 {
					t.Fatal("획득 실패 뒤 읽기 자리가 남았다")
				}
			} else {
				_, err = database.BeginReadSnapshot(waiting)
			}
			if !errors.Is(err, cause) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("획득 취소 원인 = %v", err)
			}
			for _, conn := range connections {
				conn.Release()
			}
			connections = nil
			if snapshot != nil {
				if _, err := snapshot.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestConnectionIdempotencyCancelRecoversIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	cause := errors.New("쓰기 요청 취소")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	request := IdempotencyRequest{AccountID: newTestID(t), Key: newTestID(t), ToolName: "graph_update"}
	_, err := database.ReplayIdempotent(ctx, request, func(context.Context) ([]byte, error) {
		cancel(cause)
		return nil, context.Canceled
	})
	if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("쓰기 취소 원인 = %v", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
	var count int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.idempotency_record WHERE actor_account_id=$1`, request.AccountID.String()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("취소된 예약 행 = %d, %v", count, err)
	}
}

func TestConnectionPeriodicLostLockDiscardsSessionIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	const key = int64(940004)
	_, acquired, err := database.WithTryAdvisoryLock(t.Context(), key, func(context.Context) (int, error) {
		// 잠금 연결을 직접 노출하지 않고, DB가 소유권을 잃은 경우를 만든다.
		var pid int32
		if err := database.pool.QueryRow(t.Context(), `SELECT pid FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND granted`, key).Scan(&pid); err != nil {
			return 0, err
		}
		var terminated bool
		if err := database.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil {
			return 0, err
		}
		if !terminated {
			return 0, errors.New("시험 잠금 세션을 종료하지 못했다")
		}
		return 0, nil
	})
	if !acquired || err == nil {
		t.Fatalf("잠금 상실 오류 = %t, %v", acquired, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
	_, acquired, err = database.WithTryAdvisoryLock(t.Context(), key, func(context.Context) (int, error) { return 1, nil })
	if !acquired || err != nil {
		t.Fatalf("실패 뒤 잠금 재획득 = %t, %v", acquired, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}
