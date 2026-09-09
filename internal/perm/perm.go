// Package perm은 요청 시점의 그래프 유효 등급을 계산하고 연산 권한을 검사한다.
package perm

import (
	"context"
	"fmt"

	"agent_context_sharing/internal/model"
)

// GradeStore는 유효 등급을 한 질의로 계산하는 접근 계층 계약이다.
type GradeStore interface {
	EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error)
}

// DeniedError는 등급이 부족할 때 필요한 최소 등급을 담는다.
type DeniedError struct{ Required model.GraphGrade }

// Error는 권한 부족을 사람이 읽을 수 있는 형태로 만든다.
func (error DeniedError) Error() string { return "그래프 권한이 부족하다" }

// NotFoundError는 그래프가 없거나 요청 계정에 유효 등급이 없음을 나타낸다.
type NotFoundError struct{}

// Error는 존재를 드러내지 않는 그래프 부재 오류를 만든다.
func (NotFoundError) Error() string { return "그래프를 찾을 수 없다" }

// Require는 대상 그래프가 없거나 접근 불가이면 존재를 드러내지 않고 not found로 처리하고,
// 유효 등급이 모자랄 때만 필요한 등급을 반환한다.
func Require(ctx context.Context, source GradeStore, graphID, accountID model.ID, required model.GraphGrade) error {
	if source == nil || !required.Valid() {
		return fmt.Errorf("권한 검사 인자가 올바르지 않다")
	}
	actual, found, err := source.EffectiveGrade(ctx, graphID, accountID)
	if err != nil {
		return err
	}
	if !found {
		return NotFoundError{}
	}
	if rank(actual) < rank(required) {
		return DeniedError{Required: required}
	}
	return nil
}

func rank(grade model.GraphGrade) int {
	switch grade {
	case model.GraphGradeOwner:
		return 3
	case model.GraphGradeEditor:
		return 2
	case model.GraphGradeViewer:
		return 1
	default:
		return 0
	}
}
