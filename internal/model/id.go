// Package model은 저장 방식과 독립적인 컨텍스트 그래프 도메인 타입과 규칙을 제공한다.
package model

import (
	"crypto/rand"
	"fmt"
	"io"
	"time"
)

// ID는 시간 정렬 가능한 UUIDv7 식별자다.
type ID [16]byte

// NewID는 현재 UTC 시각과 암호학적으로 안전한 난수로 UUIDv7 식별자를 만든다.
func NewID() (ID, error) {
	return newIDAt(time.Now().UTC(), rand.Reader)
}

// newIDAt은 결정적인 테스트를 위해 시각과 난수 원천을 받아 UUIDv7 식별자를 만든다.
func newIDAt(at time.Time, reader io.Reader) (ID, error) {
	if reader == nil {
		return ID{}, fmt.Errorf("난수 원천이 없다")
	}
	unixMilliseconds := at.UTC().UnixMilli()
	if unixMilliseconds < 0 || unixMilliseconds >= 1<<48 {
		return ID{}, fmt.Errorf("UUIDv7 범위를 벗어난 시각 %s", at.UTC().Format(time.RFC3339Nano))
	}

	var id ID
	if _, err := io.ReadFull(reader, id[:]); err != nil {
		return ID{}, fmt.Errorf("UUIDv7 난수 생성: %w", err)
	}
	for index := range 6 {
		id[index] = byte(unixMilliseconds >> (8 * (5 - index)))
	}
	id[6] = id[6]&0x0f | 0x70
	id[8] = id[8]&0x3f | 0x80
	return id, nil
}

// ParseID는 표준 UUID 문자열을 UUIDv7 식별자로 해석한다.
func ParseID(raw string) (ID, error) {
	// 하이픈을 위치와 무관하게 지우면 `0-1-2...`처럼 비정규 표기도 통과해, 같은 식별자가
	// 여러 문자열로 들어오고 기록과 응답의 표기가 어긋난다. 표준 8-4-4-4-12 배치이거나
	// 하이픈이 아예 없는 표기만 받는다.
	compact := raw
	if len(raw) == 36 {
		if raw[8] != '-' || raw[13] != '-' || raw[18] != '-' || raw[23] != '-' {
			return ID{}, fmt.Errorf("UUID 하이픈 위치가 표준 배치가 아니다")
		}
		compact = raw[:8] + raw[9:13] + raw[14:18] + raw[19:23] + raw[24:]
	}
	if len(compact) != 32 {
		return ID{}, fmt.Errorf("UUID 길이가 32자 hex가 아니다")
	}
	var id ID
	for index := range id {
		high, ok := hexValue(compact[index*2])
		if !ok {
			return ID{}, fmt.Errorf("UUID에 16진수가 아닌 문자가 있다")
		}
		low, ok := hexValue(compact[index*2+1])
		if !ok {
			return ID{}, fmt.Errorf("UUID에 16진수가 아닌 문자가 있다")
		}
		id[index] = high<<4 | low
	}
	if !id.IsV7() {
		return ID{}, fmt.Errorf("UUIDv7 식별자가 아니다")
	}
	return id, nil
}

// IsZero는 식별자가 아직 부여되지 않았는지 확인한다.
func (id ID) IsZero() bool {
	return id == ID{}
}

// IsV7은 UUID variant와 version 7 비트를 확인한다.
func (id ID) IsV7() bool {
	return !id.IsZero() && id[6]>>4 == 7 && id[8]&0xc0 == 0x80
}

// Time은 UUIDv7에 들어 있는 밀리초 UTC 시각을 돌려준다.
func (id ID) Time() (time.Time, error) {
	if !id.IsV7() {
		return time.Time{}, fmt.Errorf("UUIDv7 식별자가 아니다")
	}
	var milliseconds int64
	for index := range 6 {
		milliseconds = milliseconds<<8 | int64(id[index])
	}
	return time.UnixMilli(milliseconds).UTC(), nil
}

// String은 UUID의 표준 8-4-4-4-12 소문자 표현을 돌려준다.
func (id ID) String() string {
	const digits = "0123456789abcdef"
	var text [36]byte
	position := 0
	for index, value := range id {
		if index == 4 || index == 6 || index == 8 || index == 10 {
			text[position] = '-'
			position++
		}
		text[position] = digits[value>>4]
		text[position+1] = digits[value&0x0f]
		position += 2
	}
	return string(text[:])
}

// hexValue는 ASCII 16진수 문자를 수로 바꾼다.
func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}
