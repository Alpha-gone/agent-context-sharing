package plan

import "time"

// RetentionPeriod는 한 플랜 항목의 기본 달력 기간과 명시한 일수를 구분한다.
// 영값은 무기한이며 외부에서 별도 월 단위 정책을 만들 수 없다.
type RetentionPeriod struct {
	days   int
	months int
}

// DaysAt은 UTC 회차의 기준 시각에 맞는 보존 일수를 반환한다.
// 월말을 그 달의 마지막 날로 제한하여 Date의 다음 달 정규화가
// 아직 보존해야 할 기록을 정리 범위에 넣지 않게 한다.
func (period RetentionPeriod) DaysAt(at time.Time) int {
	if period.months == 0 {
		return period.days
	}
	at = at.UTC()
	year, month, day := at.Date()
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -period.months, 0)
	lastDay := first.AddDate(0, 1, -1).Day()
	hour, minute, second := at.Clock()
	cutoff := time.Date(first.Year(), first.Month(), min(day, lastDay), hour, minute, second, at.Nanosecond(), time.UTC)
	return int(at.Sub(cutoff) / (24 * time.Hour))
}
