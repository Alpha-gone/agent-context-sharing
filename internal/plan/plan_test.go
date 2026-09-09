package plan

import "testing"

func TestChecks(t *testing.T) {
	if err := CheckRequest("hop", 5, 4); err == nil {
		t.Fatal("요청 한도 초과가 허용됐다")
	}
	if err := CheckIncrease("storage", 11, 1, 10); err == nil {
		t.Fatal("초과 상태 확장이 허용됐다")
	}
	if err := CheckIncrease("storage", 11, -1, 10); err != nil {
		t.Fatalf("초과 상태 축소가 거부됐다: %v", err)
	}
	if err := CheckIncrease("storage", 9, 2, 10); err == nil {
		t.Fatal("한 번의 확장으로 한도를 넘는 요청이 허용됐다")
	}
	if err := CheckIncrease("storage", 9, 1, 10); err != nil {
		t.Fatalf("한도까지의 확장이 거부됐다: %v", err)
	}
}
