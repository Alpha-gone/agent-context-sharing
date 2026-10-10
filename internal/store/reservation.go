package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/semaphore"
)

// 「데이터베이스 연결」의 연결 예약이다. 연결 하나를 쥔 채 같은 풀에서 연결을 더 기다리는
// 경계는 시작 전에 동시에 쥘 최대 연결 수를 예약한다. 예약 총량이 풀 상한을 넘지 않으므로
// 기다리는 경계가 있으면 예약한 경계들이 실제로 쥔 연결의 합은 풀 상한보다 작고, 그 대기는
// 연결 하나만 쓰고 돌려주는 요청이 끝나면 풀린다.
const (
	// snapshotConnections는 동기화 스냅숏 한 요청의 최대 점유다. 조정 연결 하나와 경계 안
	// 읽기 자리 셋이다. 각 자리는 읽기 연결을 반환한 뒤 접근 기록 연결을 하나씩 쓰고,
	// 기록이 끝날 때까지 유지되므로 실제 점유도 이 값을 넘지 않는다.
	snapshotConnections = 4
	// idempotencyConnections는 멱등성 예약 트랜잭션 하나와, 그 안의 도메인 연산이 한 번에
	// 하나씩 쓰는 풀 읽기 하나다.
	idempotencyConnections = 2
	// contextWriteConnections는 컨텍스트 쓰기 트랜잭션과 충돌 판정용 풀 읽기 하나다.
	contextWriteConnections = 2
)

// reserveContextWriteConnections는 멱등성 저장점의 바깥 예약을 재사용한다. 연결을 쥔
// 뒤 다시 예약하면 다른 예약 요청과 서로 기다릴 수 있으므로 중첩 예약하지 않는다.
func (s *Store) reserveContextWriteConnections(ctx context.Context) (func(), error) {
	if _, ok := ctx.Value(writeTransactionContextKey{}).(pgx.Tx); ok {
		return func() {}, nil
	}
	return s.reserveConnections(ctx, contextWriteConnections)
}

// connectionBudget은 예약 총량이다. 풀 상한에서 스냅숏 한 요청의 점유를 빼 예약하지 않는
// 요청의 몫을 남기되, 스냅숏 한 요청은 들어갈 수 있게 그 점유보다 작게 두지 않는다.
// New가 풀 상한을 snapshotConnections 이상으로 강제하므로 총량은 풀 상한을 넘지 않는다.
func connectionBudget(maxConns int32) int64 {
	return max(snapshotConnections, int64(maxConns)-snapshotConnections)
}

// reserveConnections는 연결 count개를 예약하고 반납 함수를 돌려준다. 예약을 기다리는 동안
// 연결을 쥐지 않으며 ctx가 끝나면 대기를 멈춘다. 반납 함수는 여러 번 불러도 한 번만 반납한다.
func (s *Store) reserveConnections(ctx context.Context, count int64) (func(), error) {
	if err := s.reservations.Acquire(ctx, count); err != nil {
		return nil, fmt.Errorf("연결 예약: %w", err)
	}
	return sync.OnceFunc(func() { s.reservations.Release(count) }), nil
}

// newReservations는 풀 상한에 맞는 연결 예약을 만든다.
func newReservations(maxConns int32) (*semaphore.Weighted, error) {
	if maxConns < snapshotConnections {
		return nil, fmt.Errorf("pool_max_conns는 동기화 스냅숏 한 요청의 점유 %d 이상이어야 한다: %d", snapshotConnections, maxConns)
	}
	return semaphore.NewWeighted(connectionBudget(maxConns)), nil
}
