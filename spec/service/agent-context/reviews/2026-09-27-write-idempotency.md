# 쓰기 멱등성 구현 검토

## 범위

- 검토일: 2026-09-27
- 요구사항: `FR-AGENT_CONTEXT-155`
- 대상: 협상된 `graph_create`, `graph_update`, `node_create`, `node_update`, `node_discard`, `node_restore`, `relation_confirm`, `relation_discard` 호출

## 구현 대조

- `server/discover`는 `io.github.alpha-gone/write-idempotency` 확장과 86,400,000ms 보관 기간을 광고한다.
- 전송 계층은 요청별 클라이언트 확장 선언이 있을 때만 위 여덟 쓰기의 `Idempotency-Key` Structured Fields String을 받는다. 값은 하이픈을 포함한 UUIDv7이어야 하며, 도구 이름과 정규화·검증된 인자의 결정적 JSON SHA-256 지문을 함께 처리기에 넘긴다.
- `idempotency_record`는 계정과 키를 기본 키로, 도구·지문·완료 결과·접수/만료 시각을 보관한다. 저장소는 계정·키 자문 잠금 아래 만료 행을 먼저 지우고 예약·업무 변경·적용/거부 기록·결과 저장을 한 트랜잭션에서 끝낸다.
- 같은 계정·키에서 도구나 지문이 다르면 `invalid_argument`와 `Idempotency-Key` 필드를 반환한다. 완료 결과는 요청 시점의 인증·권한 확인 뒤에만 재생한다.
- 만료 결과 정리는 독립 자문 잠금 키를 써 매시간 실행하는 여덟 번째 주기 작업이다.

## 검증

- `internal/mcp/transport_test.go`는 발견 확장 광고, 협상된 키 전달, 잘못된 키와 하이픈 없는 UUIDv7의 전송 전 거부를 확인한다.
- `internal/mcp/handler_integration_test.go`는 실제 AGE 개발 데이터베이스에서 순차·동시 재시도가 한 `node_create`와 한 `operation_log`만 남기는지, 같은 키의 다른 요청이 거부되는지, 롤백에 결과 행이 남지 않는지, 만료 행이 정리 전에도 새 요청으로 처리되는지를 확인한다.
- 기존 처리기 통합·계약 테스트는 확장 문맥 없이 연산 13종을 호출해 기존 클라이언트 경로를 유지한다.
- 로컬 Docker 개발 데이터베이스에서 `go run ./cmd/migrate up`을 다시 실행해 적용할 마이그레이션이 없고 적용된 판이 3개임을 확인했다. `TEST_DATABASE_REQUIRED=1 go test ./...`, `go build ./...`, `go vet ./...`, `gofmt -l .`도 Go 1.27.1 컨테이너에서 통과했다.

## 남은 범위

에이전트 컨텍스트 MCP 클라이언트 구현이 준비되면 실제 전송 실패 뒤 같은 키를 재사용하는 종단 간 검증을 추가한다. 이는 서버의 완료 결과 저장 순서와 별개의 클라이언트 준비 조건이다.
