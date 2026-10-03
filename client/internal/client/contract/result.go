package contract

import (
	"bytes"
	"encoding/json/jsontext"
)

// Result는 도구 결과의 내용·순서·숫자·커서를 해석하지 않고 보관한다.
// JSON-RPC 외피와 HTTP 상태 검증은 원격 전송 경계가 담당한다.
type Result struct{ raw jsontext.Value }

// NewResult는 완전한 JSON 객체만 독립 복사본으로 보관한다.
func NewResult(raw jsontext.Value) (*Result, error) {
	if !raw.IsValid() || raw.Kind() != '{' {
		return nil, ErrProtocol
	}
	return &Result{raw: bytes.Clone(raw)}, nil
}

// Raw는 필드 누락이나 재정렬 없이 결과의 독립 복사본을 반환한다.
func (r *Result) Raw() jsontext.Value { return bytes.Clone(r.raw) }
