package plan

import (
	"fmt"
	"testing"
	"time"
)

func TestDefaultRejectedRetentionUsesCalendarMonths(t *testing.T) {
	for _, sample := range []struct{ at, cutoff string }{
		{"2024-08-31T12:34:56.123456789Z", "2024-02-29T12:34:56.123456789Z"},
		{"2023-08-31T12:34:56Z", "2023-02-28T12:34:56Z"},
		{"2024-10-31T12:34:56Z", "2024-04-30T12:34:56Z"},
		{"2024-03-31T12:34:56Z", "2023-09-30T12:34:56Z"},
		{"2024-01-31T12:34:56Z", "2023-07-31T12:34:56Z"},
	} {
		t.Run(sample.at, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339Nano, sample.at)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339Nano, sample.cutoff)
			if err != nil {
				t.Fatal(err)
			}
			days := Default().RejectedOperationRetention.DaysAt(at)
			if got := at.AddDate(0, 0, -days); !got.Equal(want) {
				t.Fatalf("기본 6개월 기준점: %s, want %s (%d일)", got, want, days)
			}
		})
	}
}

func TestRejectedRetentionExplicitDaysAndZero(t *testing.T) {
	at := time.Date(2024, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, days := range []int{0, 1, 183, 365} {
		plans, id := contractPlans(t, `{"rejected_operation_days":`+fmt.Sprint(days)+`}`)
		if got := plans.For(id).RejectedOperationRetention.DaysAt(at); got != days {
			t.Fatalf("명시한 일수: %d, want %d", got, days)
		}
	}
	plans, id := contractPlans(t, `{"max_hops":1}`)
	if got := plans.For(id).RejectedOperationRetention.DaysAt(at); got != 184 {
		t.Fatalf("생략한 기본 6개월: %d, want 184", got)
	}
	local := time.Date(2024, 8, 31, 0, 0, 0, 0, time.FixedZone("test", 9*60*60))
	if Default().RejectedOperationRetention.DaysAt(local) != Default().RejectedOperationRetention.DaysAt(local.UTC()) {
		t.Fatal("기간 판정이 UTC 순간에 따라 정규화되지 않았다")
	}
}
