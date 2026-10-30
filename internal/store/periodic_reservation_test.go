package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPeriodicGuardSaturatedReservationDeadline(t *testing.T) {
	reservations, err := newReservations(4)
	if err != nil {
		t.Fatal(err)
	}
	// 예약이 가득 차면 실제 풀 연결 단계까지 진행하지 않아야 한다.
	database := &Store{reservations: reservations}
	release, err := database.reserveConnections(t.Context(), connectionBudget(4))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	called := false
	count, acquired, err := database.WithTryAdvisoryLock(ctx, 155, func(context.Context) (int, error) {
		called = true
		return 1, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || count != 0 || acquired || called {
		t.Fatal("예약 포화 마감·실행권 경계", count, acquired, called, err)
	}
	release()
	if !reservations.TryAcquire(connectionBudget(4)) {
		t.Fatal("마감 뒤 예약 누수")
	}
	reservations.Release(connectionBudget(4))
}
