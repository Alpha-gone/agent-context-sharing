# 쓰기 멱등성 구현 검토

## 범위

- 검토일: 2026-09-27
- 요구사항: `FR-AGENT_CONTEXT-155`
- 대상: 협상된 `graph_create`, `graph_update`, `node_create`, `node_update`, `node_discard`, `node_restore`, `relation_confirm`, `relation_discard` 호출

## 구현 대조

- `server/discover`는 `io.github.alpha-gone/write-idempotency` 확장과 86,400,000ms 보관 기간을 광고한다.
- 전송 계층은 요청별 클라이언트 확장 선언이 있을 때만 위 여덟 쓰기의 `Idempotency-Key` 헤더 값과, 도구 이름과 입력 검증을 통과한 인자의 결정적 JSON SHA-256 지문을 처리기에 넘긴다. 지문의 정규화는 객체 키 순서와 공백 같은 표현 차이만 없애고 기본값을 채우지 않는다.
- 처리기는 권한 확인 뒤 7단계에서 헤더가 하이픈을 포함한 UUIDv7을 담은 Structured Fields String 하나인지 검증한다. 따라서 권한 없는 요청은 키 형식과 무관하게 `not_found` 또는 `permission_denied`로 거부된다.
- `idempotency_record`는 계정과 키를 기본 키로, `tool_name`·`request_fingerprint`·`tool_result`·`received_at`·`expires_at`을 보관한다. 저장소는 계정·키 자문 잠금 아래 만료 행을 먼저 지우고 예약·업무 변경·적용/거부 기록·결과 저장을 한 트랜잭션에서 끝낸다.
- 도메인 연산은 예약 트랜잭션 안의 저장점에서 실행한다. 누적 한도 초과처럼 업무 변경 뒤에 드러난 거부는 저장점까지만 되돌려 업무 변경과 쓰기 속도 소비를 없앤 뒤 거부 기록과 완료 결과만 확정한다.
- 같은 계정·키에서 도구나 지문이 다르면 `invalid_argument`와 `Idempotency-Key` 필드를 반환한다. 완료 결과는 요청 시점의 인증·권한 확인 뒤에만 재생하며, `graph_create` 성공 결과는 저장된 `graph_id`에 계정이 지금도 등급을 가질 때만 돌려주고 아니면 `not_found`로 답한다.
- 만료 결과 정리는 독립 자문 잠금 키를 써 매시간 실행하는 여덟 번째 주기 작업이다.

## 검증

- `internal/mcp/transport_test.go`는 발견 확장 광고, 협상된 키 헤더와 요청 지문의 전달, 따옴표 누락·하이픈 없는 표기·UUIDv4·형식 오류·중복 헤더 키의 거부를 확인한다.
- `internal/mcp/handler_integration_test.go`는 실제 AGE 개발 데이터베이스에서 순차·동시 재시도가 한 `node_create`와 한 `operation_log`만 남기는지, 같은 키의 다른 요청이 거부되는지, 롤백에 결과 행이 남지 않는지, 만료 행이 정리 전에도 새 요청으로 처리되는지를 확인한다. 트랜잭션 밖 사전 검사를 통과한 뒤 업무 변경 후 한도로 거부된 생성이 정점을 남기지 않고 같은 거부를 재생하는지, 권한 없는 계정의 잘못된 키가 `permission_denied`로 거부되는지, 등급을 잃은 계정의 `graph_create` 재생이 `not_found`인지도 확인한다.
- 기존 처리기 통합·계약 테스트는 확장 문맥 없이 연산 13종을 호출해 기존 클라이언트 경로를 유지한다.
- 열 이름 정정 뒤 로컬 Docker 개발 데이터베이스의 `003_idempotency_record` 적용 이력과 테이블을 되돌리고 `go run ./cmd/migrate up`으로 다시 적용했다. 두 번째 실행은 적용할 마이그레이션이 없고 적용된 판이 3개임을 확인했다. `TEST_DATABASE_REQUIRED=1 go test ./...`, `go build ./...`, `go vet ./...`, `gofmt -l .`도 Go 1.27.1 도구 체인에서 통과했다.

## 남은 범위

에이전트 컨텍스트 MCP 클라이언트 구현이 준비되면 실제 전송 실패 뒤 같은 키를 재사용하는 종단 간 검증을 추가한다. 이는 서버의 완료 결과 저장 순서와 별개의 클라이언트 준비 조건이다.
