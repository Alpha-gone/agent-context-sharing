package model

import "context"

type correlationIDKey struct{}

// WithCorrelationID는 서버가 발급한 요청 상관 ID를 처리 경계 사이에 전달한다.
// 호출자는 사용자 입력의 request_id·멱등성 키를 이 값으로 사용하지 않는다.
func WithCorrelationID(ctx context.Context, id ID) (context.Context, error) {
	if err := validateID("correlation_id", id); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, correlationIDKey{}, id), nil
}

// CorrelationID는 요청의 서버 상관 ID를 반환한다. 요청 없는 주기·도구 실행은 영값이다.
func CorrelationID(ctx context.Context) ID {
	id, _ := ctx.Value(correlationIDKey{}).(ID)
	return id
}
