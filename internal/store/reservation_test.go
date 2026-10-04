package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestConnectionBudget는 예약 총량이 스냅숏 한 요청의 점유를 남기되 그보다 작아지지 않고,
// 풀 상한을 넘지 않는지 확인한다.
func TestConnectionBudget(t *testing.T) {
	for _, test := range []struct {
		maxConns int32
		want     int64
	}{
		{maxConns: 4, want: 4},
		{maxConns: 7, want: 4},
		{maxConns: 8, want: 4},
		{maxConns: 16, want: 12},
	} {
		if got := connectionBudget(test.maxConns); got != test.want || got > int64(test.maxConns) {
			t.Fatalf("connectionBudget(%d) = %d, want %d", test.maxConns, got, test.want)
		}
	}
}

// TestContextWriteReservationSharesOuterBudget은 바깥 멱등성 트랜잭션이 예약을
// 모두 쥔 상태에서도 저장점이 중복 예약을 기다리지 않는지 확인한다.
func TestContextWriteReservationSharesOuterBudget(t *testing.T) {
	reservations, err := newReservations(4)
	if err != nil {
		t.Fatal(err)
	}
	database := &Store{reservations: reservations}
	releaseOuter, err := database.reserveConnections(t.Context(), connectionBudget(4))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOuter()
	// 이 시험은 예약만 확인하므로 실제 트랜잭션 메서드는 호출하지 않는다.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, writeTransactionContextKey{}, pgx.Tx(&afterEvidenceReadTx{}))
	release, err := database.reserveContextWriteConnections(ctx)
	if err != nil {
		t.Fatalf("바깥 예약 공유: %v", err)
	}
	release()
	if reservations.TryAcquire(1) {
		t.Fatal("저장점이 바깥 예약을 반납했다")
	}
	releaseOuter()
	if !reservations.TryAcquire(connectionBudget(4)) {
		t.Fatal("바깥 예약이 남았다")
	}
	reservations.Release(connectionBudget(4))
}

// TestNewReservationsRejectsSmallPool은 스냅숏 한 요청도 담지 못하는 풀 상한을 거부하는지 확인한다.
func TestNewReservationsRejectsSmallPool(t *testing.T) {
	if _, err := newReservations(snapshotConnections - 1); err == nil {
		t.Fatal("풀 상한이 스냅숏 점유보다 작은데 연결 예약을 만들었다")
	}
	if _, err := newReservations(snapshotConnections); err != nil {
		t.Fatalf("풀 상한 %d의 연결 예약: %v", snapshotConnections, err)
	}
}
