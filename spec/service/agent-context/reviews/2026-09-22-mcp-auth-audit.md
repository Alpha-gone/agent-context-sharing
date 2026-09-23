# MCP 전송·OAuth 구현 결함 분석

## 문서 정보

- 점검일: 2026-09-22
- 대상: 현재 `dev` 작업 트리의 서버 구현(`internal/mcp`, `internal/authz`, `cmd/server`)
- 문서 성격: 특정 시점의 구현 점검 기록이다. 기준 요구사항은 `../SRS.md`, 상세 설계는 `../SDD.md`이며, 이 문서만으로 계약을 변경하지 않는다.
- 처리 상태: 2026-09-23에 발견 사항 여섯 건을 모두 처리했다. 각 항목의 근거를 명세 원문으로 다시 대조해 확인한 뒤 `../SRS.md`와 `../SDD.md`의 계약을 먼저 고치고 `internal/mcp`, `internal/authz`, `cmd/server`의 구현과 계약 테스트를 맞췄다. 반영 내용은 아래 「처리 결과」에 있다.
- 범위: MCP 2026-07-28 전송 계약, OAuth 인가 코드 흐름, PKCE, Bearer 토큰 오류 사상과 관련 계약 테스트
- 제외: 데이터베이스가 필요한 통합 환경의 실제 OAuth 왕복 검증은 접속 정보가 없어 수행하지 않았다.

## 판단 기준

구현 결함은 두 축으로 분리해 판정했다.

- 공개 프로토콜 계약: MCP 2026-07-28 명세와 OAuth 표준을 기준으로 상호 운용성과 보안 영향을 판단한다.
- 저장소 계약: `SRS.md`와 `SDD.md`에 이미 정한 요구사항을 기준으로 구현·설계·요구사항의 드리프트를 판단한다.

따라서 아래에서 **명세 보완 필요**로 표시한 항목은 코드를 먼저 임의로 바꾸지 말고 SRS와 SDD의 정합 결정을 선행해야 한다. 근거 URL과 반영 위치는 `../reference.md`의 2026-09-22 행에 기록했다.

## 발견 사항

### 심각도: 심각

- [x] **MCP 2026 요청·응답 외피가 필수 계약을 충족하지 않는다.**
  - 근거: `internal/mcp/transport.go`는 `tools/list`와 `tools/call`만 허용하며 `server/discover`를 지원하지 않는다. 요청 `_meta`에서는 `protocolVersion`만 읽고 필수 `clientCapabilities`를 검증하지 않는다. `ToolResult`와 RPC 결과는 필수 `resultType`을 넣지 않으며, `tools/list` 결과는 `ttlMs`, `cacheScope`를 누락한다. 지원하지 않는 프로토콜 버전도 MCP 2026의 `-32022`가 아닌 `-32019`로 처리한다. 손상된 JSON도 JSON-RPC Parse Error `-32700`이 아니라 Invalid Request `-32600`으로 돌려준다.
  - 영향: 표준 MCP 2026 클라이언트는 발견 요청, 캐시 동작, 결과 역직렬화 또는 버전 협상에서 서버를 호환하지 않는 것으로 판단할 수 있다.
  - 정합성: `SRS.md`는 MCP 2026 판을 요구하지만, 현재 SDD의 MCP 표면은 `tools/list`와 `tools/call` 중심으로만 구체화되어 있다. 외부 계약 위반과 함께 SRS/SDD의 세부 계약 공백이다.
  - 조치 방향: `server/discover`, 요청 메타데이터, 표준 오류 코드, 모든 결과의 `resultType`, `ListToolsResult` 캐시 필드를 SRS·SDD에 명시한 뒤 전송 계층과 계약 테스트를 함께 수정한다.

### 심각도: 높음

- [x] **유효하지 않은 Bearer 토큰을 HTTP 200 도메인 오류로 반환한다.**
  - 근거: `internal/mcp/transport.go`는 토큰 검증 실패를 JSON-RPC `unauthenticated` 결과로 쓰며 HTTP 상태를 성공으로 유지한다. `internal/mcp/transport_test.go`도 잘못된 토큰의 200 응답을 기대한다.
  - 영향: 표준 OAuth 자원 서버·프록시·클라이언트가 인증 실패를 성공 응답으로 취급하거나 갱신·재인증 흐름을 시작하지 못할 수 있다.
  - 정합성: 현 SRS와 SDD도 이 동작을 의도한 것으로 기술한다. OAuth Bearer 규칙의 401 요구와 충돌하므로 구현만의 결함이 아니라 명세 보완이 필요하다.
  - 조치 방향: 유효하지 않거나 만료된 접근 토큰은 `401 Unauthorized`와 적절한 `WWW-Authenticate: Bearer` 오류로 사상하고, JSON-RPC 오류를 함께 둘지 여부를 SRS·SDD에서 결정한다.

- [x] **토큰 요청의 `resource` 결속을 검증하지 않는다.**
  - 근거: `internal/authz/http.go`는 토큰 교환 시 `code`, `redirect_uri`, `code_verifier`만 `Exchange`로 전달한다. `internal/authz/authz.go`의 `Exchange` 서명에도 `resource`가 없고, `internal/authz/http_test.go`의 정상 토큰 요청도 `resource` 없이 성공한다.
  - 영향: 인가 요청에서 선택한 보호 리소스와 토큰 요청 사이의 결속이 사라져 resource indicator 기반의 대상 리소스 제한을 보장할 수 없다.
  - 정합성: SRS는 인가와 토큰 요청 모두에서 `resource`를 요구하지만 SDD의 인가 코드 흐름과 구현은 토큰 단계에서 이를 생략한다. 명백한 SRS·SDD·구현 드리프트다.
  - 조치 방향: 토큰 엔드포인트가 `resource`를 필수로 받고 인가 코드에 저장한 값과 정확히 일치하는지 검증하도록 SDD와 구현을 맞추고, 누락·불일치 회귀 테스트를 추가한다.

- [x] **PKCE `code_verifier` 형식과 최소 길이를 검증하지 않는다.**
  - 근거: `internal/authz/authz.go`는 `code_challenge`의 존재와 `S256` 방식만 확인한 뒤 임의 문자열 verifier를 해시한다. 현재 핸들러 테스트도 43자보다 짧은 verifier로 정상 토큰을 발급받는다.
  - 영향: RFC 7636이 요구하는 43~128자의 unreserved 문자 verifier와 충분한 엔트로피 보장이 약화되어 인가 코드 탈취 완화 효과가 낮아진다.
  - 조치 방향: verifier 문법·길이를 요청 경계에서 검증하고, 42자·허용되지 않은 문자·129자·정상 경계값에 대한 테스트를 둔다.

### 심각도: 보통

- [x] **인증 기반 인프라 장애를 모두 `unauthenticated`로 숨긴다.**
  - 근거: `internal/authz/cache.go`에서 서명 키 조회 또는 폐기 목록 조회의 저장소 오류가 인증 실패와 같은 오류로 합쳐지고, `internal/mcp/transport.go`도 검증 오류를 구분 없이 인증 실패 결과로 매핑한다.
  - 영향: 실제 서버·데이터 저장소 장애를 사용자가 재인증 문제로 오인하며, 관측·재시도·장애 대응이 지연된다.
  - 정합성: SRS는 JWKS 조회 실패를 `internal`으로 사상하도록 정한다. 구현과 오류 계약의 드리프트다.
  - 조치 방향: 유효하지 않은 자격 증명, 폐기된 토큰, 내부 키·폐기 목록 조회 실패를 형식화된 오류 유형으로 구분하고 외부 상태·감사 이벤트·회귀 테스트를 각각 맞춘다.

- [x] **OAuth 요청 검증과 메타데이터가 실제 공개 클라이언트 계약을 충분히 표현하지 못한다.**
  - 근거: `/authorize`는 `response_type`과 `scope`를 읽거나 검증하지 않는다. 토큰 응답의 `scope`는 `agent-context`가 아닌 서버 resource URL을 반환한다. 인증 서버 메타데이터에는 공개 클라이언트에 필요한 `token_endpoint_auth_methods_supported: ["none"]`가 없다. 성공·오류 리다이렉트에는 `iss`도 포함하지 않는다.
  - 영향: `response_type=code` 외 응답 유형의 묵인, scope 기반 클라이언트 호환성 저하, 기본 클라이언트 인증 방식 오해가 발생한다. `iss` 누락은 필수 위반은 아니지만 다중 인가 서버·mix-up 방어 호환성을 낮춘다.
  - 정합성: SDD는 단일 `agent-context` scope를 정하지만 구현이 다른 값을 반환한다. 공개 클라이언트와 `iss` 정책은 SRS·SDD에 더 명시할 필요가 있다.
  - 조치 방향: `response_type=code`, 허용 scope, 토큰 응답 scope를 명시적으로 검증·고정하고 메타데이터의 인증 방식을 선언한다. `iss`는 지원 여부와 metadata 선언을 설계에서 결정한다.

## 계약 테스트의 공백

현재 테스트는 상당수가 위의 비호환 동작을 성공 조건으로 고정하거나 필수 필드를 검사하지 않는다.

| 영역 | 현재 공백 또는 잘못 고정된 기대 | 추가·변경할 회귀 테스트 |
| --- | --- | --- |
| MCP 전송 | `tools/list`는 도구 목록만 검사하며 `resultType`, `ttlMs`, `cacheScope`를 검사하지 않는다. | `server/discover`, 필수 `_meta`, 버전 오류 `-32022`, 파싱 오류 `-32700`, 모든 결과 형식을 표 기반으로 검증한다. |
| Bearer 인증 | 잘못된 토큰의 HTTP 200을 기대한다. | 누락·만료·위조 토큰이 401 및 `WWW-Authenticate`로 사상되는지 검증한다. |
| 인가 코드 | 토큰 요청의 `resource` 생략과 짧은 verifier를 정상 흐름으로 둔다. | resource 누락·불일치, verifier 경계·문자 집합, 재사용 코드를 거부하는 사례를 분리해 검증한다. |
| OAuth 메타데이터 | issuer 존재 여부만 검사한다. | `response_type`, scope, 공개 클라이언트 인증 방식, `iss` 지원 정책을 메타데이터와 엔드포인트 응답 모두에서 검증한다. |

## 권장 처리 순서

1. SRS와 SDD에서 MCP 2026 외피, 401 사상, resource 결속, 공개 클라이언트·`iss` 정책을 먼저 확정한다.
2. `internal/mcp`의 요청 해석·응답 직렬화·HTTP 상태 사상을 표준 계약에 맞춘다.
3. `internal/authz`의 `resource` 전달, PKCE 검증, 오류 분류, OAuth 요청 검증을 구현한다.
4. 위 표의 계약 테스트와 데이터베이스 통합 테스트를 추가하고, 표준 MCP 2026 클라이언트 상호 운용성을 실제로 확인한다.

## 검증 근거와 한계

- `/Users/alphagone/sdk/go1.27.1/bin/go`와 임시 `GOCACHE`로 `go test ./...`, `go build ./...`, `go vet ./...`를 실행해 통과했다.
- `gofmt -l cmd internal migrations` 출력은 없었다.
- `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED`가 설정되지 않아 데이터베이스 의존 통합 테스트는 건너뛰었다. 따라서 이 문서의 결론은 코드 경로·단위 테스트·계약 대조에 근거하며, 실제 DB 장애 사상과 전체 OAuth 왕복은 별도 환경에서 재검증해야 한다.

## 처리 결과

2026-09-23에 여섯 건을 모두 처리했다. 착수 전에 각 항목의 근거를 `../reference.md`에 기록된 출처로 다시 대조해 여섯 건이 모두 실제 계약 위반임을 확인했고, 그 과정에서 `_meta` 필수 필드 누락이 헤더 불일치(`-32020`)가 아니라 `-32602`라는 점을 추가로 확인해 설계에 반영했다.

| 발견 사항 | 명세 반영 | 구현 반영 |
| --- | --- | --- |
| MCP 2026 요청·응답 외피 | SRS 「외부 연동 요구사항」·「공통 규칙」, SDD 「요청 처리 순서」·「MCP 표면」 | `internal/mcp/transport.go`의 `server/discover`, `_meta` 검증, 응답 외피, 캐시 힌트, `-32700`·`-32602`·`-32022` |
| Bearer 토큰의 HTTP 200 | SRS 「오류 코드」·「예외와 오류 처리」, SDD 「요청 처리 순서」·「메타데이터 문서」 | 인증 실패를 `401`과 `WWW-Authenticate`로 올리고 도메인 코드에서 제거 |
| 토큰 요청의 `resource` 결속 | SRS 「프로토콜 매핑」, SDD 「인가 코드 흐름」 10단계 | `Exchange`가 `resource`를 받아 코드 행의 값과 대조 |
| PKCE `code_verifier` 형식 | SRS 「프로토콜 매핑」, SDD 「인가 코드 흐름」 9단계 | `verifierPattern`으로 43~128자 unreserved 검증 |
| 인프라 장애를 인증 실패로 숨김 | SRS 「토큰 형식과 검증」, SDD 「토큰 검증」 | `ErrInvalidCredential`과 `ErrUnavailable`을 나누고 후자를 `500`으로 사상 |
| OAuth 요청 검증과 메타데이터 | SRS 「프로토콜 매핑」, SDD 「메타데이터 문서」 | `response_type`·`scope` 검증, 토큰 응답 scope 수정, `none`·`iss` 선언, 리다이렉트의 `iss` |

인증 실패를 전송 계층으로 올린 결과 도메인 오류 코드가 9종에서 8종이 되었다. `unauthenticated`가 `tools` 결과로 나올 경로가 없어졌기 때문이며, 이 변경은 `SRS.md`, `SDD.md`, `AGENTS.md` 계층과 `spec/service/agent-context-client/SRS.md`에 함께 반영했다.

「계약 테스트의 공백」 표의 네 영역은 `internal/mcp/transport_test.go`, `internal/authz/http_test.go`, `internal/authz/cache_test.go`, `cmd/server/renewal_test.go`에 회귀 테스트로 채웠다.

### 재검증 결과

- `go build ./...`, `go vet ./...`, `gofmt -l cmd internal migrations`를 통과했다.
- `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED`를 설정해 통합 계층을 포함한 `go test ./...`를 실행했다. 이 문서가 남긴 한계 중 데이터베이스 의존 계층의 미실행은 해소되었다.
- `internal/store`의 `TestReindexGraphDropsExcludedEmbeddingsIntegration`은 공유 데이터베이스로 전체 묶음을 실행할 때만 실패하며, 이 변경 이전 커밋에서도 같은 조건에서 재현된다. 이 점검의 범위 밖이고 별도 항목이다.
- 표준 MCP 2026-07-28 클라이언트와의 실제 상호 운용 확인과 전체 OAuth 왕복은 여전히 별도 환경이 필요하며 수행하지 않았다.

## 결론

점검 시점의 서버는 기본 단위 테스트와 빌드 검증은 통과하지만, MCP 2026 상호 운용성 및 OAuth 자원 결속에 영향을 주는 미해결 결함이 있었다. 특히 MCP 외피와 HTTP 인증 오류 사상은 외부 클라이언트 연결을 직접 차단할 수 있는 문제였다. 여섯 건 모두 SRS·SDD의 계약을 먼저 정합화한 뒤 구현과 회귀 테스트를 맞춰 처리했으며, 남은 것은 실제 클라이언트와의 상호 운용 확인이다.
