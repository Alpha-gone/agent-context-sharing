package model

import "fmt"

// FieldError는 검증이 거절한 속성의 이름을 오류에 함께 담는다.
//
// `SDD.md`의 「오류 부가 정보」가 `invalid_argument`의 "위반한 필드"를 검증한 계층에서
// 담기로 확정했다. 이름은 호출자가 고칠 대상을 가리켜야 하므로 MCP 연산의 인자 이름을
// 쓰며, 인자로 받지 않는 속성만 「컨텍스트 모델」의 속성명을 쓴다.
type FieldError struct {
	Field   string
	Message string
}

// Error는 필드 이름 없이 기존 검증 메시지를 그대로 돌려준다.
func (e FieldError) Error() string { return e.Message }

// fieldErrorf는 검증 메시지를 바꾸지 않고 위반한 필드 이름만 덧붙인다.
func fieldErrorf(field, format string, args ...any) error {
	return FieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}
