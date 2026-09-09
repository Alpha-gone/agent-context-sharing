// Package plan은 계정 플랜의 고정 항목과 한도 판정을 제공한다.
package plan

import "fmt"

// Limits는 계정 플랜이 정할 수 있는 14개 항목의 값이다. 0은 해당 한도가 없음을 뜻한다.
type Limits struct {
	MaxHops                  int
	MaxHopNodes              int
	GraphPage                PageSize
	ContextBudget            int
	GraphsPerAccount         int
	StoredCharactersPerGraph int64
	RetentionDays            int
	GraceDays                int
	AuditRetentionDays       int
	TierMoveAfterDays        int
	RejectedOperationDays    int
	WritesPerMinute          int
	ProposalRetentionDays    int
	RelationPage             PageSize
}

// PageSize는 목록 페이지 크기 한 항목의 기본값과 상한을 함께 표현한다.
type PageSize struct{ Default, Maximum int }

// Default는 플랜 정책을 따로 부여하지 않은 계정에 적용하는 기본값이다.
func Default() Limits {
	return Limits{
		MaxHops: 4, MaxHopNodes: 200, GraphPage: PageSize{Default: 50, Maximum: 200},
		ContextBudget: 20_000, GraceDays: 30, RejectedOperationDays: 183,
		ProposalRetentionDays: 90, RelationPage: PageSize{Default: 50, Maximum: 200},
	}
}

// LimitError는 한도 초과 항목과 현재·허용 값을 호출자에게 전달한다.
type LimitError struct {
	Name    string
	Current int64
	Allowed int64
}

// Error는 한도 초과를 사람이 읽을 수 있는 형태로 만든다.
func (error LimitError) Error() string {
	return fmt.Sprintf("%s 한도를 초과했다: 현재 %d, 허용 %d", error.Name, error.Current, error.Allowed)
}

// CheckRequest는 하나의 요청 값이 설정된 한도를 넘는지 확인한다.
func CheckRequest(name string, value, allowed int64) error {
	if allowed > 0 && value > allowed {
		return LimitError{Name: name, Current: value, Allowed: allowed}
	}
	return nil
}

// CheckIncrease는 값을 늘리는 요청만 누적 한도로 막는다. 이미 초과한 상태의 읽기와
// 축소는 허용해 플랜 하향이 기존 데이터를 삭제하지 않게 한다.
func CheckIncrease(name string, current, delta, allowed int64) error {
	if allowed > 0 && delta > 0 && delta > allowed-current {
		return LimitError{Name: name, Current: current, Allowed: allowed}
	}
	return nil
}
