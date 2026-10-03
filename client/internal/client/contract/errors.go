package contract

import "errors"

// ErrProtocol은 입력값이나 원격 본문을 포함하지 않는 client_protocol 계약 오류다.
var ErrProtocol = errors.New("원격 MCP 응답 또는 도구 입력 계약이 일치하지 않습니다.")

// ErrConfiguration은 client_configuration으로 분류할 도구 정책 구성 오류다.
var ErrConfiguration = errors.New("all, read_only 또는 유효한 allowlist 도구 공개 정책이 필요합니다.")

// ErrIdentityChanged는 발견·목록 조회 중 인증 주체가 바뀐 client_authorization 오류다.
// 조회 응답을 폐기하며 사용할 계정을 확인하고 다시 인가한 뒤 재시도할 수 있다.
var ErrIdentityChanged = errors.New("사용할 계정을 확인하고 다시 인가한 뒤 재시도하십시오.")
