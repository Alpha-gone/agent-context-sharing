# MCP 전송 계층

## 목적

- `internal/mcp/`는 Streamable HTTP MCP 전송 검증, discovery 문서, 도구 정의·입력 검증과 도메인 오류 직렬화를 담당한다.

## 소유권

- 루트 `AGENTS.md`가 구현 공통 계약과 이 패키지의 상위 경계를 소유한다.
- 이 문서는 MCP 전송 계층의 로컬 계약을 소유한다.

## 로컬 계약

- `server/discover`는 지원 protocol revision, `tools` 기능과 `io.github.alpha-gone/write-idempotency` 확장만 알린다.
- 협상된 쓰기에서 전송 계층은 `Idempotency-Key` 헤더 값과 요청 지문만 넘기고, 키 형식 검증과 결과 재생은 권한 확인 뒤 처리기가 맡는다.
- `tools/list`는 `SRS.md`의 MCP 연산 13종만 이름, 설명과 `inputSchema`로 노출하며 캐시 힌트를 함께 싣는다.
- 공개 도구 정의는 별도 클라이언트 모듈의 `client/internal/client/contract/tool_manifest.json`과 스냅샷 시험으로 대조한다. 서버 계약을 의도적으로 바꿀 때만 검토 후 스냅샷을 갱신한다.
- `client_live`의 `TestClientServiceLive`는 실제 저장소·인가·MCP를 조립하고 별도 클라이언트 모듈을 HTTP로 시험한다. 전체 서버 저장소 시험과 DB를 분리하며, 세션·브라우저 대체와 갱신 구간 토큰 fixture를 실제 사용자 로그인·시스템 브라우저 검증으로 보고하지 않는다.
- 모든 결과는 `resultType`과 서버 정보를 담은 응답 외피 안에 넣는다.
- `tools/call`은 `SDD.md`의 「요청 처리 순서」에서 전송·인증 뒤에 파라미터 형식과 상한을 검증하고, 계층별 속성 규칙은 `model`에 위임한다.
- `/mcp` 본문은 JSON 해석 전에 SDD의 고정 바이트 상한을 적용하며 초과는 인증 전에 전송 오류로 거부한다. 배열 필터의 서버 검증은 공개 스키마의 `maxItems`와 일치해야 한다.
- 요청 `id`는 문자열 또는 숫자만 받고 원문을 보존한다. 누락·`null`·불리언·객체·배열은 인증 전에 `400`, `-32600`, `id: null`로 거부한다.
- JSON 문법 오류만 `-32700`으로 반환한다. 문법상 유효한 배치·스칼라·중복 필드와 요청 필드 형식 오류는 헤더·본문 대조와 인증 전에 `400`, `-32600`, `id: null`로 거부한다.
- 보호 요청은 `Authorization: DPoP`와 `DPoP` 헤더를 각각 하나만 받고, 토큰 실패와 proof 실패를 `invalid_token`·`invalid_dpop_proof` DPoP 도전으로 구분한다. 인증 정보가 없을 때의 도전에는 오류 값을 넣지 않으며 Bearer 하향을 허용하지 않는다.
- 웹 전용 채널 경계는 명시된 요청 이름만 열거해 판정하며, 이름 패턴으로 새 도구를 추측하지 않는다.
- 전송 계층 실패는 HTTP 상태와 JSON-RPC 오류로, 도메인 결과는 확정된 8개 MCP 오류 코드로 분리한다. 인증 실패는 도메인 결과가 아니라 `401`과 `WWW-Authenticate` 도전으로 답한다.
- 응답에 내부 오류 원인, 접근 토큰, 세션 쿠키, 비밀번호 또는 컨텍스트 본문을 넣지 않는다.
- CRUD·권한·플랜·기록의 업무 처리는 이 패키지의 호출 처리기 경계 뒤에 두며, `store`의 연결이나 질의 실행기를 노출하지 않는다.

## 작업 지침

- MCP protocol revision, 도구 이름, 입력 필드와 오류 코드는 `SRS.md`와 `SDD.md`의 현재 계약을 먼저 확인한 뒤 변경한다.
- JSON Schema는 사전 검증용이며 서버 입력 검증을 대체하지 않는다.
- 새 도구나 새 도메인 오류 코드를 추측하여 추가하지 않는다.

## 검증

- 전송 검증 순서, Origin·헤더 대조, 인증 실패, 도구 정의와 도메인 오류 사상 단위 테스트를 실행한다.
- `TestToolManifestMatchesClient`로 클라이언트 공유 계약과 실제 도구 정의의 일치를 확인한다.
- 그래프·노드 처리기의 등급, 트랜잭션 경계, 멱등성과 채널 구분은 실제 AGE 통합 테스트로 검증한다.
- Go 코드 변경 뒤 Go 1.27.1로 `gofmt`, 관련 테스트, `go build ./...`, `go vet ./...`를 실행한다.

## Child DOX Index
