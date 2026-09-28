package store

import "testing"

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

// TestNewReservationsRejectsSmallPool은 스냅숏 한 요청도 담지 못하는 풀 상한을 거부하는지 확인한다.
func TestNewReservationsRejectsSmallPool(t *testing.T) {
	if _, err := newReservations(snapshotConnections - 1); err == nil {
		t.Fatal("풀 상한이 스냅숏 점유보다 작은데 연결 예약을 만들었다")
	}
	if _, err := newReservations(snapshotConnections); err != nil {
		t.Fatalf("풀 상한 %d의 연결 예약: %v", snapshotConnections, err)
	}
}
