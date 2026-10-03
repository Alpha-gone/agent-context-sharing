package contract

import "errors"

// ErrProtocol은 입력값이나 원격 본문을 포함하지 않는 client_protocol 계약 오류다.
var ErrProtocol = errors.New("원격 MCP 응답 또는 도구 입력 계약이 일치하지 않습니다.")

// ErrConfiguration은 client_configuration으로 분류할 기동 구성 오류다.
var ErrConfiguration = errors.New("필수 기동 구성과 도구 공개 정책을 확인한 뒤 프로세스를 다시 시작하십시오.")

// ErrIdentityChanged는 인증·발견·목록에서 주체 결합을 지키지 못한 client_authorization 오류다.
// 다른 주체의 토큰·조회 응답은 폐기하고 현재 계정으로 다시 인가해야 한다.
var ErrIdentityChanged = errors.New("사용할 계정을 확인하고 다시 인가한 뒤 재시도하십시오.")

// ErrTransport는 본문이나 주소를 노출하지 않는 client_transport 통신 오류다.
var ErrTransport = errors.New("원격 연결 상태를 확인한 뒤 다시 시도하십시오.")

// ErrIndeterminate는 전달됐을 수 있는 쓰기의 client_indeterminate 오류다.
var ErrIndeterminate = errors.New("쓰기 처리 결과를 확인할 수 없습니다. 대상 상태를 조회한 뒤 다음 조치를 결정하십시오.")

// ErrBusy는 입력 크기나 대기열 상한을 넘은 client_busy 오류다.
var ErrBusy = errors.New("진행 중인 요청이 많거나 입력이 너무 큽니다. 이후 다시 시도하십시오.")
