# MCP 전송과 도구 계약

목표 Streamable HTTP 전송, 도구 14종, 행위자 결합, 멱등성 외피, 입력·응답·오류의 정본이다. 보호 HTTP 전송 모듈은 구현했으며 기동 경로의 업무 연결·전체 서버/호스트 인수는 대기다.

[설계 전체](../SDD.md) · [요구사항과 설계 대응](traceability.md)

## 표준 MCP 경계

초기 목표 revision은 `2026-07-28` 하나다. [표준 versioning](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)에 따라 요청별 revision·기능을 판정하며 `initialize`·`notifications/initialized` 교환을 요구하지 않는다. 이전 revision으로 자동 하향하지 않으며 다른 revision은 공통 계약 테스트와 대상 호스트 인수를 통과한 뒤 추가한다.

`server/discover`는 서버의 필수 구현이며 호스트의 사전 호출은 선택이다. [발견 응답](https://modelcontextprotocol.io/specification/2026-07-28/server/discover)에 `supportedVersions: ["2026-07-28"]`, `capabilities.tools`, `_meta["io.modelcontextprotocol/serverInfo"]`, 고정 `instructions`를 제공한다. `tools/list`·`tools/call`은 발견 이력 없이 독립 처리한다.

각 요청의 `params._meta`에는 `io.modelcontextprotocol/protocolVersion`과 `io.modelcontextprotocol/clientCapabilities`를 필수로 받고 `io.modelcontextprotocol/clientInfo`는 선택으로 받는다. [기본 프로토콜](https://modelcontextprotocol.io/specification/2026-07-28/basic/index)에 따라 필수 메타데이터 누락은 HTTP `400`·JSON-RPC `-32602`다. 호스트가 선언한 client 정보는 인증 주체나 `agent_id`의 근거로 쓰지 않는다.

[Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)에 따라 `/mcp`는 단일 요청 POST에 `application/json` 결과를 반환한다. 요청 `Accept`는 JSON·SSE를 모두 포함하며 `Content-Type`은 JSON이다. 첫 구현은 SSE·`MCP-Session-Id`를 사용하지 않고 GET·DELETE는 `405`다. JSON-RPC 응답 POST는 거부한다. 접수 가능한 알림의 transport 응답은 `202` 빈 본문이나 이 revision의 HTTP core에는 클라이언트 알림이 없으며 `notifications/initialized`·`notifications/cancelled`를 수명주기로 제공하지 않는다.

`Origin`이 있으면 허용 출처를 검사하고 부적합하면 `403`이다. 모든 요청에 `MCP-Protocol-Version`·`Mcp-Method`, `tools/call`에는 `Mcp-Name`을 검증한다. 각각 본문 revision·method·`params.name`과 일치해야 하며 헤더 누락·형식 오류·불일치는 HTTP `400`·`HeaderMismatch(-32020)`다. 헤더 이름은 대소문자를 구분하지 않고 값은 구분한다. 표준 Base64 sentinel은 이름 값인 `Mcp-Name`에서 해독 후 비교하며 revision·method에는 적용하지 않는다. 도구에는 `x-mcp-header`를 지정하지 않으며 서비스 전용 필수 헤더를 추가하지 않는다.

미지원 revision은 HTTP `400`·`UnsupportedProtocolVersion(-32022)`와 `data.supported: ["2026-07-28"]`·`data.requested`를 반환한다. 유효한 revision·외피의 미지원 RPC는 HTTP `404`·`-32601`이다. 이전 `initialize` 요청은 필수 외피 누락 또는 미지원 메서드로 거부하고 지원 revision을 진단에 표시한다. 요청 본문 상한과 업무·조회 시간 예산은 유지한다.

완료 결과에는 `resultType: "complete"`를 포함하고 도구 결과의 표준 `content`, `structuredContent`, `isError`에 기존 도메인 결과·오류를 매핑한다. 도메인 오류 8종의 의미와 `context_flow_get` 평면 응답은 유지하고 기존 업무 도구 13종에 등록 도구 하나를 추가하여 목표 목록은 14종이다. SDK·호스트의 실제 적합성은 `TBD-AGENT_CONTEXT-064`에서 검증하며 revision 선정만으로 지원 완료를 주장하지 않는다.

### 보호 HTTP 구현 경계

`mcp.NewHandler`는 검증된 허용 Origin·Bearer 보호 함수와 완전한 14종 처리기를 받는다. Origin 원문 대조→POST·미디어 타입·표준 헤더 형식→제한된 바이트 읽기→Bearer 검증→JSON·메타데이터·헤더/본문 대조→SDK·도구 입력 검증 순서다. Origin이 없으면 허용하며 빈 값·복수·목록은 거부한다. 단일 JSON Content-Type은 매개변수를 허용하고, Accept는 JSON과 SSE의 유효한 양수 품질 범위를 요구한다. 구체적인 q=0 거부는 wildcard보다 우선한다.

전송 본문은 고정 SDK의 기본값인 4 MiB로 제한한다. 선언 길이가 초과하면 읽기 전에, 길이 미상·chunked·축소 선언은 제한된 읽기로 413·JSON-RPC -32600을 반환한다. 초과 요청은 인증·업무를 호출하지 않고 JSON 해석은 Bearer 뒤에 수행한다. 헤더로 확인할 수 있는 선행 거부는 본문을 읽지 않는다. 단일 JSON 이외의 Content-Type은 415, JSON·SSE를 함께 허용하지 않는 Accept는 400이다.

발견·목록의 ttlMs=0·private와 별도로 모든 HTTP 응답에 no-store를 유지한다. 공개 Host를 loopback 프록시로 전달하는 보호 생성자는 SDK의 자동 localhost Host 거부를 해제하고 정확한 Origin·Bearer와 바깥 TLS 판정을 적용한다. 인증 없는 SDK 관찰 경계는 이 옵션을 사용하지 않는다. SDK의 PropagateRequestCancellation을 명시해 원래 요청의 context를 유지한다. 업무 시간 예산·커밋/재생·실제 단절 인수는 후속 MCP-08이 소유한다.

[MCP-02 인수](../reviews/2026-10-20-mcp-02.md)는 실제 OAuth/TLS와 서버 라우터 주입까지 검증했다. 현재 main에는 업무 13종을 대신할 임시 처리기를 넣지 않는다. MCP-06·07에서 실제 업무 처리기를 모두 주입하고 MCP-09의 전체 서버 계약·TEST-08의 실제 호스트 시험으로 직접 연결을 인수한다.

## 공통 행위자·도구·쓰기 안전성

`agent_register({registration_key})`는 인증 계정과 접근 토큰의 검증된 `client_id`에 결합해 UUIDv7 `agent_id`를 반환한다. `agent_registration`은 식별자, 계정, OAuth client, 등록 키, 생성 시각과 폐기 시각을 보관하고 `(account_id, client_id, registration_key)`를 유일 키로 둔다. 동시 등록은 upsert로 같은 활성 식별자를 반환하며 폐기한 키는 새 호출의 암묵적 복구 수단으로 사용하지 않는다.

등록 키는 16~128자의 ASCII `[A-Za-z0-9_-]`이며 계정당 활성 등록 상한은 128개다. 초과는 `limit_exceeded`다. 등록 키는 자격 증명이 아니며 이름·대화 제목·본문을 넣지 않는다. 기본 키 선택과 반환 식별자의 후속 사용은 공통 server instructions·도구 설명으로 안내하고 호스트별 코드·대화 ID 자동 전달에 의존하지 않는다. 등록은 애플리케이션별 감사 구분이며 개별 모델·프로세스의 신원 인증을 보장하지 않는다. 등록 폐기는 인증된 `/connections` 관리 POST의 agent 대상 동작으로 제공하고 세션 계정의 소유만 허용한다. 과거 감사 기록은 남기며 이후 같은 등록 ID의 쓰기는 거부한다.

행위자 필요 6종은 등록된 `agent_id`를 입력으로 받는다. 서버는 활성 등록의 계정·client 결합을 확인하고 기존 저장 속성과 관리 기록을 채운다. 임의 UUID와 다른 계정·client의 등록은 같은 `invalid_argument`로 거부한다. 기존 감사 UUID와 과거 컨텍스트는 변경하지 않으며 권한 판정에는 agent 식별자를 사용하지 않는다.

기존 업무 쓰기 8종은 선택 `request_id` 입력을 받는다. 키 형식은 등록 키와 같은 16~128자 ASCII이며 UUIDv7만 요구하지 않는다. `idempotency_record`의 키 저장을 문자열로 확장하고 계정 범위·최초 24시간·요청 지문·최초 결과 저장은 유지한다. 지문은 도구 이름과 정규화 입력에서 `request_id`만 제외하며 `agent_id`는 포함한다. 표준 결과 외피와 OAuth 자격은 지문에 넣지 않는다.

현재 인증·그래프 권한·행위자 결합을 확인한 후에만 결과를 재생한다. 키 없는 쓰기는 실행하되 불확정 결과의 자동 재시도 안전성을 광고하지 않는다. 멱등성 키를 유지한 재시도에서도 현재 권한·행위자 결합과 24시간 유효성을 확인한다. 완료 결과 재생은 기대 판을 다시 적용하지 않으며 새 키의 쓰기만 현재 판의 충돌을 검사한다. 등록 도구는 유일 키 자체로 멱등 처리하며 업무 쓰기 8종의 보관과 구분한다.

표준 도구 annotations의 읽기·변경 특성과 각 입력 `description`에 필수 조건, 예외, 등록·멱등성·재조회 규칙을 넣는다. JSON에는 주석 문법을 넣지 않으며 #131의 스키마 설명 보강과 연결한다. `instructions`는 등록 후 도구 사용, 그래프 선택, 결과 불명확 시 재조회와 반복 인가 금지를 안내하되 서버의 검증을 대체하지 않는다.

### 등록 저장 열

`agent_registration`은 아래 열을 사용한다. 선택 열 이외에는 `NOT NULL`이며 시각은 UTC로 입출력한다. OAuth grant와 수명이 다르므로 계열 폐기·만료로 등록이나 과거 기여 식별자를 삭제하지 않는다.

| 열 | SQL 타입·NULL | 키·검증 |
|---|---|---|
| `agent_id` | `uuid`, PK | 서버 발급 UUIDv7. 판 검증은 모델·저장소 |
| `account_id` | `uuid` | 검증한 등록 계정. 저장소가 계정 존재를 확인 |
| `client_id` | `text` | 접근 토큰에서 검증한 사전 등록 client |
| `registration_key` | `text` | 16~128자 ASCII `[A-Za-z0-9_-]` CHECK |
| `created_at` | `timestamptz` | 최초 등록 시각, 재등록으로 변경하지 않음 |
| `revoked_at` | `timestamptz`, 선택 | 본인 관리 폐기 시각. NULL이면 활성 |

UNIQUE `(account_id, client_id, registration_key)`는 폐기된 행에도 적용한다. 활성 행만의 부분 UNIQUE로 만들면 폐기 키를 새 활성 ID로 암묵 복구할 수 있으므로 사용하지 않는다. 계정·client별 목록과 활성 128개 한도 판정에는 `(account_id, client_id, created_at, agent_id)`와 `account_id WHERE revoked_at IS NULL` 인덱스를 둔다. 상한은 CHECK로 세지 않고 계정 범위 잠금 아래 재검사하며, UUID 타입·유일 제약만으로 활성·계정/client 결합이 검증되었다고 간주하지 않는다.

### 등록 구현 경계

[MCP-04 모듈 인수](../reviews/2026-10-20-mcp-04.md)에서 등록·현재 결합 검사·소유자 폐기 API를 구현했다. `store.RegisterAgent`와 `RevokeAgent`는 계정 행을 같은 순서로 잠그고 Read Committed의 별도 명령으로 현재 등록·개수를 대조한다. 활성 재조회는 상한에서도 ID·생성 시각을 유지하며 폐기 키는 복구하지 않는다. 모든 client의 활성 개수를 계정당 128개로 제한하고 소유 폐기로 빈자리를 회복한다. 폐기는 반복해도 최초 시각을 유지하며 다른 계정·없는 등록은 같은 오류다.

`model.ValidateRegistrationKey`가 저장 방식과 독립적인 키 형식을 소유한다. `mcp.RegisterAgent`와 `BindActor`는 인증 경계가 주입한 Principal의 계정/client만 사용하며 저장소는 AgentRegistry 인터페이스로 주입한다. clientInfo·agent·등록 키로 그래프 등급을 만들지 않는다. BindActor는 매번 현재 활성 결합을 확인하고 검증한 계정·agent만 반환한다. 누적 상한은 plan.LimitError의 agent_registrations_per_account로 전달하고 저장 해석·조회 장애는 입력 거부와 구분한다. 보호 /mcp 전송·실제 Bearer와 등록의 결합 모듈은 MCP-02에서 인수했다. 기동 경로의 여섯 업무 쓰기·/connections 목록과 POST의 연결은 MCP-06·AUTH-10의 후속 범위다.

## 요청 처리 순서

보호 HTTP·인증 주체 전달 모듈은 구현했으며 실제 업무 도구의 조립은 후속 대상이다. 표준 SDK 메시지·입력 검증, Bearer 검증·현재 권한 검사·행위자 결합, 도메인 연산·멱등성 트랜잭션, 출력 격리 확인과 표준 결과 매핑 순으로 수행한다. HTTP 오류와 도메인 오류를 구분하고 내부 장애를 재인증 신호로 바꾸지 않는다. 세부 정본은 [표준 MCP 경계](mcp.md#표준-mcp-경계)·[공통 행위자·도구·쓰기 안전성](mcp.md#공통-행위자도구쓰기-안전성)이다.

저장 계층의 멱등성·관리 기록·권한 재검사·원천 불변 규칙은 유지하며 전송 어댑터 부재를 도메인 계약의 제거로 해석하지 않는다.

## 연산 계약

| 연산               | 필요 등급          | 트랜잭션  | 멱등성 |
|--------------------|--------------------|-----------|--------|
| `graph_list`       | 열람자 (결과 필터) | 읽기 전용 | 멱등   |
| `graph_create`     | 인증된 계정        | 단일 쓰기 | 비멱등 |
| `graph_get`        | 열람자             | 읽기 전용 | 멱등   |
| `graph_update`     | 편집자             | 단일 쓰기 | 조건부 |
| `node_create`      | 편집자             | 단일 쓰기 | 조건부 |
| `node_get`         | 열람자             | 읽기 전용 | 멱등   |
| `node_update`      | 편집자             | 단일 쓰기 | 조건부 |
| `node_discard`     | 편집자             | 단일 쓰기 | 비멱등 |
| `node_restore`     | 편집자             | 단일 쓰기 | 비멱등 |
| `context_flow_get` | 열람자             | 읽기 전용 | 멱등   |
| `relation_list`    | 열람자             | 읽기 전용 | 멱등   |
| `relation_confirm` | 편집자             | 단일 쓰기 | 멱등   |
| `relation_discard` | 편집자             | 단일 쓰기 | 비멱등 |

등급은 [SRS 등급별 허용 연산](../SRS.md#등급별-허용-연산)이 확정한 값을 그대로 옮긴 것이다. `graph_create`만 기존 권한을 요구하지 않으며 생성한 계정이 소유자가 된다.

`조건부`는 `expected_version`이 있는 수정 연산이다. 같은 요청을 두 번 보내면 첫 번째가 `version`을 올리므로 두 번째는 `version_conflict`로 거부된다. 호출자가 응답을 받지 못한 채 재시도해도 중복 적용이 일어나지 않으므로, 낙관적 잠금은 같은 판의 중복 적용을 막지만 첫 호출의 성공 결과를 재생하지 않는다. 불확정 쓰기의 확인은 같은 `request_id` 재시도 또는 재조회로 수행한다.

`relation_confirm`이 멱등인 근거는 [SRS 사건과 관계](../SRS.md#사건과-관계)가 "같은 정체성의 관계를 다시 확정하는 요청은 오류가 아니다"라고 확정한 것이다. 정체성이 그래프·유형·양 끝의 조합이므로 재확정은 기존 관계의 현재 상태를 반환한다. 관계에는 `version`이 없어 조건부가 될 수 없고, 대신 정체성이 중복을 막는다.

`node_create`가 조건부인 근거는 계층에 따라 갈리기 때문이다. 원천은 [`SRS.md`](../SRS.md)의 [SRS 생성과 수정 검증](../SRS.md#생성과-수정-검증)이 `source_ref`를 자연 키로 확정해 같은 값의 재요청이 기존 것을 돌려주므로 멱등이다. 파생과 사건에는 자연 키가 없어 재요청이 새 컨텍스트를 만든다.

폐기와 복구가 비멱등인 근거는 확정된 오류 매핑이다. 이미 폐기된 컨텍스트와 관계의 재폐기는 `invalid_argument`로 거부한다. 컨텍스트의 재폐기·재복구는 사전 상태 확인과 동시 쓰기 뒤 재조회 모두 `ErrInvalidState`로 전달해 같은 오류로 사상한다. 동시 전이에서 AGE 갱신 오류를 받더라도 현재 상태가 요청 전이와 같으면 상태 오류이며, 본문·판만 달라졌으면 `version_conflict`다.

`node_restore`의 대칭 경우인 이미 활성인 컨텍스트의 복구 요청은 [`SRS.md`](../SRS.md)가 매핑하지 않았다. 재폐기와 같게 `invalid_argument`로 처리한다. 대칭으로 두는 이유는 두 연산이 같은 상태 값을 반대로 바꾸기 때문이며, 성공으로 처리하면 호출자가 자기 요청이 실제로 상태를 바꿨는지 알 수 없다. 오류 코드는 기존 8종을 넘지 않는다.

표의 멱등성은 키가 없는 도메인 연산 자체의 성질이다. 목표 직접 연결의 선택 입력 `request_id`는 이 분류를 바꾸지 않고 8종 쓰기의 같은 논리적 호출을 재생 안전하게 만든다. 따라서 새 키로 보낸 `graph_create`나 파생 `node_create`는 여전히 새 대상을 만들고, 같은 키로 다시 보낸 요청만 최초 결과를 받는다. `graph_update`와 `node_update`도 같은 키면 최초 성공 결과를 재생하지만 새 키와 오래된 `expected_version`이면 기존처럼 `version_conflict`다.

## 입력 검증

아래 MCP 입력 검증 분담은 표준 전송 계층의 계약이다. 14종 도구의 스키마·표면 검증은 구현했으며 실제 인증·업무 저장 연결은 후속 대상이다. 모델·저장 계층의 검증과 웹 입력 검증은 유지한다.

검증은 `model`과 `mcp`가 나눠 맡는다.

컨텍스트 문자열은 유효한 UTF-8이어야 하며 U+0000을 포함할 수 없다. `model`에서 저장 전에 검증하고 위반 필드를 담은 입력 오류로 반환한다. 질의 문자열 변환에서 유효하지 않은 문자열을 만나면 AGE가 처리할 수 있는 `null` 리터럴로 대체하여 일치하는 행이 없게 한다.

사건 시간 갱신은 같은 트랜잭션에서 연결된 확정 관계의 시간 제약을 먼저 검사하며 위반하면 입력 오류로 거부한다. 제안 상태의 관계는 이후 시간 갱신을 제한하지 않는다. 사용자 쓰기는 트랜잭션 안에서 활성 그래프를 재확인하고 삭제된 그래프에는 변경과 기록을 커밋하지 않는다.

| 검증 대상                                                               | 위치    |
|-------------------------------------------------------------------------|---------|
| 파라미터 존재, 타입, 길이, 열거형 값, 커서 형식, `locator`의 URI scheme | `mcp`   |
| 계층별 필수 속성, 속성 사이의 조건부 필수와 불변                        | `model` |
| 시점 순서, 사건 시간 범위, 관계의 양 끝 유형과 순환                     | `model` |

`model`에 두는 검증은 저장 방식과 무관한 규칙이고, `mcp`에 두는 검증은 연산 표면의 규칙이다. 같은 규칙을 두 곳에 두지 않으므로 한쪽만 고쳐 어긋나는 일이 없다.

도구 목록에 제공하는 스키마를 서버에서도 검증한다. 적용 판 `jsonschema-go`는 `format`을 검증하지 않으므로 UUIDv7·실제 UTC 달력 시각·URI·불투명 커서의 Base64 URL 인코딩은 MCP에서 보완한다. 커서 내부 값·현재 대상의 계층·시간 순서·참조 존재·권한·행위자 결합은 각 소유 모델·저장·권한 경계가 판정한다. 필드별 위반은 입력 원문 없이 `invalid_argument`와 필드로 반환한다. 정수 입력은 소수부가 0인 표기와 지수 표기를 포함해 정밀도 손실 없이 해석한다.

`context_flow_get`의 `as_of`는 `FR-AGENT_CONTEXT-082`에 따라 미지정이면 현재 UTC를 사용한다. 내부 검색 입력도 `model`과 같은 `Location() == time.UTC` 기준으로 검사하며, 같은 순간을 표현하는 지역 시간대나 별도 고정 시간대는 UTC 입력으로 간주하지 않는다. 검증은 검색 저장소·임베딩·읽기 스냅숏을 사용하기 전에 완료한다.

`node_create`가 계층에 따라 다른 필수 속성을 요구하므로 파라미터 검증만으로는 부족하다. `mcp`가 `layer` 값이 셋 중 하나인지 보고, `model`이 그 계층의 필수 속성이 다 있는지 본다. `supersedes_context_id`는 파생에서만 허용하고 이전 파생의 존재·계층·그래프 일치는 저장 계층이 같은 트랜잭션에서 확인한다. `management_action: keep`은 `node_update`에서 가변 필드를 허용하지 않고 판 번호만 검사해 상태 전이 없는 관리 기록을 남긴다. 컨텍스트를 바꾸는 세 연산(`node_update`, `node_discard`, `node_restore`)과 관계를 확정·폐기하는 두 연산도 등록된 실행 에이전트를 `agent_id`로 받는다. 접근 토큰은 계정만 대표하므로, 이 값이 없으면 적용·거부 기록의 실행 에이전트와 연산 폐기 채널을 정확히 남길 수 없다. 이 값은 생성 시의 같은 이름 속성을 바꾸지 않으며, 해당 요청을 대행한 에이전트의 감사 값으로만 쓴다.

`judgment_input`은 컨텍스트 생성·갱신·폐기·복구와 관계 확정·폐기의 선택 입력이다. 8000자 이하의 비어 있지 않은 텍스트만 허용하며, 병합·분할처럼 한 판단에서 나온 여러 요청에 같은 값을 넣어 기록을 묶는다. 생략한 요청은 검증을 마친 인자 묶음을 기록한다.

`derived_from`과 `member_refs`는 개수 상한만 `mcp`가 맡는다. [SRS 입력 검증 세부](../SRS.md#입력-검증-세부)가 100개와 1000개라는 값을 정했으므로 위 분담표의 길이 검증에 해당한다.

다만 "1개 이상"은 `model`이 본다. [`SRS.md`](../SRS.md)의 [SRS 컨텍스트 모델](../SRS.md#컨텍스트-모델)이 둘을 필수 속성으로 두었으므로 상한과 달리 계층별 필수 속성에 해당하며, 저장 방식과 무관하게 근거 없는 파생이나 구성원 없는 사건이 만들어지지 않아야 한다. 같은 규칙을 두 곳에 두지 않는다는 원칙은 그대로다. 존재는 `model`이, 상한은 `mcp`가 한 번씩만 본다.

열거형 배열 필터도 공개 `inputSchema.maxItems`와 같은 원소 수 상한을 서버에서 검증한다. `grade_filter`와 `state_filter`는 3개, `traversal_filter`는 7개, `type_filter`는 4개다. 빈 배열과 상한 이하의 중복 값은 기존대로 허용하지만 중복도 원소 수에 포함한다. 초과는 도구 입력 검증에서 해당 필드를 명시한 `invalid_argument`이며, 전송 본문 초과의 `413`과 계정 플랜의 `limit_exceeded`와 구분한다. 참조 배열은 기존 100개·1000개 상한을 유지하므로 배열 종류마다 새 일괄 상한을 추가하지 않는다.

`body`와 `work_context`의 길이는 `mcp`가 본다. 둘 다 연산 표면의 파라미터이고 [SRS 입력 검증 세부](../SRS.md#입력-검증-세부)가 값을 정했으므로 위 분담표의 첫 줄에 그대로 해당한다.

`body` 상한을 데이터베이스 제약으로 걸지 않는 이유는 [그래프 스키마](data.md#그래프-스키마)가 밝힌 것과 같다. AGE의 property는 스키마를 갖지 않아 열 단위 제약을 쓸 수 없고, `body`는 `Context` 정점의 property다. 계층별 필수 속성과 원천 불변성을 접근 계층이 강제하는 것과 같은 자리에서 처리한다.

## 페이지 처리

커서 페이지 처리는 `graph_list`와 `relation_list`에 있다. [SRS 홉 범위 조회](../SRS.md#홉-범위-조회)가 홉 조회에 페이지 처리를 두지 않기로 확정했고, 나머지 연산은 단일 대상이나 부분 그래프를 반환한다.

두 연산의 커서는 내용만 다르고 규칙이 같다. `graph_list`는 마지막 행의 `last_activity_at`과 `graph_id`를, `relation_list`는 마지막 행의 `relation_id`를 담는다. `relation_id`가 UUIDv7이라 그 값 하나로 정렬 위치가 정해지므로 보조 키가 필요 없다.

| 항목      | 확정                                                           |
|-----------|----------------------------------------------------------------|
| 정렬      | `last_activity_at` 내림차순, 같으면 `graph_id` 내림차순        |
| 커서 내용 | 마지막 행의 `last_activity_at`과 `graph_id`                    |
| 인코딩    | 두 값을 이어 붙여 base64url로 감싼다                           |
| 계약      | 호출자에게 불투명하며 받은 값을 그대로 되돌려주도록 요구한다   |
| 유효성    | 해독할 수 없거나 형식이 어긋나면 `invalid_argument`로 거부한다 |

`graph_id`를 정렬 키에 더하는 이유는 `last_activity_at`이 같은 행이 있을 수 있기 때문이다. 두 값의 조합이 유일하므로 커서가 가리키는 위치가 하나로 정해지고, 같은 시각의 행이 건너뛰거나 중복되지 않는다.

`last_activity_at`은 쓰기로 커지는 값이다. 그래서 페이지를 넘기는 동안 활동한 그래프는 목록 앞으로 이동하고, 이미 지나간 페이지에 있으므로 그 페이징의 뒤 페이지에는 나타나지 않는다. 중복은 나지 않는다. 활동 시각이 커지기만 하므로 이미 본 행은 커서 뒤로 물러나기 때문이다. 누락을 없애려면 정렬 키를 불변 값으로 바꾸거나 스냅샷으로 목록 전체를 읽어야 하는데, 앞은 [SRS 그래프 목록 조회](../SRS.md#그래프-목록-조회)가 확정한 최근 활동순을 바꾸고 뒤는 페이지 처리와 [인스턴스 무상태성](architecture.md#인스턴스-무상태성)을 모두 무의미하게 한다. 이 귀결을 감수하기로 하고, 페이징 도중 활동한 그래프는 새 조회의 첫 페이지에서 다시 보인다.

커서를 서명하지 않는 이유는 위조가 격리를 깨지 않기 때문이다. 커서는 정렬 위치만 담고 권한 판정은 요청마다 별도로 수행하므로, 조작된 커서는 다른 위치의 목록을 볼 뿐 권한 없는 그래프를 보여주지 않는다.

## MCP 표면

직접 연결의 표면은 [표준 MCP 경계](mcp.md#표준-mcp-경계)를 따른다. 현재 `internal/mcp`에는 최소 SDK 진입 경계와 [14종 도구 정의·검증](../reviews/2026-10-18-mcp-03.md)이 있다. 공통 instructions와 도구의 description·annotations·스키마를 실제 SDK 목록·호출로 인수했다. 실제 서버 연결·업무 도구의 전체 결과 스냅샷·취소·호스트 인수는 대기 상태다. 실제 호스트 검증 전에는 지원을 주장하지 않는다.

## 응답 직렬화

후속 MCP 구현은 `resultType: "complete"`와 표준 `content`, `structuredContent`, `isError`에 도메인 결과·오류를 매핑한다. 작업 컨텍스트 흐름의 평면 결과를 유지하고 HTTP·JSON-RPC·도메인 오류 계층을 구분한다. 현재 웹 응답과 상태 경로는 각 처리기의 직렬화를 유지한다.

## 오류의 생성과 전달

오류는 만들어지는 곳과 코드로 바뀌는 곳을 나눈다.

| 계층    | 만드는 것                                 | 코드 사상       |
|---------|-------------------------------------------|-----------------|
| `model` | 규칙 위반을 담은 도메인 오류              | 하지 않는다     |
| `store` | 없는 대상, 버전 불일치, 데이터베이스 실패 | 하지 않는다     |
| `plan`  | 한도 초과와 초과한 항목·값                | 하지 않는다     |
| `perm`  | 등급 부족과 필요한 등급                   | 하지 않는다     |
| `authz` | 웹 세션 자격 실패·내부 장애를 나눈 오류 타입 | 하지 않는다 |
| `mcp` (목표) | 도메인 오류를 구조화된 도구 결과로 바꾼다 | 도메인 코드 사상 |
| `web` | 같은 오류를 HTTP 상태·고정 화면 안내로 바꾼다 | 웹 응답 사상 |

코드 사상을 진입점에만 두는 이유는 채널이 둘이기 때문이다. 같은 도메인 오류가 MCP에서는 `invalid_argument`가 되고 웹에서는 화면 메시지가 된다. 아래 계층이 코드를 만들면 웹이 쓰지 않는 값을 만들어 내려보내게 되고, 코드 8종이 MCP 규약이라는 사실도 흐려진다.

아래 계층은 대신 판별 가능한 오류 타입을 반환한다. Go의 오류 래핑으로 원인을 감싸 올리고 `mcp`가 타입을 확인해 코드를 정한다. 중간 계층이 원인을 삼키지 않으므로 사상 지점에서 필요한 부가 정보를 그대로 꺼낼 수 있다.

`internal`로 사상하는 오류는 원인을 응답에 담지 않고 허용된 원인 코드·단계·식별자만 진단 기록에 남긴다. 데이터베이스 오류 문구가 응답에 실리면 내부 구조가 노출된다.

웹 등급 부여·회수·복구 요청·운영자 복구는 래핑된 저장소 오류도 원인을 판별해 HTTP 상태와 고정 안내 문구를 정한다. 등급 하향·회수의 `ErrLastOwner`는 `409`와 마지막 소유자 안내로 표시한다. 없는 등급·복구 대상·미처리 요청과 잠금 뒤 소유자 등급을 잃은 행위자의 `ErrNotFound`는 `404`로 표시한다. 복구 요청 대상은 권한 부족과 자동 삭제 조건 불충족을 구분해 노출하지 않는다. 복구의 `ErrInvalidState`는 `409`로 표시한다. 그 밖의 저장소 장애는 `500`과 작업 실패 안내로 표시하며 내부 오류 원문은 응답에 담지 않는다. 성공 시 기존 `303` 이동은 유지한다.

## 부가 정보의 출처

[SRS MCP 연산 매핑](../SRS.md#mcp-연산-매핑)이 코드마다 함께 반환할 부가 정보를 확정했다. 그 값을 어디에서 얻는지 정한다.

| 코드                | 부가 정보        | 출처                                          |
|---------------------|------------------|-----------------------------------------------|
| `permission_denied` | 필요한 등급      | `perm`이 연산에 요구되는 등급을 담아 반환한다 |
| `invalid_argument`  | 위반한 필드      | `model` 또는 `mcp`의 검증이 필드명을 담는다   |
| `version_conflict`  | 현재 `version`   | `store`가 비교 시점의 값을 담는다             |
| `limit_exceeded`    | 초과한 한도와 값 | `plan`이 항목 이름과 두 값을 담는다           |
| `result_truncated`  | 잘린 홉 경계     | `store`의 홉 탐색이 절단 지점을 담는다        |
| `not_supported`     | 대체 채널        | `mcp`가 고정 값으로 채운다                    |

부가 정보를 오류에 실어 올리는 이유는 사상 시점에 다시 계산할 수 없기 때문이다. 필요한 등급은 등급 검사를 한 곳에만 있고, 현재 `version`은 트랜잭션 안에서 읽은 값이다.

## SDK와 수명주기

`FR-AGENT_CONTEXT-164`~`172`의 첫 구현은 공식 `github.com/modelcontextprotocol/go-sdk` `v1.8.0`을 사용한다. [공식 릴리스](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0)가 `2026-07-28` 지원을 명시하며 `ServerOptions.SupportedProtocolVersions`를 해당 revision 하나로 제한한다. SDK의 추가 revision을 자동 노출하지 않고 stateful handler를 사용하지 않는다. 실제 Go 빌드·무상태 transport 동작·호스트 적합성은 인수 시험 대상이다.

SDK의 지원 목록 제한만으로 이전 수명주기와 외피 없는 호출의 거부가 보장되지는 않는다. 서비스의 MCP 진입 경계는 SDK에 위임하기 전에 필수 외피·허용 revision과 메서드를 확인하고, SDK가 직접 처리하지 않는 표준 Base64 Mcp-Name의 해독·본문 대조를 보완한다. 오류는 위 표준 MCP 경계의 JSON-RPC 계약을 유지한다. SDK를 교체하거나 이전 revision을 허용하는 방식으로 우회하지 않는다. [MCP-01 검토](../reviews/2026-10-18-mcp-01.md#서비스-진입-경계-보완과-최종-인수)에서 최소 진입 경계와 SDK의 교환을 인수했다. 인증·Origin·서버 라우터 결합 모듈은 [MCP-02 검토](../reviews/2026-10-20-mcp-02.md)에서 인수했다. 기동 경로의 업무 13종 연결과 전체 서버 계약은 후속 구현 대상이다.

서버 정보·도구 설명은 고정 구성에서 만들며 사용자의 입력이나 자격 증명을 instructions에 넣지 않는다. 서버는 이전 요청·발견 결과·접속 인스턴스로 revision·기능을 추론하지 않는다. 표준 메타데이터·헤더·Origin·메시지 검증은 SDK 경계에 두고 인수 시험에서 발견을 생략한 호출과 다른 인스턴스로 이어지는 호출도 대조한다.

[캐시 계약](https://modelcontextprotocol.io/specification/2026-07-28/server/utilities/caching)에 따라 `server/discover`·`tools/list` 완료 결과에는 `ttlMs: 0`·`cacheScope: "private"`를 명시한다. SDK `ServerOptions.SetCacheable`로 적용하며 인증 맥락 간 캐시 공유를 허용하지 않는다. 캐시 힌트는 매 요청의 인증·등급 검사를 대체하지 않는다. 첫 구현은 구독·listChanged·sampling·elicitation·roots·MRTR와 별도 확장을 광고하지 않는다.

HTTP 취소는 해당 요청의 연결 단절과 서버 시간 예산으로 감지해 진행 중 DB·제공자 호출의 context에 전파한다. [표준 취소](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/cancellation)의 `notifications/cancelled`는 stdio용이며 이 HTTP 서버에 요구하지 않는다. JSON 단일 응답에서는 원격 단절 감지 시점에 한계가 있으므로 즉시 취소를 보장하지 않는다. 커밋 전에 실패하면 도메인 트랜잭션을 롤백한다. 이미 커밋한 쓰기의 HTTP 단절·취소는 적용 결과를 되돌리지 않으며 최초 멱등성 결과도 보존한다. 결과를 모르면 같은 키 또는 현재 상태 조회로 확인한다.

## 도구 입력과 반환 계약

업무 13종의 필드별 타입·필수·계층 조건은 [SRS 연산 목록](../SRS.md#연산-목록)과 [입력 검증](#입력-검증)을 스키마로 표현한다. `additionalProperties: false`를 적용하고 서비스 검증도 동일하게 수행한다. 문서에 없는 입력을 호스트별로 주입하지 않는다.

| 묶음 | 도구 | 공통 입력·출력 |
|---|---|---|
| 계정 등록 | `agent_register` | `registration_key` → 활성 계정·client에 결합된 `agent_id` |
| 그래프 읽기 | `graph_list`, `graph_get` | 필터·불투명 커서 또는 `graph_id` → 등급을 확인한 그래프·목록 |
| 그래프 쓰기 | `graph_create`, `graph_update` | 이름·설명, 수정 기대 판, 선택 `request_id` → 현재 그래프·판 |
| 노드 읽기 | `node_get` | graph·context, hops·방향·필터 → 평면 부분 그래프·최단 거리·절단 상태 |
| 노드 쓰기 | `node_create`, `node_update`, `node_discard`, `node_restore` | graph·context 또는 계층별 생성 값, `agent_id`, 선택 `request_id` → 현재 노드·판 |
| 작업 흐름 | `context_flow_get` | graph·work_context·as_of·scope·budget → contexts·references·relations·검색 메타데이터 |
| 관계 읽기 | `relation_list` | graph·context·상태·유형·커서 → 사건 관계 페이지 |
| 관계 쓰기 | `relation_confirm`, `relation_discard` | graph·관계 정체성 또는 식별자, `agent_id`, 선택 `request_id` → 현재 관계 상태 |

행위자 6종은 네 노드 쓰기와 두 관계 쓰기다. 멱등성 대상 8종은 이 여섯 쓰기와 두 그래프 쓰기다. 등록 도구는 자연 유일 키로 처리하며 업무 재생 저장의 24시간 수명과 분리한다. 각 스키마의 `description`은 계층·기대 판·등록 결합·재시도·채널 경계를 설명하고 annotations는 읽기·변경 특성을 표시한다. annotations와 호스트 승인은 서버 권한 검사를 대신하지 않는다.

성공 결과는 `resultType: "complete"`와 표준 `structuredContent`의 객체·같은 값을 나타내는 text `content`로 반환한다. 도메인 실패도 완료 결과 안의 `isError=true`와 `code`·`message`·필요 부가 정보로 제공한다. `result_truncated`는 `isError=false`인 정상 응답의 부분 상태다. 실제 도구별 구조는 계약 스냅샷으로 검증하며 구현 전 스냅샷이 존재한다고 보고하지 않는다.

## 전송 오류와 도메인 오류

| 단계 | 응답 |
|---|---|
| Bearer 부재·서명·만료·폐기 오류 | HTTP `401`, `WWW-Authenticate`와 보호 리소스 메타데이터 위치 |
| 단일 scope 부족 | HTTP `403` |
| 인증 저장소·키 검증 내부 장애 | HTTP `500`; 재인증 실패로 바꾸지 않는다 |
| 필수 요청 메타데이터 누락 | HTTP `400`, JSON-RPC `-32602` |
| 필수 표준 헤더 누락·형식 오류·본문 불일치 | HTTP `400`, JSON-RPC `-32020` |
| 미지원 protocol revision | HTTP `400`, JSON-RPC `-32022`와 지원·요청 revision |
| 유효한 외피의 미지원 RPC 메서드 | HTTP `404`, JSON-RPC `-32601` |
| JSON 해석·JSON-RPC 요청·메서드·params 오류 | 선택 revision과 SDK의 표준 JSON-RPC 오류 |
| 업무 입력·상태·등급·한도 오류 | 정상 도구 응답 안의 도메인 코드 8종과 `isError` |
| 빈 결과·일부 채널 실패·정상 상한 절단 | 정상 도구 결과와 부분 상태 |

인증 전에 큰 입력 해석·DB 업무를 시작하지 않는다. 현재 권한 없는 그래프와 존재하지 않는 그래프는 `not_found`로 통일하고, 접근 가능한 그래프에서 등급이 부족한 경우에만 `permission_denied`를 반환한다.

## 멱등성 저장과 경합

### 쓰기 도구 연결

MCP-06은 `NewWriteHandlers`에 현재 등급·플랜·그래프/노드 조회·등록·여덟 쓰기·거부 기록·재생 실행기를 주입한다. `cmd/server`의 저장 어댑터가 기존 Store API의 한도·관리 기록 형식을 맞추며 DB 핸들과 SDK 타입을 저장 계층으로 넘기지 않는다. 미사용 노드 폐기/복구 주입 계약의 기대 판 인자는 제거한다. 공개 입력은 바뀌지 않는다.

인증 경계의 Principal을 받은 뒤 재생 잠금 후 현재 그래프의 활성 상태·편집자 등급, 이어 활성 agent 결합을 확인한다. 최초 적용은 현재 계정 플랜을 전달하며 저장 경계가 누적·빈도 한도를 원자적으로 검사한다. 재생에는 조회 결과의 판을 다시 적용하거나 한도를 소비하지 않는다. Bearer의 전송 오류·시간 예산과 단절 처리는 MCP-08의 인수 범위다.

수정 입력은 생략과 명시한 빈 문자열·null을 구분하여 현재 값에 병합한다. 계층에 맞지 않는 수정 필드는 거부하고 원천 불변·keep·파생 대체·사건/관계 정체성과 복구 채널은 기존 저장 API에 위임한다. 판단 입력 생략 시 request_id를 제외한 정규화 도구·인자를 관리 기록에 남긴다. 생성/확정 거부는 대상을 비우며, 기존 노드의 거부 판은 조회한 현재 판 또는 충돌 오류의 판을 사용한다.

성공 완료 객체의 graph/node/relation은 snake_case 속성과 문자열 UUID·UTC 시각으로 직렬화한다. 거부는 code·message와 field/required_grade/current_version/limit/current/allowed/alternative_channel의 해당 값을 보존한다. 저장 내부 장애는 조립 어댑터가 원인을 보존한 DependencyFailure로 구분해 내부 오류에 포함된 필드 오류가 결정적 입력 거부로 저장되지 않게 한다. 결정적 거부만 저장점 롤백 후 거부 기록과 함께 재생 결과로 커밋하며, 내부 장애·취소는 완료 결과로 저장하지 않는다.

MCP-05의 실행 경계는 `WriteRunner`와 주입 `WriteExecutor.ExecuteWrite`다. 기존 미사용 WriteResult 전용 주입 계약을 도메인 완료 객체의 JSON 바이트 계약으로 구체화한다. 저장소는 MCP SDK·프로토콜 오류를 알지 않으며 성공·결정적 거부 객체만 보관한다. Apply의 error는 미완료 실패이며 예약까지 롤백한다. 도메인 거부는 해당 쓰기의 저장점 롤백 뒤 필요한 거부 감사를 남기고 오류 코드·부가 정보를 완료 객체로 직렬화한다. 여덟 도구의 변환은 [쓰기 도구 연결](#쓰기-도구-연결)을 따른다.

WriteRunner는 같은 공개 스키마로 인자를 검사한 뒤 request_id를 제외한다. 도구 이름·나머지 인자를 결정적으로 직렬화하며 객체 키 순서·JSON escape·정수의 소수/지수 표기·UUID의 허용 표기를 통일한다. 배열 순서·중복과 선택 필드 생략 여부는 유지하고 clientInfo·표준 외피·자격은 지문에 넣지 않는다. 문자열 키는 대소문자를 구분한다. WriteExecutor는 이 정규화 값의 SHA-256을 저장하고 같은 계정·키로 직렬화한다.

필수 Check 콜백은 인증·현재 등급을 읽기 전용으로 재검사한다. 키가 있으면 자문 잠금을 획득한 뒤, 재생 행을 읽기 전에 실행하며 WriteRunner는 뒤이어 여섯 쓰기의 현재 행위자 결합을 확인한다. 잠금 대기 중 상실한 권한·폐기 등록으로 이전 결과를 얻을 수 없다. 키가 없으면 같은 검사 뒤 일반 업무 쓰기를 수행하며 재생 행을 만들지 않는다. 키 대기·업무 쓰기의 예약은 바깥 경계를 공유한다. 기존 UUID 호출 API는 같은 내부 저장 경계에 위임해 기존 행·지문·결과를 유지한다.

같은 계정·`request_id`의 자문 잠금과 재생 행을 예약한 트랜잭션에서 지문을 대조한다. 요청 지문은 도구 이름과 키를 제외한 정규화 인자의 SHA-256이며 행위자·기대 판·판단 입력도 포함한다. 같은 키의 다른 요청은 `invalid_argument`다. 현재 인증·권한·행위자 검사를 통과한 뒤 기존 완료 결과를 재생한다.

최초 처리의 도메인 쓰기와 적용 감사·색인 작업은 같은 트랜잭션·저장점 안에 묶고 최초 성공 또는 결정적 도메인 거부 결과를 채운 뒤 커밋한다. 결과 없는 예약은 커밋하지 않는다. 일시 내부 장애·취소·연결 실패는 예약과 쓰기를 롤백하여 다음 같은 키가 다시 처리할 수 있게 한다. 재생은 저장된 결과를 반환하는 것이므로 오래된 기대 판을 다시 적용하지 않는다. 새 키·새 쓰기는 현재 판을 검증한다.

[MCP-05 모듈 인수](../reviews/2026-10-20-mcp-05.md)에서 위 실행 경계를 구현했다. Store.ExecuteWrite가 문자열 키·SHA-256·DB 시계의 수명을 관리하고 기존 ReplayIdempotent는 UUID 호출을 같은 경계로 연결한다. 실제 삽입 여부로 최초 예약과 완료 재생을 구분하고 비어 있거나 잘못된 결과·표준 외피·명백한 자격 필드는 커밋하지 않는다. 최초와 재생은 같은 jsonb 표현을 반환한다. 오류는 원인을 보존하고 저장 장애의 원문은 노출하지 않는다. 키가 없어도 완료 객체·오류/취소를 검사하며, 업무가 이미 커밋한 뒤의 불확정 결과에는 재조회가 필요하다. 여덟 도구의 현재 권한 조회·도메인 변환은 [MCP-06](../reviews/2026-10-20-mcp-06.md)에서 인수했다.

동시 재생은 첫 커밋 뒤 같은 결과를 읽고 적용 기록·색인 작업·한도 카운터를 다시 늘리지 않는다. 최초 접수부터 24시간이 지나면 키 재사용을 새 요청으로 처리할 수 있으므로 그 이후 안전한 자동 재시도를 보장하지 않는다. 재생 시 수명을 연장하지 않는다. DB 연결 예약·잠금 순서는 [아키텍처](architecture.md#데이터베이스-연결)와 [수명주기](context-lifecycle.md#판정의-직렬화)를 따른다.
