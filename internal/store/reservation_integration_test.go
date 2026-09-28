package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// newSmallPoolStore는 풀 상한을 허용 최솟값으로 둔 저장소다. 요청 수가 풀 상한을 넘을 때의
// 연결 대기를 적은 요청으로 재현한다.
func newSmallPoolStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		newIntegrationStore(t) // 건너뛰기와 TEST_DATABASE_REQUIRED 판정을 공유한다.
		return nil
	}
	separator := "?"
	if strings.Contains(databaseURL, "?") {
		separator = "&"
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	database, err := New(t.Context(), databaseURL+separator+"pool_max_conns=4", graphName, nil, nil, "")
	if err != nil {
		t.Fatalf("작은 풀 저장소 준비: %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

// TestConnectionReservationPreventsPoolStarvationIntegration은 풀 상한보다 많은 스냅숏 검색과
// 멱등성 쓰기가 동시에 첫 연결을 쥐고 두 번째 연결을 서로 기다리며 멈추지 않는지 확인한다.
// 예약이 없으면 풀 상한만큼의 요청이 첫 연결을 모두 잡아 제한 시간까지 끝나지 않는다.
func TestConnectionReservationPreventsPoolStarvationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	accountID, graphID := newTestID(t), newTestID(t)
	t.Cleanup(func() {
		_, _ = database.pool.Exec(context.Background(), `DELETE FROM public.idempotency_record WHERE actor_account_id = $1`, accountID.String())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	const requests = 8
	// 첫 연결을 쥔 뒤 잠시 머물러, 예약이 없다면 풀 상한만큼의 요청이 모두 첫 연결을 잡은
	// 상태에서 두 번째 연결을 요청하게 만든다.
	holdSecondConnection := func() { time.Sleep(100 * time.Millisecond) }

	var done sync.WaitGroup
	// 스냅숏 요청은 읽기마다 하나씩 최대 셋, 멱등성 요청은 하나의 오류를 보낸다.
	errs := make(chan error, 4*requests)
	for range requests {
		done.Go(func() {
			snapshot, err := database.BeginReadSnapshot(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer snapshot.Close(context.Background())
			holdSecondConnection()
			readContext := snapshot.Context(ctx)
			var reads sync.WaitGroup
			for range 3 {
				reads.Go(func() {
					if _, err := database.KeywordCandidates(readContext, graphID, "연결 예약", time.Now().UTC(), 5); err != nil {
						errs <- err
					}
				})
			}
			reads.Wait()
		})
		done.Go(func() {
			request := IdempotencyRequest{AccountID: accountID, Key: newTestID(t), ToolName: "graph_update"}
			_, err := database.ReplayIdempotent(ctx, request, func(transactionContext context.Context) ([]byte, error) {
				holdSecondConnection()
				// 처리기의 권한 확인처럼 예약 트랜잭션 밖의 풀 연결을 쓴다.
				if _, _, err := database.EffectiveGrade(transactionContext, graphID, accountID); err != nil {
					return nil, err
				}
				return []byte(`{"content":[]}`), nil
			})
			if err != nil {
				errs <- err
			}
		})
	}
	done.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("동시 요청 실패: %v", err)
	}
}

// TestReadSnapshotLimitsConcurrentReadsIntegration은 한 스냅숏이 경계 안 읽기 트랜잭션을 셋보다
// 많이 열지 않고, 넘는 읽기는 앞선 읽기가 끝날 때까지 기다리는지 확인한다.
func TestReadSnapshotLimitsConcurrentReadsIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	snapshot, err := database.BeginReadSnapshot(t.Context())
	if err != nil {
		t.Fatalf("스냅숏 시작: %v", err)
	}
	ctx := snapshot.Context(t.Context())
	releases := make([]func(), 0, snapshotConnections-1)
	for range snapshotConnections - 1 {
		_, release, err := database.enterReadScope(ctx)
		if err != nil {
			t.Fatalf("경계 안 읽기 시작: %v", err)
		}
		releases = append(releases, release)
	}

	waiting, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, _, err := database.enterReadScope(waiting); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("자리가 찬 스냅숏의 읽기 = %v, want 대기 뒤 %v", err, context.DeadlineExceeded)
	}

	releases[0]()
	_, release, err := database.enterReadScope(ctx)
	if err != nil {
		t.Fatalf("앞선 읽기가 끝난 뒤 읽기 시작: %v", err)
	}
	release()
	for _, release := range releases[1:] {
		release()
	}
	if stats := snapshot.Close(context.Background()); stats.PeakConnections > snapshotConnections {
		t.Fatalf("스냅숏 동시 연결 최대 = %d, want %d 이하", stats.PeakConnections, snapshotConnections)
	}
}
