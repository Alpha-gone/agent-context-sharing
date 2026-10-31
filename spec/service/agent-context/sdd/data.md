# 데이터 모델과 저장

관계형 열, AGE 사상, 인덱스, 격리·불변식, 온라인 저장 계층과 마이그레이션의 정본이다.

[설계 전체](../SDD.md) · [요구사항과 설계 대응](traceability.md)

## 물리 배치

하나의 PostgreSQL 인스턴스 안에 두 종류의 저장소가 있다. AGE 확장이 만든 그래프와 일반 관계형 테이블이다.

| 논리 요소                  | 저장 위치                   | 근거                                           |
|----------------------------|-----------------------------|------------------------------------------------|
| 컨텍스트와 계층별 속성     | AGE 그래프의 `Context` 정점 | [SRS 정점 사상](../SRS.md#정점-사상)                                  |
| 참조와 사건 관계           | AGE 그래프의 간선           | [SRS 간선 사상](../SRS.md#간선-사상)                                  |
| 컨텍스트 그래프 메타데이터 | 일반 테이블                 | [SRS 그래프 배치](../SRS.md#그래프-배치)                                |
| 계정                       | 일반 테이블                 | 인증 입력이자 권한과 기여 기록의 참조 대상이다 |
| 권한과 팀                  | 일반 테이블                 | 목록·필터가 SQL이고 그래프 데이터가 아니다     |
| 임베딩                     | 일반 테이블                 | [SRS 임베딩 저장](../SRS.md#임베딩-저장)                                |
| 관리 연산 기록과 감사 기록 | 일반 테이블                 | 아래 설계 결정                                 |
| 쓰기 멱등성 결과           | 일반 테이블                 | 인증 계정과 키 범위의 24시간 재생 저장소       |
| 색인 대기 작업             | 일반 테이블                 | 아래 설계 결정                                 |

AGE 그래프는 하나만 만든다. 이름은 배포 구성으로 두되 인스턴스 안에서 하나로 고정한다. 컨텍스트 그래프의 구분은 전적으로 `graph_id` 속성이 담당한다.

그래프·컨텍스트·계정·관계·팀·감사 기록의 식별자는 UUIDv7을 쓴다. 키 ID·OAuth 코드/토큰 해시·쓰기 요청 키의 타입은 각 저장 계약을 따른다. [`SRS.md`](../SRS.md)의 [SRS 식별 방식](../SRS.md#식별-방식)이 컨텍스트에 대해 확정한 것을 그래프, 계정, 관계, 팀, 기록에도 같게 적용한다. 시간 정렬이 되므로 기록 시점 기준 조회와 커서 페이지 처리에서 별도의 정렬 열이 필요 없고, 값 하나로 두 목적을 다 쓴다.

## 관계형 스키마

| 테이블               | 담는 것                          | 기본 키                                  | 비고                                             |
|----------------------|----------------------------------|------------------------------------------|--------------------------------------------------|
| `account`            | 계정과 자격 증명                 | `account_id`                             | [SRS 계정 속성](../SRS.md#계정-속성)의 4개 열을 그대로 쓴다             |
| `context_graph`      | 컨텍스트 그래프 메타데이터       | `graph_id`                               | [SRS 컨텍스트 그래프 속성](../SRS.md#컨텍스트-그래프-속성)의 10개 열과 내부 열 `grace_expires_at`·`content_revision` |
| `graph_grant`        | 계정·팀에 부여한 등급            | `graph_id`, `subject_type`, `subject_id` | 직접 부여만 담고 상속은 담지 않는다              |
| `team`               | 팀과 팀 관리자                   | `team_id`                                |                                                  |
| `team_member`        | 팀 구성원                        | `team_id`, `account_id`                  |                                                  |
| `context_embedding`  | 컨텍스트 본문의 임베딩           | 파티션별 `context_id` 유일               | [임베딩 스키마](data.md#임베딩-스키마)가 정의한다                     |
| `index_task`         | 색인 대기 작업과 재시도 상태     | `task_id`                                | [색인 작업 큐](indexing.md#색인-작업-큐)가 정의한다                      |
| `operation_log`      | 적용·거부된 컨텍스트 관리 연산   | `operation_id`                           | [SRS 기록 항목](../SRS.md#기록-항목)의 7개 항목을 열로 쓴다             |
| `idempotency_record` | 선택 `request_id`가 있는 쓰기 요청의 지문과 결과   | `actor_account_id`, `idempotency_key`    | 최초 접수부터 24시간 뒤 만료한다                 |
| `web_audit_log`      | 권한·팀·삭제·복구의 웹 감사 기록 | `audit_id`                               | `FR-AGENT_CONTEXT-109`의 여섯 대상을 담는다      |
| `signing_key`        | 토큰 서명 키와 JWKS 공개 자료    | `key_id`                                 | [인스턴스 무상태성](architecture.md#인스턴스-무상태성)                            |
| `request_rate`       | 계정별 쓰기 요청 빈도 카운터     | `account_id`, `window_started_at`        | 한도가 설정된 계정만 행을 만든다                 |
| `revoked_token`      | 폐기 목록                        | `token_id`                               | 만료된 항목은 정리한다                           |

직접 연결의 목표 추가는 `oauth_authorization_code`, `oauth_grant`, `oauth_refresh_token`과 `agent_registration`이다. OAuth 열·결합·보존은 [인증 저장 모델](authentication.md#oauth-저장-모델과-원자적-전이)이 소유하고 등록 열·유일 키·상한은 [공통 행위자](mcp.md#공통-행위자도구쓰기-안전성)가 소유한다. 이 표의 기존 업무 테이블과 목표 추가를 구분하며 기존 DB에 이 테이블이 이미 존재한다고 가정하지 않는다.

각 테이블의 열은 [테이블 열 정의](data.md#테이블-열-정의)가 정한다. 이 표는 테이블의 목적과 기본 키까지만 담는다.

`graph_grant`가 직접 부여한 등급만 담는 이유는 상속이 계산 결과이기 때문이다. [`SRS.md`](../SRS.md)의 [SRS 상속 규칙](../SRS.md#상속-규칙)이 "직접 부여받은 등급과 소속 팀에서 상속한 등급 중 더 높은 등급을 적용한다"고 확정했으므로, 유효 등급은 이 테이블과 `team_member`를 조인해 매번 구한다. 상속 결과를 저장하면 팀 구성이 바뀔 때마다 갱신해야 하고 갱신이 빠지면 권한이 어긋난다. `FR-AGENT_CONTEXT-106`이 요구한 "직접 부여한 등급과 팀에서 상속한 등급을 구분해 표시"도 두 출처가 나뉘어 있어야 성립한다.

`subject_type`으로 계정과 팀을 한 테이블에 두는 이유는 등급 판정이 둘을 함께 읽기 때문이다. 테이블을 나누면 유효 등급을 구할 때마다 두 테이블을 각각 읽고 합쳐야 한다.

관리 연산 기록과 감사 기록을 관계형 테이블에 두는 것은 이 문서의 설계 결정이다. [`SRS.md`](../SRS.md)의 [SRS 감사 범위](../SRS.md#감사-범위)와 [SRS 기록 보존](../SRS.md#기록-보존)이 저장 형식을 상세 설계로 넘겼다. 두 기록 모두 기간 기준 조회와 보존 기간 경과분의 정리가 필요하고 그래프 탐색 대상이 아니므로, [SRS 질의 경계](../SRS.md#질의-경계)가 SQL로 확정한 쪽에 둔다. 그래프에 두면 탐색 질의가 기록까지 지나가게 되어 홉 거리가 어긋난다.

두 기록을 한 테이블로 합치지 않는 이유는 보존 기간이 다르기 때문이다. [`SRS.md`](../SRS.md)가 적용된 연산과 웹 감사 기록은 대상 보관 기간 이상, 거부된 연산은 6개월로 확정했다. 정리 주기가 다른 것을 한 테이블에 두면 정리 질의가 매번 종류를 구분해야 한다. `operation_log`는 적용과 거부를 `result` 열로 구분하되 정리 질의가 그 열을 조건으로 쓴다.

## 테이블 열 정의

[관계형 스키마](data.md#관계형-스키마)의 기존 업무 테이블 중 넷은 다른 소절이 이미 열을 정했다. `account`는 [`SRS.md`](../SRS.md)의 [SRS 계정 속성](../SRS.md#계정-속성)을, `context_graph`는 [SRS 컨텍스트 그래프 속성](../SRS.md#컨텍스트-그래프-속성)에 [소프트 삭제 수명주기](context-lifecycle.md#소프트-삭제-수명주기)의 내부 열 `grace_expires_at`과 요청 단위 일관 읽기 비교용 `content_revision`을 더해, `context_embedding`은 [임베딩 스키마](data.md#임베딩-스키마)를, `index_task`는 [색인 작업 큐](indexing.md#색인-작업-큐)를 그대로 쓴다. 관계형 테이블의 열을 여기에서 정한다.

`content_revision`은 1에서 시작하는 64비트 정수다. 컨텍스트 생성·갱신·폐기·복구, 참조 변경, 사건 관계 확정·폐기, 임베딩 공개와 색인 대상에서 빠진 임베딩 제거 트랜잭션에서 하나씩 증가한다. 그래프 이름·설명·권한·감사 기록처럼 검색 후보·간선·출처를 바꾸지 않는 갱신은 증가시키지 않는다. 이 값은 검색 일관성 비교용 내부 상태이며 낙관적 잠금이나 공개 응답의 `version`을 대신하지 않는다.

식별자 열은 [물리 배치](data.md#물리-배치)가 확정한 대로 UUIDv7을 쓴다. 열거형 값은 [SRS 컨텍스트 모델](../SRS.md#컨텍스트-모델)이 영문 소문자 식별자를 쓴 것과 같게 두어 접근 계층이 같은 방식으로 검증하게 한다.

`graph_grant`는 계정과 팀에 직접 부여한 등급만 담는다.

| 열             | 타입   | 설명                                      |
|----------------|--------|-------------------------------------------|
| `graph_id`     | UUIDv7 | 대상 그래프. 기본 키의 일부               |
| `subject_type` | 열거형 | `account`, `team` 중 하나. 기본 키의 일부 |
| `subject_id`   | UUIDv7 | 대상 계정 또는 팀. 기본 키의 일부         |
| `grade`        | 열거형 | `owner`, `editor`, `viewer` 중 하나       |

`grade`의 세 값은 [SRS 권한 등급](../SRS.md#권한-등급)의 소유자·편집자·열람자에 차례로 대응한다. 부여 시점과 수행 계정을 두지 않는 이유는 [SRS 감사 기록](../SRS.md#감사-기록)이 그 이력을 `web_audit_log`에 남기기 때문이다. 이 테이블은 현재 상태만 갖는다.

`team`과 `team_member`는 권한 그룹을 담는다.

| 테이블        | 열                   | 타입            | 설명                                      |
|---------------|----------------------|-----------------|-------------------------------------------|
| `team`        | `team_id`            | UUIDv7          | 기본 키                                   |
| `team`        | `name`               | 텍스트          | 표시용 이름. 빈 문자열을 허용하지 않는다  |
| `team`        | `manager_account_id` | UUIDv7          | 팀 관리자 계정                            |
| `team`        | `created_at`         | 타임스탬프(UTC) | 생성 시점                                 |
| `team`        | `deleted_at`         | 타임스탬프(UTC) | 소프트 삭제 시점. 값이 없으면 활성 상태다 |
| `team_member` | `team_id`            | UUIDv7          | 기본 키의 일부                            |
| `team_member` | `account_id`         | UUIDv7          | 기본 키의 일부                            |

`deleted_at`을 두는 이유는 [SRS 권한과 팀 관리 절차](../SRS.md#권한과-팀-관리-절차)가 팀 삭제를 소프트 삭제로 확정했기 때문이다. 유효 등급 계산이 이 값을 함께 보므로 삭제된 팀의 `graph_grant` 행은 남아 있어도 등급이 적용되지 않는다. 행을 지우지 않으니 복구는 값을 비우는 것으로 끝난다.

`name`을 두는 이유는 [SRS 권한과 팀 관리 화면](../SRS.md#권한과-팀-관리-화면)이 등급 행마다 대상을 보여주기 때문이다. 대상이 팀일 때 식별자만으로는 어느 팀인지 화면에서 구분할 수 없다. `manager_account_id`는 [SRS 권한과 팀 관리 절차](../SRS.md#권한과-팀-관리-절차)가 "생성한 계정이 그 팀의 팀 관리자가 된다"로 확정한 값이며, 구성원 관리 권한을 판정할 때 읽는다.

`operation_log`는 [SRS 기록 항목](../SRS.md#기록-항목)의 7개 항목을 열로 편다.

| 열                 | 타입            | 대응 항목                                                     |
|--------------------|-----------------|---------------------------------------------------------------|
| `operation_id`     | UUIDv7          | 기본 키                                                       |
| `graph_id`         | UUIDv7          | 보존 기간 판정과 그래프 단위 조회에 쓴다                      |
| `operation_kind`   | 열거형          | 연산 종류. `add`, `update`, `supersede`, `discard`, `keep`    |
| `context_id`       | UUIDv7          | 대상이 컨텍스트일 때의 식별자. 관계 기록에서는 비어 있다      |
| `relation_id`      | UUIDv7          | 대상이 사건 관계일 때의 식별자. 컨텍스트 기록에서는 비어 있다 |
| `target_version`   | 정수            | 대상 컨텍스트의 연산 시점 판 번호. 관계 기록에서는 비어 있다  |
| `judgment_input`   | 텍스트          | 판단 입력                                                     |
| `actor_account_id` | UUIDv7          | 실행 계정                                                     |
| `actor_agent_id`   | UUIDv7          | 실행 에이전트                                                 |
| `applied_at`       | 타임스탬프(UTC) | 적용 시각                                                     |
| `result`           | 열거형          | 적용 결과. `applied`, `rejected`                              |
| `reject_reason`    | 텍스트          | 거부 사유. `result`가 `rejected`일 때만 채운다                |

대상 식별자를 두 열로 나누는 이유는 한 열에 두 종류를 섞으면 기록을 해석할 수 없기 때문이다. [SRS 기록 항목](../SRS.md#기록-항목)이 대상을 컨텍스트 또는 사건 관계로 확정했는데 열이 하나뿐이면 어느 쪽을 가리키는지 판별할 값이 없고, [사후 탐지](context-lifecycle.md#사후-탐지)가 대상 컨텍스트에서 연산 기록으로 거슬러 올라가는 탐색이 관계 기록을 컨텍스트로 잘못 읽는다. 두 열 중 정확히 하나만 채우는 제약을 스키마에 두어 이 규칙을 데이터베이스가 강제한다.

`target_version`을 비울 수 있게 두는 이유도 같은 확정에서 나온다. 관계에는 판 번호가 없으므로 관계 기록에는 남길 값이 없고, 컨텍스트 기록에서는 여전히 필수다. 대상 열과 함께 하나의 제약으로 묶어 컨텍스트 기록이 판 번호 없이 들어가지 않게 한다.

세 열을 모두 비울 수 있는 경우는 하나뿐이다. [SRS 기록 항목](../SRS.md#기록-항목)이 추가와 확정의 거부에서 대상을 비우기로 확정했으므로, `result`가 `rejected`인 기록만 대상 없이 들어갈 수 있다. 적용된 기록은 언제나 대상을 하나 갖는다. 이 규칙도 같은 제약에 넣어 적용 기록이 대상 없이 들어가지 않게 한다.

관계 기록의 `operation_kind`는 `add`와 `discard`만 쓴다. [SRS 기록 항목](../SRS.md#기록-항목)이 관계에 추가와 갱신을 나눌 기준이 없다고 확정했으므로 `update`, `supersede`, `keep`은 컨텍스트 기록에만 나타난다.

`operation_kind`의 다섯 값은 [SRS 연산 종류](../SRS.md#연산-종류)의 추가·갱신·대체·폐기·유지에 차례로 대응한다. `result`를 별도 열로 두는 것은 [관계형 스키마](data.md#관계형-스키마)가 이미 확정했으며, 적용과 거부의 보존 기간이 달라 정리 질의가 이 열을 조건으로 쓴다. `graph_id`를 함께 두는 이유는 보존 기간이 대상 그래프의 계정 플랜을 따르기 때문이다. 정점을 거치지 않고 정리 대상을 고를 수 있어야 한다.

`idempotency_record`는 선택 `request_id`가 있는 쓰기 호출의 전송 재시도 결과만 담으며 관리 연산 기록과 목적이 다르다.

| 열                   | 타입            | 설명                                                     |
|----------------------|-----------------|----------------------------------------------------------|
| `actor_account_id`   | UUIDv7          | 인증 계정. 기본 키의 일부                                |
| `idempotency_key` | 텍스트 | 선택 `request_id`의 16~128자 ASCII 값. 기본 키의 일부 |
| `tool_name`          | 텍스트          | 8종 쓰기 도구 중 하나                                    |
| `request_fingerprint`| 32바이트        | 도구 이름과 `request_id`를 제외한 검증 인자의 결정적 JSON SHA-256       |
| `tool_result`        | JSONB           | 최초 완료된 `tools` 성공 또는 결정적 도메인 오류 결과. 예약 중에만 비어 있고 결과를 채우기 전에는 커밋하지 않는다 |
| `received_at`        | 타임스탬프(UTC) | 최초 접수 시각                                            |
| `expires_at`         | 타임스탬프(UTC) | `received_at`부터 24시간 뒤이며 재생해도 연장하지 않는다 |

기본 키에 인증 계정을 포함하는 이유는 다른 계정이 우연히 또는 의도적으로 같은 키를 보내도 결과를 조회하지 못하게 하기 위해서다. `tool_name`과 전체 인자 지문을 별도로 저장해 같은 키를 다른 도구나 인자에 재사용하면 `invalid_argument`로 거부한다. `tool_result`는 응답 외피를 붙이기 전의 구조화된 결과를 저장하므로 재생 시점의 `serverInfo`와 protocol 외피는 현재 서버가 다시 구성한다.

관리 연산 기록을 재생 저장소로 겸하지 않는다. `operation_log`에는 `graph_create`와 `graph_update`가 없고 정확한 `tools` 결과도 없으며 적용·거부 기록의 보존 기간은 멱등성의 고정 24시간과 다르다. 두 책임을 합치면 감사 보존 정책이 전송 재시도의 수명에 종속된다.

`web_audit_log`는 대상 여섯 종을 한 테이블에 담는다.

| 열                     | 타입            | 채우는 대상                                                                                      |
|------------------------|-----------------|--------------------------------------------------------------------------------------------------|
| `audit_id`             | UUIDv7          | 전부. 기본 키                                                                                    |
| `target_kind`          | 열거형          | 전부. `grant`, `ownership_transfer`, `team`, `web_delete`, `restore_request`, `operator_restore` |
| `action`               | 열거형          | 전부. `grant`, `revoke`, `transfer`, `add`, `remove`, `delete`, `restore`, `request`             |
| `actor_account_id`     | UUIDv7          | 전부. 수행 계정                                                                                  |
| `occurred_at`          | 타임스탬프(UTC) | 전부. 시각                                                                                       |
| `graph_id`             | UUIDv7          | `grant`, `ownership_transfer`, `web_delete`, `restore_request`, `operator_restore`               |
| `subject_type`         | 열거형          | `grant`, `ownership_transfer`, `operator_restore`                                                |
| `subject_id`           | UUIDv7          | `grant`, `ownership_transfer`, `operator_restore`                                                |
| `before_grade`         | 열거형          | 등급 변경·운영자 복구의 이전 등급. 이전 부여가 없으면 생략                                                         |
| `after_grade`          | 열거형          | 등급 변경·상속 근거·운영자 복구의 이후 등급                                                         |
| `team_id`              | UUIDv7          | `team`, 팀 상속 소유자 근거의 `grant`                                                           |
| `member_account_id`    | UUIDv7          | `team`의 추가·제거 계정, 팀 상속 소유자 근거의 계정                                                                   |
| `target_context_id`    | UUIDv7          | `web_delete`. 대상이 노드일 때                                                                   |
| `requester_account_id` | UUIDv7          | `restore_request`, `operator_restore`. 복구를 요청한 소유자였던 계정                                                |

대상마다 채우는 열이 다르므로 공통 다섯 열을 빼고는 값이 없을 수 있다. 대상별로 테이블을 나누지 않는 이유는 [SRS 감사 기록](../SRS.md#감사-기록)이 두 기록을 화면에서 함께 시각 내림차순으로 보여주라고 확정했기 때문이다. 여섯 테이블로 나누면 화면이 매번 여섯을 합쳐 정렬해야 하고 보존 기간 정리도 여섯 번 돈다.

`restore_request`를 별도 대상으로 두는 이유는 요청과 처리가 다른 시점의 다른 행위이기 때문이다. 요청은 소유자였던 계정이, 처리는 운영자 자격을 가진 계정이 하며 사이에 시간이 있다. 한 행으로 합치면 요청만 있고 처리는 없는 상태를 표현할 수 없어 운영자 화면의 목록을 만들 수 없다. 요청 행은 공통 열 외에 `graph_id`와 `requester_account_id`를 채운다. 팀 상속의 과거 소유자 근거는 [감사 조회](observability.md#감사-기록의-조회)를 따른다.

`operator_restore`에 요청자와 처리자를 모두 남기는 것은 [SRS 감사 기록](../SRS.md#감사-기록)이 확정한 것이다. 처리한 운영자는 `actor_account_id`에, 요청자는 `requester_account_id`에 들어간다. [복구](operations.md#복구)가 요청 기록 없이는 처리 경로가 열리지 않는다고 정했으므로 이 열이 비어 있으면 안 되는 대상이 하나 생긴다.

`signing_key`와 `revoked_token`은 웹 세션 인증 경로가 쓴다.

| 테이블          | 열            | 타입            | 설명                         |
|-----------------|---------------|-----------------|------------------------------|
| `signing_key`   | `key_id`      | 텍스트          | JWT 헤더의 `kid`. 기본 키    |
| `signing_key`   | `algorithm`   | 텍스트          | 서명 알고리즘                |
| `signing_key`   | `public_key`  | 텍스트          | JWKS로 공개하는 공개 키      |
| `signing_key`   | `private_key` | 텍스트          | 서명에 쓰는 개인 키          |
| `signing_key`   | `state`       | 열거형          | `active`, `retired` 중 하나  |
| `signing_key`   | `created_at`  | 타임스탬프(UTC) | 생성 시점                    |
| `revoked_token` | `token_id`    | 텍스트          | 폐기한 토큰의 `jti`. 기본 키 |
| `revoked_token` | `expires_at`  | 타임스탬프(UTC) | 원 토큰의 `exp`              |

`state`를 두는 이유는 [SRS 토큰 형식과 검증](../SRS.md#토큰-형식과-검증)이 키 회전을 요구하기 때문이다. 회전하면 새 키로만 서명하되 옛 키로 서명된 토큰이 만료될 때까지 검증은 계속해야 하므로, 서명에 쓰는 키와 검증에만 쓰는 키를 구분할 값이 필요하다. `state = 'active'`를 대상으로 한 부분 유일 인덱스로 활성 키가 하나만 남게 하고, 인가 서버는 지연 생성 경합에서 이 제약에 걸리면 이미 만들어진 활성 키를 다시 읽는다.

`algorithm`을 값으로 고정하지 않고 열로 두는 이유는 회전 때문이다. [인가 서버](authentication.md#인가-서버)가 ES256을 확정했으므로 지금 만들어지는 행은 모두 그 값을 갖지만, 알고리즘을 바꾸는 회전에서는 새 키와 옛 키의 값이 달라야 하고 검증기는 행에 적힌 값으로 각 토큰을 검증해야 한다. `state`가 서명용 키와 검증 전용 키를 구분하는 것과 같은 이유다.

`private_key`는 현재 웹 세션 발급·회전 경로에서만 사용하며 공개 JWKS 응답에는 포함하지 않는다.

`revoked_token`에 `expires_at`을 두는 이유는 [SRS 토큰 형식과 검증](../SRS.md#토큰-형식과-검증)이 "해당 토큰의 `exp`가 지나면 목록에서 지운다"로 확정했기 때문이다. 만료된 토큰은 이미 만료 검증에서 걸러지므로 목록이 무한히 자라지 않는다.

`request_rate`는 [계정 플랜 값](authorization.md#계정-플랜-값)이 정한 1분 고정 창의 카운터다.

| 열                  | 타입            | 설명                                |
|---------------------|-----------------|-------------------------------------|
| `account_id`        | UUIDv7          | 대상 계정. 기본 키의 일부           |
| `window_started_at` | 타임스탬프(UTC) | 창의 시작 시각. 기본 키의 일부      |
| `count`             | 정수            | 그 창에서 수행한 쓰기 연산의 수     |

두 열을 기본 키로 두면 창마다 계정당 한 행이 생기고, 갱신이 `count`를 올리는 upsert 하나로 끝난다. 창을 별도 열로 두지 않고 시작 시각으로 표현하는 이유는 그 값이 곧 정리 조건이기 때문이다. [주기 작업](operations.md#주기-작업)이 현재 창보다 오래된 행을 이 열로 지운다.

한도를 켠 계정만 행을 갖는다. 기본값이 무제한이므로 기본 배포에서는 이 테이블이 비어 있다.

## 신규 구현의 DDL 대응

[DATA-01](../development-plan/data.md#data-01)의 입력은 위 열 정의와 아래 물리 제약이다. 이 절은 구현할 목표이며 기존 SQL의 존재나 적용 여부를 완료 근거로 삼지 않는다. OAuth의 열은 [인증 저장 열](authentication.md#oauth-저장-열), 등록의 열은 [등록 저장 열](mcp.md#등록-저장-열)이 소유한다. 목표 업무 테이블은 기존 13종과 추가 4종, 총 17종이며 임베딩 자식 파티션과 마이그레이션 이력은 별도로 센다.

### SQL 타입·NULL·키

아래에서 명시한 선택 열만 NULL을 허용하고 나머지는 `NOT NULL`이다. PK는 NULL을 허용하지 않는다. UUIDv7의 SQL 타입은 `uuid`지만 UUID 판·유효성은 모델과 저장소가 검증한다. 시각은 `timestamptz`로 저장하고 UTC로 입출력하며, AGE의 시각 property는 UTC RFC 3339 문자열로 변환한다. `version`·`target_version`·`content_revision`·저장량은 Go `int64`와 대응하는 `bigint`로 두고 1 이상인 판과 0 이상인 누계를 구분한다. 값의 증가·합산은 저장소가 오버플로를 검사한다. 열거형은 PostgreSQL enum 타입 대신 `text`와 닫힌 값 집합의 CHECK로 표현한다.

| 테이블 | 열과 SQL 타입 | NULL 허용 | 키·행 제약과 조회 |
|---|---|---|---|
| `account` | `account_id: uuid`, `login_id/password_hash: text`, `created_at: timestamptz` | 없음 | PK `account_id`, UNIQUE `login_id`. 로그인 형식·비어 있지 않은 해시 CHECK. 해시 생성·원문 비저장은 인증 경계 |
| `context_graph` | `graph_id/created_by: uuid`, `name/description: text`, `created_at/last_activity_at/grace_started_at/grace_expires_at/deleted_at: timestamptz`, `stored_chars/version/content_revision: bigint` | `description`, `grace_started_at`, `grace_expires_at`, `deleted_at` | PK `graph_id`. 이름 비어 있지 않음, `stored_chars >= 0`, 두 판 `>= 1` CHECK. 기본값은 저장량 0·두 판 1. 이름은 UNIQUE가 아니다. 활성 목록은 시각·ID 커서 |
| `graph_grant` | `graph_id/subject_id: uuid`, `subject_type/grade: text` | 없음 | PK `(graph_id, subject_type, subject_id)`. 대상 종류와 등급 CHECK. 유효 등급은 조인으로 계산하며 저장하지 않는다 |
| `team` | `team_id/manager_account_id: uuid`, `name: text`, `created_at/deleted_at: timestamptz` | `deleted_at` | PK `team_id`, 비어 있지 않은 이름 CHECK. 이름 UNIQUE 없음. 관리 계정·구성원·활성 상태는 저장소 검사 |
| `team_member` | `team_id/account_id: uuid` | 없음 | PK `(team_id, account_id)`, 계정별 팀 조회 인덱스 |
| `context_embedding` | `context_id/graph_id: uuid`, `embedding: vector(n) 또는 halfvec(n)`, `model_id/storage_tier: text`, `indexed_at/last_accessed_at: timestamptz` | 없음 | `storage_tier` LIST 파티션과 값 CHECK. 두 자식에서 각각 UNIQUE `context_id`. 그래프·HNSW 조회와 전역 중복 책임은 [임베딩 스키마](#임베딩-스키마) |
| `index_task` | `task_id/context_id/graph_id: uuid`, `attempts: integer`, `state/last_error/correlation_id: text`, `enqueued_at/next_attempt_at: timestamptz` | `last_error`, `correlation_id` | PK `task_id`, UNIQUE `context_id`, 시도 횟수 기본 0·음수 금지, 상태 CHECK. 확보는 상태·다음 시도 시각, 세대는 [큐 계약](indexing.md#색인-작업-큐) |
| `context_revision` | `graph_id/context_id/supersedes_context_id: uuid`, `version/previous_version/supersedes_version: bigint`, `snapshot: jsonb` | `previous_version`, `supersedes_context_id`, `supersedes_version` | 복합 PK·이전/대체 FK·판/쌍/JSON object CHECK. 상세는 [파생 판 이력](#파생-판-이력) |
| `operation_log` | `operation_id/graph_id/context_id/relation_id/actor_account_id/actor_agent_id: uuid`, `operation_kind/judgment_input/result/reject_reason: text`, `target_version: bigint`, `applied_at: timestamptz` | `context_id`, `relation_id`, `target_version`, `reject_reason` | PK `operation_id`. 연산·결과·대상 조합 CHECK는 아래 표. 컨텍스트·관계별 이력과 그래프별 감사·보존 조회 |
| `idempotency_record` | `actor_account_id: uuid`, `idempotency_key/tool_name: text`, `request_fingerprint: bytea`, `tool_result: jsonb`, `received_at/expires_at: timestamptz` | `tool_result`는 트랜잭션 내부 예약 중에만 허용 | PK `(actor_account_id, idempotency_key)`. 키 형식과 32바이트 지문 CHECK. 결과 없는 커밋은 저장소가 금지하며 [멱등성 경계](mcp.md#멱등성-저장과-경합)에서 검증. 만료 인덱스 |
| `web_audit_log` | `audit_id/actor_account_id/graph_id/subject_id/team_id/member_account_id/target_context_id/requester_account_id: uuid`, `target_kind/action/subject_type/before_grade/after_grade: text`, `occurred_at: timestamptz` | 공통 5개 열 `audit_id/target_kind/action/actor_account_id/occurred_at` 이외 | PK `audit_id`, 종류·행위·선택 등급 CHECK와 운영자 복구 요청자 필수 CHECK. 종류별 필수·금지 조합은 저장소 검증. 그래프 없는 팀 감사도 계정·시각으로 조회 |
| `signing_key` | `key_id/algorithm/public_key/private_key/state: text`, `created_at: timestamptz` | 없음 | PK `key_id`, 상태 CHECK, `state = 'active'`의 단일 상수 부분 UNIQUE. 키 형식·알고리즘·공개 응답 비밀 제외는 인증 경계 |
| `request_rate` | `account_id: uuid`, `window_started_at: timestamptz`, `count: bigint` | 없음 | PK `(account_id, window_started_at)`, 기본 0·음수 금지 CHECK. 한도·분 창 정규화는 저장소, 만료 정리는 창 인덱스 |
| `revoked_token` | `token_id: text`, `expires_at: timestamptz` | 없음 | PK `token_id`, `expires_at` 인덱스. 만료 시각은 원 토큰의 `exp`를 보존 |

`idempotency_record.tool_name`은 업무 쓰기 8종의 닫힌 값 집합으로 제한한다. 키는 16~128자 ASCII `[A-Za-z0-9_-]`, 지문 길이는 `octet_length(request_fingerprint) = 32`다. 만료 시각은 최초 접수 +24시간으로 저장소가 계산하며 재생 시 변경하지 않는다. 예약 상태를 다른 연결에 노출하거나 독립 트랜잭션으로 커밋하지 않는다. JSONB 결과의 도메인 구조와 OAuth·표준 외피 제외는 MCP 실행 경계의 책임이다.

### 참조와 검증 책임

기존 업무 13종의 계정·그래프·팀 참조에는 SQL 외래 키를 추가하지 않는다. `graph_grant.subject_id`는 다형 참조이고, 컨텍스트·관계는 AGE에 있으며 거부 감사는 요청이 지정한 부재 대상을 남길 수 있다. 일반 계정·팀 참조도 단일 저장 계층에서 존재와 결합을 검사한다. FK가 없다는 것은 참조 검사를 생략한다는 뜻이 아니며, 소프트 삭제·현재 권한·마지막 소유자 보호는 FK로 보장할 수 없다. 목표 OAuth의 계열 연결 두 곳은 인증 정리 순서를 강제할 수 있으므로 아래와 같이 FK를 둔다. 삭제 cascade로 기록을 조기에 지우지 않는다.

| 참조·조건 | DB 책임 | 모델·저장소·인증 책임 |
|---|---|---|
| `context_graph.created_by`, `team.manager_account_id`, `team_member.account_id`, 감사 실행 계정, 플랜/빈도 계정 | UUID 타입·NULL·해당 키 유일성 | 계정 존재와 검증된 실행 계정 사용. 생성자·관리자·기여자 불변성과 변경 권한 |
| `graph_grant.graph_id/subject_id`, `team_member.team_id` | 복합 PK·대상 종류/등급 CHECK | 대상 존재·활성 상태·현재 직접/상속 등급·소유자 보존, 같은 쓰기의 잠금 |
| AGE 정점/간선·임베딩·큐·기록의 `graph_id` | UUID 또는 property 표현식 인덱스 | 입력·조회·변경·응답 모두 같은 그래프. 실제 끝점 존재·계층·기수성은 불변식 검사 |
| OAuth 코드·갱신 토큰의 `grant_id` | `oauth_grant(grant_id)` FK, `ON DELETE NO ACTION`. 코드에서는 미소비 시 NULL | 생성·소비·계열 결합·폐기와 자식 먼저 정리하는 순서. 계정/client/resource/scope는 인증 입력과 재대조 |
| 등록의 계정/client와 기여·확정 agent 식별자 | 등록 PK·계정/client/키 UNIQUE | 활성·동일 계정/client 검사, 계정당 활성 상한 직렬화. 폐기 후 과거 기여·감사 식별자 보존 |
| 적용/거부 연산의 대상 조합 | 컨텍스트면 관계 NULL·양수 판 필수, 관계면 컨텍스트/판 NULL, 무대상은 거부만 허용. 관계 종류는 add/discard만. 적용이면 사유 NULL CHECK | 거부 시 사유 필수와 공개 안전성, 무대상 거부가 추가/확정에 해당하는지, 지정된 거부 대상·판의 의미 |
| 유일성·행 사이 규칙 | 로그인·원천 자연 키·등록 키·큐·파티션별 임베딩·활성 서명 키의 UNIQUE | 사건 관계 정체성/순환/시간, 파티션 간 임베딩 하나, 동시 소유자/플랜 누적 판정은 잠금과 같은 트랜잭션 |

CHECK는 같은 행의 값과 NULL 조합을 검사한다. 다른 행의 존재·현재 시각에 따른 만료·계정당 상한을 CHECK 안의 조회나 `now()`로 구현하지 않는다. 특히 PostgreSQL CHECK는 결과가 NULL이어도 허용하므로 필수 값에는 별도 `NOT NULL`이나 명시적인 `IS NOT NULL` 조건이 필요하다. [PostgreSQL 18 제약 문서](https://www.postgresql.org/docs/18/ddl-constraints.html)가 이 경계의 근거다.

### 판·유예·저장량의 구분

| 값 | 저장·변경 책임 | 사용하지 않는 곳 |
|---|---|---|
| 그래프 `version` | 그래프 정보 수정의 기대 판과 조건부 증가 | 노드·관계·임베딩 변경의 일관 읽기 비교 |
| 컨텍스트 `version` | 원천은 항상 1. 파생·사건 변경은 모델·수명주기 계약과 기대 판 검사 | 관계 판과 임베딩 이력의 대체 |
| `content_revision` | 검색 후보·참조·관계·임베딩을 바꾼 트랜잭션에서 내부 증가 | 공개 응답·낙관적 잠금. 이름·권한·감사만의 변경은 증가 대상 아님 |
| `grace_started_at/grace_expires_at` | 접근 가능한 계정 소멸 시 같은 트랜잭션에서 시작과 당시 플랜 만료를 기록. 무기한이면 만료 NULL, 유예 취소면 둘 다 NULL | 플랜 변경 후 재계산·별도 삭제 채널 열 추가 |
| `stored_chars` | 활성 컨텍스트 본문 문자 수 합. 추가·갱신·폐기·복구와 같은 트랜잭션에서 증감 | 바이트 수·벡터/파일 크기·삭제된 본문 합산 |

플랜은 구성에서 계정별로 주입하며 새 `plan` 테이블·이름·운영 값을 만들지 않는다. 유예·저장량 갱신과 보존 예외의 상세는 [수명주기](context-lifecycle.md#소프트-삭제-수명주기)·[그래프 메타데이터 갱신](context-lifecycle.md#트랜잭션-경계)을 따른다.

## 스키마 적용

현재 적용 순서는 `001_init.sql` → `009_remove_legacy_connection.sql` → `010_target_schema.sql` → `011_query_statistics.sql` → `012_embedding_configuration.sql` → `013_context_revision.sql`이다. 적용된 SQL·checksum은 유지하고 과거 002~008의 번호를 재사용하지 않는다. 009는 사용하지 않는 연결 전용 테이블을 제거하며, 010은 [목표 DDL](#신규-구현의-ddl-대응)의 OAuth·등록·멱등성·bigint 대응을 추가한다. 011은 조회 인덱스·통계를 보강하고 012는 [벡터 형상 전환](indexing.md#전환복귀-경계)과 선택 상관 ID 열의 준비를 담당한다. 서비스·작업자 정지와 백업 후 명시적으로 적용한다. 기존 행이 새 제약에 맞지 않으면 자동 삭제·수정하지 않고 해당 파일 전체를 롤백한다. 저장 구조의 준비는 인증·등록 연산이나 실제 호스트 지원의 완료를 뜻하지 않는다.

번호를 붙인 마이그레이션 파일과 적용 이력 테이블로 스키마를 만들고 판올림한다. 운영 중인 데이터베이스의 테이블을 손으로 고치지 않는다.

보존해야 할 운영 데이터가 생기기 전에는 사용자 승인으로 개발 중 마이그레이션을 `001_init.sql`에 통합하고 제거할 수 있다. 이 예외와 재생성 절차는 `migrations/AGENTS.md`를 따른다. 001의 체크섬이 바뀌므로 통합 전 파일을 적용한 개발·시험 DB는 재생성해야 하며, 기존 적용 이력을 덮어써서 수용하지 않는다. 보존해야 할 운영 데이터가 생긴 뒤에는 다음 번호 파일만 추가한다.

| 근거                                                          | 확정 위치                |
|---------------------------------------------------------------|--------------------------|
| 비교 실험이 대상 기능만 바꾸고 조건을 함께 기록해야 한다      | [`SRS.md`](../SRS.md)의 [SRS 공통 원칙](../SRS.md#공통-원칙) |
| 인스턴스를 늘려 확장하므로 모든 인스턴스가 같은 스키마를 본다 | [배포 형상](architecture.md#배포-형상)            |
| 시점 복구는 복구한 시점의 스키마 판을 알 수 있어야 한다       | [백업과 복구](operations.md#백업과-복구)          |

손으로 고치면 어떤 스키마에서 잰 값인지 남지 않아 [SRS 검색 품질 평가](../SRS.md#검색-품질-평가)의 비교가 성립하지 않는다.

파일을 그대로 실행하지 않고 최소 실행기를 두는 이유는 셋이다.

| 이유         | 내용                                                                                                                                |
|--------------|-------------------------------------------------------------------------------------------------------------------------------------|
| 함수 호출    | 그래프와 label 생성이 DDL이 아니라 `ag_catalog`의 함수 호출이고, 그래프 이름이 [물리 배치](data.md#물리-배치)의 배포 구성 값이라 파일에 박을 수 없다 |
| 구성 값 치환 | `context_embedding`의 벡터 열 타입·차원과 콜드 파티션 tablespace가 배포 구성 값이라 정적 `CREATE TABLE`을 쓸 수 없다              |
| 연결 준비    | 그래프와 label을 만들려면 마이그레이션 연결도 [데이터베이스 연결](architecture.md#데이터베이스-연결)과 같은 AGE 준비를 거쳐야 한다                                   |

적용 순서가 강제된다.

| 단계 | 대상                                      |
|------|-------------------------------------------|
| 1    | `age`와 `vector` 확장                     |
| 2    | AGE 그래프                                |
| 3    | 정점 label `Context`와 간선 label 일곱 종 |
| 4    | 관계형 테이블                             |
| 5    | [인덱스](data.md#인덱스)의 인덱스                       |

3단계가 4단계보다 앞서는 이유는 [인덱스](data.md#인덱스)가 `Context` 정점과 간선 label에 거는 인덱스 때문이다. AGE의 label 테이블은 label이 만들어질 때 생기므로 인덱스를 걸려면 label이 먼저 있어야 한다. 정점을 넣을 때 자동으로 만들어지기를 기다리지 않고 명시적으로 만든다.

실행기는 [패키지 경계](architecture.md#패키지-경계)에 정한 애플리케이션 패키지 밖에 두고 `store`를 거치지 않는다. `store`가 데이터베이스 핸들을 밖으로 내보내지 않기로 한 계약을 지키기 위해서다. [격리 강제의 구현](data.md#격리-강제의-구현)이 정한 접근 계층은 `graph_id`로 요청 경로를 가르는 장치이고, 스키마를 만드는 일은 그 경로가 아니라 그 경로가 성립하기 위한 선행 조건이다. 실행기는 AGE 준비 세 문장만 같은 방식으로 갖고 나머지는 공유하지 않는다.

관계형 테이블과 인덱스는 스키마를 `public`으로 명시해 만든다. [데이터베이스 연결](architecture.md#데이터베이스-연결)이 정한 `search_path`의 첫 항목이 `ag_catalog`이므로, 명시하지 않으면 AGE의 카탈로그 스키마에 테이블이 만들어진다. 읽기는 `public`이 `search_path`에 남아 있어 이름만으로 찾을 수 있으므로 이 명시는 생성 시점에만 필요하다.

이력 조회·각 DDL 트랜잭션·자문 잠금은 같은 전용 연결을 사용한다. 대기 중인 실행기가 풀의 다른 연결을 점유해 적용을 막지 않게 한다. 잠금 획득 취소나 해제 실패로 소유권이 불명확한 연결은 폐기하고, 취소와 별개인 제한 시간으로 정리한다. 파일은 양수 세 자리 번호이며 중복·적용 파일명/checksum 변경·마지막 적용 판 이전 번호의 신규 추가를 거부한다. 삭제된 파일의 이력은 그대로 보존하되 재적용하지 않는다.

구성 값을 파일에 넣기 전에 검증한다. 그래프 이름·벡터 타입·콜드 tablespace는 식별자나 타입 자리에 들어가 매개변수로 묶을 수 없으므로, 허용된 형식과 값 집합을 통과한 값만 치환한다. 통과하지 못하면 이력 테이블을 포함한 SQL을 실행하지 않는다. 그래프·tablespace 식별자는 PostgreSQL의 63바이트 한도를 넘지 않아야 한다. 적용된 파일 일부가 두 이름을 따옴표 없이 치환하므로 PostgreSQL 18 `pg_get_keywords()`의 예약어(R)와 타입·함수 이름 키워드(T)는 마이그레이션 구성 검증에서 거부한다. 열 이름 키워드와 비예약어는 이 자리에서 따옴표 없이 쓸 수 있어 허용한다. 서버 실행 경로는 그래프 이름을 따옴표로 감싸므로 이 제약을 갖지 않는다.

`migrate status`는 자문 잠금을 기다리지 않고 읽기 전용 트랜잭션에서 이력을 읽는다. 이력 테이블 생성·구성 열 추가·구형 이력의 구성 보충을 하지 않으며 구형 이력은 `up`과 같은 방식으로 메모리에서만 복원해 검증한다. 진행 중인 `up`이 있으면 그 시점의 커밋된 이력을 보여준다.

## 파생 판 이력

`context_revision`은 일반 갱신·시스템 근거 표시·폐기·복구와 명시적 대체의 이전/결과 판을 검색 정점과 분리하여 보존한다. 키는 `(graph_id, context_id, version)`이며 같은 키의 스냅숏을 덮어쓰지 않는다. `snapshot` JSONB는 해당 판의 공통·파생 속성과 근거/대체 참조를 모두 담는다. `previous_version`은 같은 컨텍스트의 이전 판을, `supersedes_context_id`·`supersedes_version`은 명시적 대체 당시의 이전 컨텍스트 판을 가리킨다. 대체 이후 갱신에도 고정된 대체 판을 이어받는다. `previous_version`은 같은 복합 키에 대한 자기 FK이며 대체 쌍도 이력 키에 FK로 연결한다.

이전 판과 결과 판은 정점·큐·감사와 같은 쓰기 트랜잭션에서 저장한다. 생성 시 파생의 판 1을 기록하며 전파로 바뀐 직접 파생도 각각 기록한다. 기존 데이터에는 과거 내용을 역산하지 않고 첫 변경·대체 시 관찰한 현재 판을 연결 없는 기준 스냅숏으로 보존한다. 기존 대체 간선의 과거 대상 판이 알려지지 않으면 그 판 번호를 만들어 채우지 않는다. 이전 본문과 참조는 소프트 삭제·복구 뒤에도 보존하고 현재 저장량·검색 후보·홉 정점 수에는 더하지 않는다. 임의 판 조회는 필수 graph_id와 스냅숏의 ID·판 재검사를 사용하는 저장 접근 계층 내부 경계이며 새 MCP 업무 도구는 추가하지 않는다.

013은 이 관계형 테이블과 자기/대체 FK·판/대체 쌍/JSON object CHECK를 추가한다. 기존 001~012와 적용 이력은 변경하지 않는다. 기존 DB는 서비스·작업자 정지와 백업 후 013을 적용한다. 이전 프로그램으로 복귀해도 테이블을 삭제하지 않으며 그 프로그램이 새로 만든 판의 이력은 보장하지 않는다.

## 그래프 스키마

정점은 `Context` label 하나뿐이다. [SRS 컨텍스트 모델](../SRS.md#컨텍스트-모델)의 공통 속성 9개와 계층별 속성을 정점 property로 둔다.

| property 집합 | 적용 계층 | 내용                                                                                                                       |
|---------------|-----------|----------------------------------------------------------------------------------------------------------------------------|
| 공통          | 전부      | `context_id`, `graph_id`, `layer`, `body`, `recorded_at`, `created_by`, `created_by_agent`, `version`, `deleted_at`        |
| 원천          | `source`  | `source_ref_channel`, `source_ref_locator`, `occurred_at`, `origin_kind`                                                   |
| 파생          | `derived` | `derivation_kind`, `summary_scope`, `evidence_state`, `confidence_state`, `valid_from`, `valid_to`, `evidence_invalidated` |
| 사건          | `event`   | `event_start`, `event_end`                                                                                                 |

`derived_from`, `supersedes`, `member_refs`는 property로 두지 않는다. [SRS 정점 사상](../SRS.md#정점-사상)이 확정한 대로 간선으로만 표현한다.

`source_ref`는 두 property로 나눈다. [SRS 출처 참조](../SRS.md#출처-참조)가 정한 수집 경로와 원본 위치를 `source_ref_channel`과 `source_ref_locator`에 각각 담고 중첩한 agtype map으로 두지 않는다. [데이터베이스 연결](architecture.md#데이터베이스-연결)이 `agtype` 해석을 `store` 한곳에 모으기로 했는데 map을 쓰면 그 안의 값을 꺼내는 단계가 하나 더 생긴다. 경로별 집계가 필요해지면 [인덱스](data.md#인덱스)가 이미 쓰는 property 표현식 인덱스를 걸 수 있다는 점도 같다.

MCP 입력과 응답에서는 `source_ref` 하나로 유지한다. [응답 직렬화](mcp.md#응답-직렬화)가 필드 이름을 [SRS 컨텍스트 모델](../SRS.md#컨텍스트-모델)의 속성명과 같게 하기로 확정했으므로 저장 형태를 밖으로 드러내지 않으며, 두 표현 사이의 변환은 `store`가 맡는다.

AGE의 label은 스키마 검증을 제공하지 않으므로 계층별 필수 속성과 열거형 값 집합은 접근 계층이 강제한다. 정점을 만들거나 고치는 경로가 하나뿐이므로 검증 지점도 하나다. 구체적인 검증 규칙은 [인터페이스와 API 계약](mcp.md#연산-계약)이 연산별로 정한다.

| 간선 label     | 방향                      | property                  |
|----------------|---------------------------|---------------------------|
| `DERIVED_FROM` | 파생 → 근거               | `graph_id`                |
| `SUPERSEDES`   | 새 판 → 이전 판           | `graph_id`                |
| `HAS_MEMBER`   | 사건 → 구성원             | `graph_id`                |
| `PRECEDES`     | 앞선 사건 → 뒤따르는 사건 | [SRS 관계 속성](../SRS.md#관계-속성)의 12개 항목 |
| `CAUSES`       | 원인 사건 → 결과 사건     | [SRS 관계 속성](../SRS.md#관계-속성)의 12개 항목 |
| `PART_OF`      | 부분 사건 → 전체 사건     | [SRS 관계 속성](../SRS.md#관계-속성)의 12개 항목 |
| `RELATES_TO`   | 정규화한 순서의 앞 → 뒤   | [SRS 관계 속성](../SRS.md#관계-속성)의 12개 항목 |

앞의 세 간선이 `graph_id`만 갖는 이유는 참조가 컨텍스트의 속성이기 때문이다. 참조에는 상태도 확정 절차도 없으므로 [SRS 관계 속성](../SRS.md#관계-속성)이 정한 값들이 적용되지 않는다. `graph_id`를 두는 것은 [SRS 간선 사상](../SRS.md#간선-사상)이 확정한 탐색 중 대조 때문이다.

뒤의 네 간선은 사건 관계이고 `relation_type`이 label과 중복된다. 두 값을 모두 두는 이유는 질의 방향이 다르기 때문이다. 탐색은 label로 거르는 편이 빠르고, 관계 목록 조회와 상태 필터는 property를 읽는다. 접근 계층이 간선을 만들 때 둘을 함께 채우므로 어긋날 여지가 없다.

원천의 불변성은 접근 계층이 강제한다. `layer`가 `source`인 정점에 대한 수정 요청은 `deleted_at` 표시를 제외하고 거부하며, `version`은 1에서 바뀌지 않는다. 파생의 `derivation_kind`, `summary_scope`, `derived_from`과 `evidence_state`도 같은 자리에서 막는다. [SRS 불변과 가변 규칙](../SRS.md#불변과-가변-규칙)이 수정 대상을 목록으로 확정했으므로 접근 계층은 그 목록 밖의 속성을 받지 않는다. 데이터베이스 제약으로 걸지 않는 이유는 AGE의 property가 스키마를 갖지 않아 열 단위 제약을 쓸 수 없기 때문이다.

시간 유효성은 파생의 `valid_from`과 `valid_to` property로만 표현한다. [SRS 관계의 버전과 시간 유효성](../SRS.md#관계의-버전과-시간-유효성)이 확정한 대로 간선에는 두지 않으며, 만료 조회는 이 두 property를 비교하는 조건으로 처리한다.

### 논리 속성과 검증 위치

다음 표의 필수·조건부 값은 `model`이 저장 방식과 무관하게 검사하고 `store`가 읽기·쓰기 변환에서도 재검사한다. AGE의 `id/start_id/end_id`는 내부 graphid이며 공개 UUIDv7 식별자와 같지 않다. 다른 계층의 property는 허용하지 않는다. 선택값 없음은 property 부재 또는 agtype null로 읽고, 저장할 때는 생략한다. 필수값을 null·빈 문자열로 대신하지 않는다. 문자열은 UTF-8·U+0000 제외 규칙을 지키며, API의 문자 수 상한은 MCP/웹 경계에서도 검증한다.

| SRS 논리 속성 | 저장 위치·표현 | 검증 책임 |
|---|---|---|
| `context_id` | `Context.context_id`, UUIDv7 문자열 | 모델 형식·불변. 저장소가 전역 식별·중복 생성 방지와 조회 대상을 확인 |
| `graph_id` | `Context.graph_id`, UUIDv7 문자열 | 모델 불변. 저장소가 그래프 존재·접근·반환 범위 확인 |
| `layer` | `Context.layer`, 문자열 | 모델의 세 값, 생성 뒤 불변 |
| `body` | `Context.body`, 문자열 | 모델 비어 있지 않음·텍스트 적합성. API 1~8000자, 저장소가 한도와 저장량 증감을 같은 쓰기에서 적용 |
| `recorded_at` | `Context.recorded_at`, UTC 문자열 | 모델 필수·불변. 시스템 시각과 원천 발생 시각 구분 |
| `created_by` | `Context.created_by`, UUIDv7 문자열 | 모델 필수·불변. 저장소가 인증 계정과 결합 |
| `created_by_agent` | `Context.created_by_agent`, UUIDv7 문자열 | 모델 필수·불변. 저장소가 활성 등록·계정/client 결합 확인 |
| `version` | `Context.version`, agtype 정수 | 모델 양수·원천 1 고정. 저장소 기대 판 비교와 변경 원자성 |
| `deleted_at` | `Context.deleted_at`, 선택 UTC 문자열 | 모델 UTC. 저장소가 현재 폐기 채널을 감사와 대조 |
| `source_ref` | 원천의 `source_ref_channel`·`source_ref_locator`, 문자열 2개 | 모델 channel 닫힌 집합·URI scheme, API locator 1~1024자. 저장소 `(graph_id, locator)` 자연 키 유일성과 기존 원천 반환 |
| `occurred_at` | 원천의 `occurred_at`, UTC 문자열 | 모델 필수·불변. 사건 구성원 검증에서는 저장된 실제 발생 시각 사용 |
| `origin_kind` | 원천의 `origin_kind`, 문자열 | 모델 닫힌 집합·불변 |
| `derived_from` | 파생에서 근거로 향하는 `DERIVED_FROM` 간선 집합 | 모델 1~100·중복 금지. 저장소가 같은 그래프·원천/파생·존재·활성·불변 참조 확인 |
| `derivation_kind` | 파생의 `derivation_kind`, 문자열 | 모델 닫힌 집합·불변 |
| `summary_scope` | 파생의 `summary_scope`, 조건부 문자열 | 모델 summary에서만 local/global 필수·불변 |
| `evidence_state` | 파생의 `evidence_state`, 문자열 | 모델 닫힌 집합·불변 |
| `confidence_state` | 파생의 `confidence_state`, 조건부 문자열 | 모델 opinion에서만 필수. 갱신은 현재 권한·판 검사 |
| `valid_from` | 파생의 `valid_from`, 선택 UTC 문자열 | 모델 UTC·유효 범위. 저장소는 시점 필터와 현재 판 적용 |
| `valid_to` | 파생의 `valid_to`, 선택 UTC 문자열 | 모델 UTC·시작과의 순서. 대체가 이 값을 자동 변경하지 않음 |
| `supersedes` | 새 파생에서 이전 파생으로 향하는 `SUPERSEDES`, 0~1개 | 모델 선택 식별자. 저장소가 같은 그래프·파생·불변성과 실제 대상 확인 |
| `evidence_invalidated` | 파생의 `evidence_invalidated`, 불리언 | 모델 필수·기본 false. 저장소의 근거 폐기/대체 판정이며 자동 폐기 신호가 아님 |
| `member_refs` | 사건에서 구성원으로 향하는 `HAS_MEMBER`, 1~1000개 | 모델 중복·기수성. 저장소가 원천/파생·같은 그래프·활성·시간 범위 확인. 사건을 구성원으로 두지 않음 |
| `event_start` | 사건의 `event_start`, UTC 문자열 | 모델 필수·범위. 저장소가 실제 원천 구성원 시각과 대조 |
| `event_end` | 사건의 `event_end`, 선택 UTC 문자열 | 모델 시작과의 순서·미지정이면 진행 중. 구성원과 관계 시간 제약은 저장소 |

세 참조 간선은 위 `graph_id`만 property로 가지며 참조 목록·별도 관계 테이블·관계 이력을 중복 저장하지 않는다. 관계 간선의 12개 논리 속성은 다음과 같이 모두 property로 보존한다. `from_context_id/to_context_id`와 실제 `start_id/end_id`의 대조를 생략하지 않는다.

| SRS 관계 속성 | AGE 표현 | 검증 책임 |
|---|---|---|
| `relation_id`, `graph_id` | UUIDv7 문자열 | 모델 필수·불변 식별. 저장소 graph·실제 끝점 대조 |
| `relation_type` | 문자열과 해당 간선 label | 모델 네 값. 저장소가 label 일치 확인 |
| `from_context_id`, `to_context_id` | UUIDv7 문자열과 AGE의 시작/끝 정점 | 모델 서로 다른 ID·정규 방향. 저장소가 같은 그래프의 사건 존재와 실제 방향 확인 |
| `state`, `proposed_by` | 문자열 | 모델 상태/제안 출처 값 집합. 저장소 상태 전이와 탐색 시 confirmed 필터 |
| `proposed_at` | UTC 문자열 | 모델 필수·불변 |
| `confirmed_by`, `confirmed_by_agent`, `confirmed_at` | 조건부 UUIDv7·UTC 문자열 | 모델 confirmed에서 필수. 저장소가 검증 계정·등록 결합과 전이 시각을 적용 |
| `deleted_at` | 선택 UTC 문자열 | 모델 discarded 상태와 일치. 저장소 폐기·재확정 경계 |

`RELATES_TO`는 두 사건 UUID의 정규 문자열 오름차순으로 끝점을 정하고, 다른 세 관계는 의미상의 방향을 보존한다. 같은 정체성의 재확정은 현재 관계를 재사용한다. 관계에는 `version`·`valid_from`·`valid_to`를 추가하지 않는다. 정체성·순환·시간·상태의 실제 판정은 [그래프 불변식](#그래프-불변식-규칙과-감사)과 [관계 확정](context-lifecycle.md#관계-후보-제안과-확정)이 소유한다.

## 임베딩 스키마

| 열           | 타입            | 설명                                |
|--------------|-----------------|-------------------------------------|
| `context_id`       | UUIDv7          | 대상 컨텍스트                       |
| `graph_id`         | UUIDv7          | 그래프 단위 필터와 재색인에 쓴다    |
| `embedding`        | 벡터            | 저장 타입은 배포 구성이 정한다      |
| `model_id`         | 텍스트          | 생성에 쓴 모델과 압축 방식의 식별자 |
| `indexed_at`       | 타임스탬프(UTC) | 생성 시점                           |
| `last_accessed_at` | 타임스탬프(UTC) | 검색·조회에 마지막으로 쓰인 시점    |
| `storage_tier`     | 열거형          | 파티션 키. `hot` 또는 `cold`         |

컨텍스트 하나에 임베딩 하나를 둔다. PostgreSQL은 파티션 키를 포함하지 않은 부모 테이블의 유일 제약을 허용하지 않으므로 `context_id`는 각 파티션에서 유일하게 유지하고, 단일 색인 저장 경로가 이전 계층의 행을 지운 뒤 `hot`에 넣으며 계층 이동은 파티션 키 갱신으로 수행한다. [SRS 임베딩](../SRS.md#임베딩)이 `body`가 갱신되면 다시 생성한다고 확정했으므로 판별 이력이 아니라 현재 값만 필요하다. 판마다 벡터를 쌓으면 검색이 옛 판을 함께 잡는다.

`model_id`를 함께 두는 이유는 재색인 판정 때문이다. [SRS 임베딩 모델](../SRS.md#임베딩-모델)이 모델이나 압축을 바꾸면 그래프 전체를 재색인하고 그때까지 이전 벡터를 쓰지 않는다고 확정했다. 행마다 어느 모델로 만들어졌는지 알아야 재색인 진행 중에 옛 벡터를 걸러낼 수 있고, [SRS 임베딩 모델](../SRS.md#임베딩-모델)이 요구한 재색인 중 관측 지표도 이 열로 센다.

저장 타입을 고정하지 않는 이유는 [SRS 임베딩 모델](../SRS.md#임베딩-모델)이 저장 정밀도와 차원 축소를 배포 구성 값으로 확정했기 때문이다. `SUG-AGENT_CONTEXT-9`가 채택한 대로 pgvector의 `vector`, `halfvec`, `bit` 중에서 고르며 차원당 각각 4바이트, 2바이트, 8분의 1바이트를 쓴다. 이진 양자화를 고르면 원본 벡터로 재순위화해 재현율을 회복한다.

`bit`는 아직 받지 않는다. 재현율을 회복하는 재순위화 경로가 없고, [인덱스](data.md#인덱스)가 확정한 코사인 연산자 클래스가 pgvector의 bit에 없어 001이 만드는 벡터 인덱스가 성립하지 않는다. 배포 구성 검증과 마이그레이션 실행기가 같은 이유로 각각 거부하며, 실행기는 배포 구성 검증을 지나지 않고 환경 변수를 직접 읽으므로 두 곳에 둔다. 재순위화 경로가 생기면 함께 연다.

## 인덱스

홉 후보 조회는 활성 `Context`의 SQL 속성 `(graph_id, context_id COLLATE "C")` 복합 부분 인덱스와 일곱 간선 label의 `(start_id, end_id)` 복합 인덱스를 사용한다. 관계 데이터나 파생 참조를 별도 저장하지 않는다. 전자는 정렬된 후보 접근, 후자는 양 끝점 조건의 관계 존재 확인을 지원한다. 개발 단계의 사용자 승인으로 `001_init.sql`에 통합하고 같은 파일에서 대상 통계를 갱신한다. 통합 전 001을 적용한 개발·시험 DB는 [스키마 적용](data.md#스키마-적용)의 초기 마이그레이션 통합 절차에 따라 재생성한다. 인덱스 생성은 기존 마이그레이션 트랜잭션으로 실행하므로 큰 운영 테이블에 적용할 때 쓰기 잠금·빌드 공간·적용 시간을 별도로 점검한다. 운영에 자동 적용한 것으로 간주하지 않는다.

| 대상                               | 인덱스                                               | 쓰임                                       |
|------------------------------------|------------------------------------------------------|--------------------------------------------|
| `account`                          | `login_id` 유일 인덱스                               | 로그인 조회와 중복 등록 거부               |
| `context_graph`                    | `deleted_at`이 없는 행의 `last_activity_at` 내림차순 | 그래프 목록의 기본 정렬과 커서 페이지 처리 |
| `graph_grant`                      | `subject_id`                                         | 계정이 접근 가능한 그래프 목록             |
| `team_member`                      | `account_id`                                         | 유효 등급 계산                             |
| `context_embedding`                | `graph_id`와 양쪽 파티션의 HNSW 벡터 인덱스                             | 의미 유사도 채널                           |
| `Context` 정점의 `body`            | 전문 검색 인덱스                                     | 키워드 채널                                |
| `Context` 정점의 `graph_id`        | `properties ->> 'graph_id'` 표현식 인덱스            | 격리 필터와 탐색 시작점 탐색               |
| `Context` 정점의 `context_id`      | openCypher 속성 접근과 같은 `agtype` 표현식 인덱스   | 그래프 불변식 쓰기 검사의 영향 주변부 조회 |
| 활성 `Context` 정점의 `graph_id`, `context_id` | SQL 속성 표현식의 복합 부분 인덱스, `context_id`는 `C` 정렬 | 작은 홉 상한의 정렬된 후보 선택 |
| 일곱 참조·관계 edge label | `(start_id, end_id)` 복합 인덱스 | 후보 관계 존재와 선택 노드 간선 조회 |
| `PRECEDES`·`CAUSES`·`PART_OF` 간선의 `graph_id` | openCypher 속성 접근과 같은 `agtype` 표현식 인덱스 | 그래프 불변식 쓰기 검사의 순환 판정 |
| `Context` 정점의 원천 `source_ref` | `properties ->> 'graph_id'`와 `properties ->> 'source_ref_locator'`, `layer=source` 부분 유일 인덱스 | 생성 전 SQL 중복 조회·23505 재조회·동시 중복 거부                             |
| `index_task`                       | `state`와 `next_attempt_at`                          | 색인 작업자의 대기 작업 조회               |
| `index_task`                       | `context_id` 유일 인덱스                             | 컨텍스트당 한 작업과 등록 upsert           |
| `operation_log`                    | `context_id`, `applied_at`                           | 복구 판정과 보존 기간 정리                 |
| `operation_log`                    | `graph_id`, `applied_at`                             | 감사 기록 화면과 그래프별 보존 정리        |
| `idempotency_record`               | `expires_at`                                         | 만료된 쓰기 재생 결과의 주기 정리           |
| `web_audit_log`                    | `graph_id`, `occurred_at` 내림차순                   | 감사 기록 화면, 대기 복구 요청과 보존 정리 |
| `web_audit_log`                    | `graph_id`가 없는 행의 `actor_account_id`, `occurred_at` | 팀 감사 기록 정리                |
| `request_rate`                     | `window_started_at`                                  | 지난 창의 주기 정리                        |
| `signing_key`                      | `state = 'active'`의 부분 유일 인덱스                | 서명용 활성 키를 하나로 제한               |

신규 구현은 위 인덱스에 더해 `operation_log(relation_id, applied_at)`의 관계 대상 부분 인덱스, `revoked_token(expires_at)` 정리 인덱스를 포함한다. OAuth·등록의 PK·UNIQUE·목록·정리 인덱스는 각각 [인증 저장 열](authentication.md#oauth-저장-열)·[등록 저장 열](mcp.md#등록-저장-열)을 따른다. PK·UNIQUE가 생성하는 인덱스를 같은 열의 별도 일반 인덱스로 중복 생성하지 않는다. 필수 인덱스의 생성·존재 확인과 실제 실행 계획 평가는 각각 DATA-02·DATA-05에서 수행한다.

원천의 인덱스를 부분 인덱스로 두는 이유는 `source_ref`가 원천에만 있기 때문이다. `layer`가 `source`인 행만 대상으로 하면 파생과 사건은 값이 없어도 제약에 걸리지 않는다. AGE의 label 테이블이 일반 PostgreSQL 테이블이라 property 표현식에 유일 인덱스를 걸 수 있으며, `graph_id`와 `body`에 이미 표현식 인덱스를 걸고 있는 것과 같은 방식이다.

`account`의 인덱스를 유일 제약으로 두는 이유는 [`SRS.md`](../SRS.md)의 [SRS 로그인 아이디의 유일성](../SRS.md#로그인-아이디의-유일성)이 유일성을 기능의 전제로 확정했기 때문이다. 접근 계층의 검사만으로는 같은 아이디를 동시에 등록하는 두 요청을 막지 못한다. 데이터베이스가 거절하면 그 실패를 [SRS 예외와 오류 처리](../SRS.md#예외와-오류-처리)가 정한 `invalid_argument`로 사상한다.

벡터 인덱스는 HNSW로 고정한다. `hot`·`cold` 두 파티션에 동일한 모델·차원·거리 연산에 맞는 HNSW를 두며 부모 테이블을 통해 두 계층을 검색한다. 인덱스 종류를 배포 선택으로 열지 않는다. 재현율과 지연은 [SRS 검색 품질 평가](../SRS.md#검색-품질-평가)로 검증한다.

`Context` 정점의 인덱스는 AGE가 만든 label 테이블에 건다. AGE의 label이 일반 PostgreSQL 테이블이므로 property를 꺼내는 표현식에 인덱스를 걸 수 있다. 다만 가변 길이 탐색이 이 인덱스를 쓰는지는 확인이 필요하며, [격리 강제의 구현](data.md#격리-강제의-구현)이 두는 응답 재검사는 인덱스 사용 여부와 무관하게 동작한다.

원천 중복 판정은 `Context` label 테이블에서 `properties ->> 'graph_id'::text = $1`·`properties ->> 'source_ref_locator'::text = $2`와 `properties ->> 'layer'::text = 'source'`의 SQL 조건을 사용한다. 기존 `context_source_ref_idx`와 같은 표현식 및 부분 조건을 유지하여 생성 트랜잭션의 선조회와 23505 뒤 새 트랜잭션의 재조회에 함께 사용한다. `deleted_at`으로 거르지 않으므로 폐기된 원천도 같은 locator의 기존 원천으로 반환한다. SQL로 읽은 property는 기존 Context 해석·그래프/속성 검증을 거쳐 반환하며 조회를 위해 새 인덱스나 마이그레이션을 추가하지 않는다.

`context_id`와 간선 `graph_id` 인덱스는 SQL의 `->>`가 아니라 openCypher 속성 접근이 번역되는 `agtype_access_operator(VARIADIC ARRAY[properties, '"<키>"'::agtype])` 표현식에 건다. 쓰기 검사는 openCypher로 읽으므로 `->>` 표현식 인덱스는 쓰이지 않는다. 인덱스가 없으면 쓰기 한 번마다 모든 그래프의 간선과 `Context` 정점 전체를 해시 조인해, 비용이 쓰기가 바꾼 범위가 아니라 데이터베이스 전체 크기에 비례한다. 2026-09-28 개발 데이터베이스의 정점 약 2만 개에서 이 조회는 21ms였고, 인덱스를 건 뒤에는 0.1ms 아래였다. 표현식 인덱스는 `ANALYZE` 전까지 표현식 통계가 없어 플래너가 선택도를 과대 추정하고 간선 label 전체를 해시 조인하므로, 인덱스를 만드는 마이그레이션이 같은 적용에서 통계를 모은다. 기존 세 순환 검사 label의 단일 `graph_id` 인덱스는 보존한다. 사건 관계의 식별자·정체성·목록·정리는 아래 복합 인덱스를 함께 사용한다.

사건 관계 질의는 `PRECEDES`·`CAUSES`·`PART_OF`·`RELATES_TO` label을 명시한다. 유형 없는 식별자 조회·목록은 이 네 label만 읽고 목록의 식별자 내림차순 페이지를 합친다. 각 label에 openCypher와 같은 agtype 표현식의 `(graph_id, relation_id)`, `(graph_id, from_context_id, to_context_id)`, `(graph_id, to_context_id)` 인덱스를 둔다. 정체성·단일 관계·커서·양방향 사건 정리 조회가 다른 그래프의 간선을 훑지 않도록 하며 기존 끝점 재검사는 유지한다.

정점 잠금·폐기/복구 조회에는 활성 조건이 없는 text `(graph_id, context_id COLLATE "C")` 인덱스를 추가한다. 활성 존재 확인은 UUID 캐스트를 정점 열에 적용하지 않고 기존 활성 부분 인덱스와 같은 text/C 표현식으로 비교한다. 사건 제안 입력은 활성 사건 부분 인덱스 `(graph_id, context_id COLLATE "C" DESC)`로 대상 사건을 제외한 최신 식별자 1,000개까지만 고른 뒤 본문·참조를 조립한다. 이는 제안 생성 수 상한과 별개인 입력 적재 상한이며 상한 밖 사건까지 비교했다고 취급하지 않는다. 인덱스는 새 마이그레이션에서 추가하고 같은 적용에 `ANALYZE`한다.

## 격리 강제의 구현

[SRS 격리 강제](../SRS.md#격리-강제)가 확정한 세 장치를 구조로 옮긴다.

| 장치             | 구현 위치                                                                                    |
|------------------|----------------------------------------------------------------------------------------------|
| 단일 접근 계층   | 그래프 데이터에 닿는 모든 질의를 저장소 패키지 하나로 모으고 `graph_id`를 필수 인자로 받는다 |
| 응답 재검사      | 저장소 패키지가 결과를 반환하기 전에 모든 정점과 간선의 `graph_id`를 요청 값과 대조한다      |
| 격리 위반 테스트 | 서로 다른 그래프의 데이터를 넣고 모든 연산이 상대 그래프의 것을 반환하지 않는지 확인한다     |

접근 계층을 우회할 수 없게 하는 것은 Go의 패키지 경계로 강제한다. 그래프에 닿는 질의 함수를 한 패키지에 두고 그 패키지 밖으로는 `graph_id`를 받는 함수만 내보낸다. 데이터베이스 연결 자체를 다른 패키지에 노출하지 않으므로 우회 경로가 생기려면 패키지를 고쳐야 하고, 그러면 리뷰에 걸린다. 구체적인 패키지 이름과 경계는 [아키텍처](architecture.md)가 정한다.

응답 재검사가 탐색 결과에만 필요한 것이 아니다. 관계 목록과 홉 조회처럼 간선을 반환하는 연산도 대상이며, 간선에 `graph_id`를 둔 것이 여기에 쓰인다.

Row Level Security는 채택하지 않은 채로 둔다. [`SRS.md`](../SRS.md)가 읽기 경로 검증 후 판단하기로 미확정으로 남겼고 이 문서도 그 판단을 바꿀 근거를 갖고 있지 않다. 채택하면 위 세 장치를 대체하는 것이 아니라 네 번째 층으로 더해진다.

## 그래프 불변식 규칙과 감사

`store`는 쓰기 검증과 전체 감사가 함께 호출하는 규칙 목록을 소유한다. 각 규칙은 안정적인 식별자, 적용 대상, 영향 주변부 검사와 전체 검사 함수를 가지며 SQL과 openCypher 중 대상에 맞는 질의를 사용한다.

| 규칙 식별자 | 검사 |
|-------------|------|
| `graph_membership` | 정점·간선·임베딩의 `graph_id`가 대상 그래프 및 연결 대상과 같다. |
| `endpoint_layer` | `DERIVED_FROM`·`SUPERSEDES`·`HAS_MEMBER`와 사건 관계의 양 끝 계층이 논리 모델을 따른다. |
| `reference_cardinality` | 파생 근거, 대체 대상과 사건 구성원 수가 확정된 기수성 범위에 있고 같은 끝점을 중복하지 않는다. |
| `relation_identity` | 같은 유형·정규화한 양 끝의 활성 사건 관계가 중복되지 않고 `RELATES_TO`는 정규 방향으로만 저장된다. |
| `relation_acyclic` | `PRECEDES`·`CAUSES`·`PART_OF`가 순환을 만들지 않는다. |
| `relation_time` | 확정된 사건 관계의 시간 순서와 포함 범위가 유형별 제약을 만족한다. 제안·폐기 상태는 검사하지 않는다. |
| `embedding_link` | 임베딩의 `context_id`가 존재하는 같은 그래프 컨텍스트를 가리키고 현재 `model_id`와 차원이 일치한다. |
| `graph_registration` | 정점·간선·임베딩의 `graph_id`가 `context_graph`에 등록되어 있다. 그래프 단위 감사 범위 밖이므로 전체 감사만 검사한다. |

쓰기 연산은 기존 트랜잭션 안에서 새로 만들거나 바꾸는 정점·간선·임베딩과 직접 연결된 정점·간선을 영향 주변부로 넘긴다. 관련 규칙이 위반되면 커밋하지 않고 기존 입력 오류 또는 내부 오류 사상에 따라 실패한다. 규칙 목록이 기존 `model` 검증을 대신해 저장 방식과 무관한 값 검증을 가져가지는 않는다. `model`은 계층·시간·방향의 논리 판정을 유지하고, `store` 규칙은 실제 저장 상태가 그 판정과 일치하는지 검사한다.

전체 감사는 `cmd/audit`가 읽기 전용으로 실행한다. 기본 범위는 모든 활성·삭제 그래프이며 `graph_id`를 주면 하나로 좁힐 수 있다. 기본 범위는 등록 그래프의 감사 뒤에 `context_graph`에 없는 `graph_id`의 정점·간선·임베딩을 `graph_registration`으로 함께 보고하여, 잘못된 복원이나 직접 SQL로 생긴 고아 행을 놓치지 않는다. 출력은 규칙 식별자, `graph_id`, 대상 종류와 위반 정점·간선·행의 식별자만 담은 JSON Lines이며 컨텍스트 `body`와 임베딩 값은 담지 않는다. 위반이 하나라도 있으면 0이 아닌 종료 상태를 반환하지만 데이터를 삭제·갱신하거나 복구 명령을 실행하지 않는다.

`cmd/migrate up` 완료와 시점 복구 뒤 서비스 재개 절차는 전체 감사를 실행하고 위반이 없음을 확인한다. 정기 실행 주기는 첫 측정이 그래프 크기별 소요 시간과 데이터베이스 부하를 확보하기 전에는 두지 않는다.

규칙은 저장 상태를 읽은 스냅숏만 보고 판정한다. 전체 감사는 그래프의 정점·간선·임베딩과 다른 그래프에 속한 간선 끝을 읽고, 쓰기 검사는 쓰기가 만들거나 바꾼 컨텍스트와 그에 닿은 간선·이웃·임베딩을 읽는다. 순환은 영향 주변부 밖의 경로로도 닫히므로 쓰기 검사도 그래프의 확정 순환 대상 간선을 읽어 전체 감사와 같은 방식으로 답한다. 두 경로가 저장 상태를 다르게 읽으면 같은 규칙이 다른 판정을 내므로, 읽는 범위만 다르고 읽는 방식은 같게 둔다.

쓰기 검사는 영향 주변부 정점에서 나가는 간선과 들어오는 간선을 두 질의로 나눠 읽고 간선 식별자로 합친다. 한 질의에서 두 끝의 조건을 `OR`로 묶으면 조건이 두 정점에 걸쳐 조인 뒤에야 판정되므로 [인덱스](data.md#인덱스)의 `context_id` 인덱스를 쓰지 못한다. 나눠 읽으면 각 질의가 범위 정점을 인덱스로 찾고 간선 label 테이블의 `start_id`·`end_id` 인덱스로 이어져, 비용이 데이터베이스 전체가 아니라 영향 주변부 크기에 비례한다.

고아 간선 보충도 같은 스냅숏에서 읽은 범위 정점의 물리 식별자로 먼저 끝점 범위를 좁힌 뒤 누락 정점 조인을 수행한다. 아직 값을 모르는 식별자 집합을 허브의 평균 선택도로 추정하여 주변부 밖 간선 전체를 스캔하지 않도록 실제 스캔량을 검증한다. 물리 식별자는 저장 계층 내부에만 두며 외부 컨텍스트 식별자나 감사 출력 형식을 바꾸지 않는다.

2026-09-27 측정에서 전체 감사는 1,600노드 그래프에서 37ms, 개발 데이터베이스의 그래프 620개에서 7.6초였다. 쓰기 검사는 관계가 있는 컨텍스트를 쓸 때 쓰기 한 번에 14~21ms를 더했으며, 이 값은 데이터베이스 전체 크기에 비례했다. 2026-09-28 [인덱스](data.md#인덱스)의 두 인덱스와 방향별 간선 조회를 적용한 뒤에는 같은 표본에서 2.4~3.2ms였다. 쓰기마다 영향 주변부를 검사하므로 정기 감사 주기는 두지 않고 복구·마이그레이션 뒤 감사만 유지한다. 측정 조건과 값은 `reviews/2026-09-27-invariant-audit.md`에 있다.

## 저장 계층화

근거는 `FR-AGENT_CONTEXT-104`·`123`과 [SRS 저장 계층화](../SRS.md#저장-계층화)다. 계층화 대상은 임베딩 테이블뿐이다. AGE 정점·간선·본문은 현재 저장 위치를 유지하고 파티셔닝·이동하지 않는다.

| 대상 | 이동 조건 | 온라인 읽기 |
|---|---|---|
| 접근 차단된 컨텍스트의 임베딩 | 그래프 또는 노드의 접근 차단 | 복구 이후 다시 같은 모델로 검색할 수 있게 보존한다 |
| 활성 컨텍스트의 임베딩 | 플랜의 미사용 기간 초과 | `cold`에서도 사전 복원·승격 없이 직접 검색한다 |

`cold`는 같은 PostgreSQL의 영속 테이블스페이스만 허용한다. 객체 저장소·오프라인 파일·탈착 저장소와 사전 복원 경로는 초기 범위에 두지 않는다. 테이블스페이스 경로·권한·마운트는 운영자가 준비하며 마이그레이션은 기존 이름만 대조한다. 임베딩 이동은 컨텍스트 삭제나 영구 삭제가 아니다.

임베딩 테이블은 `storage_tier`의 `hot`·`cold` 값으로 목록 파티셔닝하고 `graph_id` 인덱스로 그래프 범위를 좁힌다. `indexed_at`과 별도로 `last_accessed_at`을 두고 새 색인은 `hot`에서 시작한다. 하루 주기 계층 이동은 접근이 차단된 그래프·폐기 컨텍스트의 임베딩을 기간과 무관하게 `cold`로 옮기고, 활성 그래프는 생성 계정 플랜의 `TierMoveAfterDays`가 0이 아닐 때 마지막 접근 시각이 기준을 넘긴 행만 옮긴다. 기본값 0은 활성 임베딩을 이동하지 않는다.

검색 후보 조립에 사용되거나 단일 컨텍스트 조회와 홉 조회에서 반환된 활성 컨텍스트는 `last_accessed_at`을 갱신하고 `cold` 행을 같은 문장으로 `hot`에 되읽는다. 삭제된 컨텍스트는 복구 경로가 읽어도 `cold`에 유지한다. 이 갱신은 대상 행을 `context_id` 순서로 잠그고 다른 요청이 잠근 행은 건너뛴다. 동시 요청은 겹치는 행을 서로 다른 순서로 기록하게 되어, 한 문장으로 잠그면 교착과 교착 감지 대기가 연결 풀을 고갈시키기 때문이다. 건너뛴 행은 잠근 요청이 기록하므로 접근 기록으로서 잃는 것이 없다. 부모 테이블을 조회하므로 이동 중에도 두 파티션이 같은 검색 표면을 제공하며, 계층 이동은 행 삭제로 기록하지 않는다.

흐름 응답의 출처 구분을 계산하기 위해 `DERIVED_FROM`·`HAS_MEMBER`를 따라 내부적으로 읽는 조상은 접근 기록 대상에 추가하지 않는다. `ContextOriginKinds`는 같은 읽기 스냅숏의 내부 조회로 출처만 해석하고, 조회 대상 자체를 포함하여 `last_accessed_at`·저장 계층을 바꾸지 않는다. 실제 검색 후보 조립이나 단일·홉 조회로 반환된 컨텍스트의 접근 기록은 해당 조회 경로에서 계속 수행한다. 사건 저장·색인 완료 뒤 시스템이 관계 후보를 제안하기 위해 대상 사건을 읽는 것도 사용자 조회가 아니므로 접근 시각·저장 계층을 바꾸지 않는 내부 조회를 사용한다.

동기화 스냅숏 안의 접근 기록은 [데이터베이스 연결](architecture.md#데이터베이스-연결)의 종료 경계에 따라 읽기 전용 트랜잭션을 닫은 뒤 완료하고 호출 결과를 반환한다. 기록한 접근 시각은 조회 시점의 값이며, 읽기 전용 트랜잭션 안에서 임베딩을 갱신하지 않는다.

같은 임베딩의 동시 파티션 이동은 `SKIP LOCKED`를 사용해도 PostgreSQL `40001`을 낼 수 있다. 접근 기록은 동일한 대상과 조회 시각으로 한 문장을 다시 실행해도 같은 결과를 내므로, 이 오류에만 새 자동 커밋 문장에서 한 번 재시도한다. 두 번째 실패와 다른 오류·취소는 호출자에게 전달한다. 재시도도 앞선 문장의 연결을 반환한 뒤 같은 자리에서 수행하며 추가 연결을 겹쳐 잡지 않는다. 근거는 [`reference.md`](../reference.md)의 PostgreSQL 18 `UPDATE` 문서다.

`EMBEDDING_COLD_TABLESPACE`는 마이그레이션 시 기존 PostgreSQL tablespace 이름을 받아 `cold` 파티션에 적용한다. 비우면 기본 tablespace를 써서 개발 환경에서는 논리 파티션만 만들고, 운영 배포는 값싼 저장 장치에 만든 tablespace를 지정할 수 있다. 식별자 자리에 따옴표 없이 치환되므로 영문 소문자·숫자·밑줄만 허용하고 [스키마 적용](#스키마-적용)의 키워드 제약을 따른다.


콜드 테이블스페이스의 장애는 같은 DB의 검색·복구 장애로 처리한다. 해당 계층을 빈 결과로 숨기거나 컨텍스트를 다시 생성하지 않는다. 백업은 클러스터·두 임베딩 계층·테이블스페이스 연결과 WAL을 함께 보존하고, 복구 뒤 두 계층의 질의·인덱스와 그래프 불변식을 검사한다. 저장비·검색 지연의 효과는 실제 측정 전이며 정점 파티셔닝의 채택은 다시 열지 않는다.

## 직접 연결·진단 저장 변경의 이행

`010_target_schema.sql`에서 OAuth 테이블·키·인덱스와 등록 저장을 추가하고 `idempotency_record.idempotency_key`를 문자열로 확장한다. 기존 UUID 키는 `::text`로 같은 값을 보존하고 계정·키 기본 키와 24시간 만료를 유지한다. 기존 지문·완료 결과·감사 식별자를 재작성하지 않으며 새 지문의 생성과 재생은 [MCP 계약](mcp.md#멱등성-저장과-경합)을 따른다. 기존 만료 전 키를 새 도구·다른 인자 지문으로 자동 재사용하지 않는다.

색인 큐의 선택 `correlation_id`는 진단 메타데이터로 추가한다. 기존 행은 NULL로 보존하고 재등록 시 검증된 요청 상관 ID로 갱신한다. 진단 필드로 작업 유일 키·세대·확보·재시도 의미를 바꾸지 않는다. 필드의 전달·정리·민감정보 제한은 [진단 흐름 추적](observability.md#이벤트-외피와-흐름-추적)을 따른다.

변환 전 서비스·작업자를 정지하고 백업·적용 이력·구성 지문을 확보한다. 적용 후 전체 불변식 감사와 계정/등록/멱등성/OAuth 결합을 대조하고 별도 호스트 시험을 수행한다. 실패 시 롤백 가능한 스키마 트랜잭션은 되돌리고, 이미 적용·사용한 변경의 복귀는 검증한 백업·서버 이미지로 별도 절차를 수행한다. 제거된 전용 연결 경로를 복귀 기본 경로로 다시 만들지 않는다. 운영 DB 적용·전환은 대상·백업·복귀 조건을 확인한 별도 실행으로 수행한다.
