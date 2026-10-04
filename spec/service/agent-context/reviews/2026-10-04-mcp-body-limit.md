# MCP 요청 본문 상한 검증

- 날짜: 2026-10-04
- 범위: [#46](https://github.com/Alpha-gone/agent-context-sharing/issues/46)의 `/mcp` 인증 전 본문 해석과 배열 필터 검증
- 기준: [SDD](../SDD.md)의 「요청 처리 순서」·「입력 검증」·「테스트 전략」

## 원인과 변경

`json.UnmarshalRead`가 상한 없이 인자 객체를 해석한 뒤에야 인증을 확인했다. 열거형 배열 필터는 공개 스키마의 `maxItems`와 달리 서버 `stringArray`에서 원소 수를 검사하지 않았다. 참조 배열의 100개·1000개 상한은 이미 적용되어 있었다.

SDD를 먼저 보완하고, [전송 처리기](../../../../internal/mcp/transport.go)에 고정 1MiB 상한을 적용했다. `Content-Length` 초과는 읽기 전에 거부하고 길이 미상·작게 선언된 입력도 `http.MaxBytesReader`로 제한한다. 오류에 감싸진 `MaxBytesError`를 구분해 `413`, JSON-RPC `-32600`, `id: null`과 `data.max_body_bytes`로 응답한다. 본문·부분 해석 id·인증 정보는 응답하지 않는다. 선행 Origin·메서드·헤더 검증과 JSON 문법 오류의 `400`·`-32700`은 유지한다.

[배열 검증](../../../../internal/mcp/schema.go)은 열거 값 개수를 원소 수 상한으로 사용해 기존 공개 스키마(3·7·3·4개)와 맞춘다. 중복도 개수에 포함하며, 빈 배열과 상한 이하 중복은 유지한다. 도구 목록과 클라이언트 계약 스냅샷은 변경하지 않았다.

## 검증

Go 1.27.1 / macOS ARM64에서 수행했다. 최초 전송 회귀 검증은 합성 입력과 시험 처리기·루프백 HTTP 서버를 사용했다. 이후 실제 개발 DB 통합 재검증은 아래에 별도로 기록한다. 실제 사용자 본문과 외부 API는 사용하지 않았다.

- 수정 전 회귀: 컴파일 오류를 정리한 뒤 본문 초과 요청이 `200` 또는 `401`로 처리되고, 과다 배열 필터가 도메인 처리기로 넘어가는 실패를 확인했다.
- [회귀 시험](../../../../internal/mcp/body_limit_test.go): 상한 미만·정확한 상한의 인증된 호출은 정상 처리됐다. 미인증 상한 이내 요청은 기존 `401`을 유지했다.
- 선언 길이 초과는 0바이트를 읽고 거부했다. 길이 미상·작게 선언된 8MiB 입력은 처리기 읽기가 최대 1048577바이트였으며 초과 뒤 스트림이 닫히고 인증·도메인 호출은 0회였다.
- 큰 문자열과 원소가 많은 JSON 배열의 해석 중 초과도 `413`으로 분류됐다. `id: null`, 안전한 메시지·상한 데이터와 인증 도전 없음도 확인했다.
- `server/discover`, `tools/list`, `tools/call`에 같은 상한이 적용됐다. Origin·메서드·필수 헤더가 먼저 실패하는 요청은 본문을 읽지 않았다.
- 실제 루프백 HTTP: 미인증 상한 이내 요청은 `401`, `Content-Length` 초과와 chunked 초과는 `413`이었다. JSON-RPC 오류 외피도 확인했다.
- 8000자 보조 평면 문자 텍스트 두 개와 UUID 참조 1000개를 모두 이스케이프한 본문은 411735바이트였고 해석·전송 검증을 통과했다. 도메인 처리기는 시험 대역이며 참조의 실존·중복 등 업무 규칙을 검증한 결과가 아니다.
- 필터 네 종류의 빈 배열·최대 원소 수·상한 이하 중복은 허용됐다. 상한 초과는 `invalid_argument`와 필드 이름을 반환하고 도메인 처리기를 실행하지 않았다. 공개 `maxItems`도 대조했다.

```shell
go test -race ./internal/mcp ./cmd/server -count=1
go build ./...
go vet ./...
# client/에서도 별도로 실행
go build ./...
go vet ./...
# 저장소 루트
gofmt -l .
git diff --check
```

관련 race 테스트와 서버·클라이언트 build·vet는 통과했고 gofmt의 미정리 파일은 없었다. 공유 계약 스냅샷 시험도 MCP 패키지 검증에 포함됐다. 초기 서버 race 실행은 샌드박스의 루프백 bind 차단으로 실패했으나 허용된 실행 환경에서 재실행해 통과했다.

최초 실행에서는 `.env`에 정의된 `TEST_DATABASE_URL`을 테스트 프로세스에 주입하지 않아 DB 통합 테스트 5개를 건너뛰었다. 개발 DB는 정상 실행 중이었으며 DB 부재가 아니라 실행 환경 확인 누락이었다. 서버 전체 테스트·별도 클라이언트 테스트·실제 운영 부하 시험은 실행하지 않았다.

## 실제 개발 DB 통합 재검증

2026-10-04 11:04 KST, 사용자 요청에 따라 `.env`의 `TEST_DATABASE_URL`·`AGE_GRAPH_NAME`만 값 노출 없이 테스트 프로세스에 주입했다. 접속 대상이 로컬 주소임을 확인하고 `TEST_DATABASE_REQUIRED=1`로 건너뛰기를 막았다. `.env` 파일 자체는 변경하지 않았다.

```shell
go test -race ./internal/mcp -run Integration -count=1 -v -timeout=5m
```

다음 5개 테스트가 실제 개발 DB에서 모두 통과했다. 패키지 실행 시간은 3.622초였고 건너뛴 항목과 race 오류는 없었다.

- `TestOperationContractIntegration`: 13개 도구의 연산 계약
- `TestHandlerIntegration`: 생성·조회·갱신·검색 등 처리기 동작
- `TestOperationIsolationIntegration`: 13개 도구의 그래프 격리
- `TestRelationListUsesRelationPageIntegration`: 관계 목록의 페이지 크기
- `TestRelationOperationsDoNotConsumeWriteRateIntegration`: 관계 연산의 쓰기 요청 빈도 제외

DB에 합성 시험 데이터를 쓰는 기존 통합 테스트를 실행했다. 검색은 임베딩 대역을 사용하므로 Gemini 등 외부 API를 호출하지 않았다. `embedding_unavailable` 로그는 실패 대역으로 검색의 부분 실패를 검증하는 과정에서 발생한 예상 로그다. DB 재기동·초기화·마이그레이션·운영 배포는 수행하지 않았다.

## 계약과 제한

1MiB는 기존 최대 텍스트·참조의 일반 JSON 표현을 수용하는 전송 안전 시작값이며 전체 동시 요청 메모리 상한이나 측정된 운영 용량이 아니다. JSON 해석의 객체·배열 오버헤드와 동시 요청의 합산 비용은 남는다. 본문 상한은 계정 플랜과 별개이며 새 환경 변수를 추가하지 않았다. 역방향 프록시의 본문 상한이나 운영 배포 구성은 변경하지 않았다.

루트→internal/mcp 및 루트→spec→service→agent-context DOX 체인을 확인했다. 전송 안전 경계의 소유 계약을 internal/mcp/AGENTS에 반영하고 SDD·개발 계획·공식 근거·요청 기록을 동기화했다. 서비스 요구사항·패키지 경계·소유권·Child DOX Index는 바뀌지 않아 SRS와 나머지 AGENTS는 변경하지 않았다. 한국어 표현·맞춤법·상대 링크와 diff를 점검했다.

DB 재검증은 검증 결과만 보완하므로 추가 계약 변경 없이 개발 계획·검증 기록·요청 기록을 갱신했다. 이 재검증으로 AGENTS·SRS·SDD를 추가 변경하지 않았다.

구현과 로컬 검증까지 수행한 결과다. 커밋·푸시·PR·GitHub 이슈 종료·운영 배포는 수행하지 않았다.
