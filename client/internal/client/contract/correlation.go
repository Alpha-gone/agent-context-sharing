package contract

import (
	"context"
	"uuid"
)

type correlationKey struct{}

// WithCorrelation은 호스트 입력과 무관한 새 호출 상관 식별자를 지정한다.
func WithCorrelation(ctx context.Context) context.Context {
	return context.WithValue(ctx, correlationKey{}, uuid.NewV7().String())
}

// CorrelationID는 내부에서 생성한 호출 상관 식별자만 반환한다.
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}
