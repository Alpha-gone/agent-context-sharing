// Package model은 저장 방식과 독립적인 컨텍스트 그래프 도메인 타입과 규칙을 제공한다.
package model

import (
	"time"
	"uuid"
)

// ID는 시간 정렬 가능한 UUIDv7 식별자다.
type ID [16]byte

// NewID는 표준 라이브러리로 UUIDv7을 만든다. 반환 형태는 기존 호출 계약을 유지한다.
func NewID() (ID, error) { return ID(uuid.NewV7()), nil }

// ParseID는 표준 배치 또는 하이픈 없는 UUIDv7을 해석한다.
func ParseID(raw string) (ID, error) {
	if len(raw) != 32 && len(raw) != 36 {
		return ID{}, fieldErrorf("id", "UUID는 표준 배치 또는 32자 hex여야 한다")
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return ID{}, FieldError{Field: "id", Message: "UUID 형식이 올바르지 않다", Cause: err}
	}
	id := ID(parsed)
	if !id.IsV7() {
		return ID{}, fieldErrorf("id", "UUIDv7 식별자가 아니다")
	}
	return id, nil
}

// IsZero는 식별자가 아직 부여되지 않았는지 확인한다.
func (id ID) IsZero() bool { return id == ID{} }

// IsV7은 UUID version과 RFC variant를 확인한다.
func (id ID) IsV7() bool { return id[6]>>4 == 7 && id[8]&0xc0 == 0x80 }

// Time은 UUIDv7의 48비트 밀리초 시각을 UTC로 돌려준다.
func (id ID) Time() (time.Time, error) {
	if !id.IsV7() {
		return time.Time{}, fieldErrorf("id", "UUIDv7 식별자가 아니다")
	}
	var milliseconds int64
	for index := range 6 {
		milliseconds = milliseconds<<8 | int64(id[index])
	}
	return time.UnixMilli(milliseconds).UTC(), nil
}

// String은 UUID의 표준 소문자 표현을 돌려준다.
func (id ID) String() string { return uuid.UUID(id).String() }

// Compare는 식별자의 시간 정렬에 사용하는 바이트 순서를 비교한다.
func (id ID) Compare(other ID) int { return uuid.UUID(id).Compare(uuid.UUID(other)) }

func validateID(field string, id ID) error {
	if !id.IsV7() {
		return fieldErrorf(field, "%s 식별자는 UUIDv7이어야 한다", field)
	}
	return nil
}

func validateOptionalID(field string, id *ID) error {
	if id == nil {
		return nil
	}
	return validateID(field, *id)
}

// sameIDSet은 저장 순서에 의존하지 않고 참조 집합을 비교한다.
func sameIDSet(left, right []ID) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[ID]int, len(left))
	for _, id := range left {
		counts[id]++
	}
	for _, id := range right {
		if counts[id] == 0 {
			return false
		}
		counts[id]--
	}
	return true
}

func sameOptionalID(left, right *ID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
