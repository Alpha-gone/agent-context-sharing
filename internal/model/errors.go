package model

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound     = errors.New("대상을 찾지 못했다")
	ErrInvalidState = errors.New("현재 상태에서 허용되지 않는 연산이다")
)

// VersionConflictError는 저장소가 판정한 현재 판 번호를 전달한다.
type VersionConflictError struct{ Current int64 }

func (conflict VersionConflictError) Error() string {
	return fmt.Sprintf("판 번호가 일치하지 않는다: 현재 판 %d", conflict.Current)
}
