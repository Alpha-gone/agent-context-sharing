# 출시 판정 추적성 감사

## 문서 정보

- 문서 성격: 특정 시점의 추적성 검사 기록이며 명세가 아니다. 계약은 `SRS.md`와 `SDD.md`가 소유한다.
- 검사일: 2026-09-19
- 검사 대상: `feature/phase-10-release-verification` 브랜치. `dev`의 `3008953`(감사 수정 51건 병합)에 10단계 테스트 두 건을 더한 상태다.
- 검사 범위: 개발 계획 10단계 「출시 완료 기준」 중 평가를 제외한 항목. 요구사항 추적성, MCP 연산과 웹 화면의 계약 일치, 문서와 구현의 일치를 본다.
- 기준 문서: [SRS](../SRS.md), [SDD](../SDD.md), [개발 계획](../DEVELOPMENT_PLAN.md), [용어 정리집](../../DICTIONARY.md)

## 검사 방법

| 대상 | 방법 |
|------|------|
| 요구사항 식별자 | `SRS.md`와 `SDD.md`에서 식별자를 뽑아 전수 대조한다 |
| 설계 반영 상태 | `SDD.md` 「요구사항 추적성」 표의 상태 열을 전수로 센다 |
| 구현 추적성 | 요구사항을 설계 영역 단위로 묶고 영역마다 구현 위치와 검증 테스트를 확인한다 |
| 계약 일치 | MCP 연산 목록과 웹 경로를 명세의 표와 대조한다 |
| 패키지 경계 | `go list`로 실제 import를 뽑아 「패키지 경계」 표와 대조한다 |
| 의도적 미구현 | 0단계가 제외한 항목이 구현에 들어오지 않았는지 확인한다 |

구현 추적성을 요구사항 하나씩이 아니라 영역 단위로 본 이유는 149건 각각에 코드를 대응시키는 작업이 이 검사의 하루 범위를 넘기 때문이다. 영역 단위 확인으로 드러나지 않는 개별 요구사항의 누락은 이 문서가 보증하지 않으며 「남은 확인 한계」에 적는다.

## 요구사항 식별자 전수 대조

| 항목 | 결과 |
|------|------|
| `SRS.md`의 기능 요구사항 | 149건 |
| `SRS.md`의 비기능 요구사항 | 13건 |
| `SDD.md`에 나타나지 않는 기능 요구사항 | 0건 |
| `SDD.md`에 나타나지 않는 비기능 요구사항 | 0건 |
| `SDD.md` 추적성 표에서 상태가 `반영`인 행 | 162행 |
| 상태가 `반영`이 아닌 행 | 0행 |

기능 요구사항 149건은 모두 `SRS.md`에서 상태가 `확정`이다. 요구사항 수준에서 미정으로 남긴 것은 없고, 미정은 `TBD-AGENT_CONTEXT-*`가 따로 소유한다.

## 구현 추적성

설계 영역마다 구현 위치와 그 영역을 검증하는 테스트를 확인했다.

| 영역 | 구현 | 검증 |
|------|------|------|
| 컨텍스트 모델과 계층별 검증 | `internal/model` | `model_test.go`의 필수 속성·조건부 필수·불변성·시간 범위 |
| 그래프와 컨텍스트 저장 | `internal/store`의 `graph.go`, `context.go` | `store_integration_test.go`, `context_concurrency_integration_test.go` |
| 사건 관계 | `internal/store/relation.go` | `relation_concurrency_integration_test.go` |
| 홉 범위 조회 | `internal/store/hop.go` | `hop_integration_test.go`, `store_integration_test.go` |
| 검색 채널과 결합 | `internal/search` | `search_test.go`의 결합·접기·예산 절단 |
| 색인 작업 큐와 임베딩 | `internal/index`, `internal/store/index.go` | `index_integration_test.go`, `index_lease_integration_test.go`, `worker_integration_test.go` |
| 계정·인증·인가 | `internal/authz`, `internal/store/auth.go` | `authz_test.go`, `http_test.go`, `cache_test.go`, `auth_integration_test.go` |
| 권한과 유효 등급 | `internal/perm`, `internal/store/permission.go` | `permission_lifecycle_integration_test.go`, `auth_integration_test.go` |
| 계정 플랜 한도 | `internal/plan`, `internal/store`의 한도 판정 | `plan_test.go`, `limits_integration_test.go` |
| MCP 표면과 연산 13종 | `internal/mcp` | `transport_test.go`, `handler_integration_test.go`, `contract_integration_test.go`, `isolation_integration_test.go` |
| 웹 화면 여섯 종 | `internal/web` | `web_test.go` |
| 주기 작업과 기동·종료 | `internal/store/periodic.go`, `cmd/server` | `server_test.go`의 자문 잠금·종료 순서·TLS·로그 |
| 격리 강제 | `internal/store` 전반 | `isolation_integration_test.go`가 연산 13종을 전수로 확인 |
| 스키마와 마이그레이션 | `migrations/`, `internal/migrate` | `migrate_test.go`, 빈 볼륨 적용과 재실행 확인 |

구현이 없는 영역은 하나다. `FR-AGENT_CONTEXT-112`부터 `FR-AGENT_CONTEXT-116`까지의 검증·검색 품질·그래프 효과·지속 평가·업무 효과 평가는 측정을 가능하게 하는 설계만 구현되어 있고 측정 자체는 수행하지 않았다. 「검색 품질 평가」의 데이터셋이 저장소에 없기 때문이며, 개발 계획 6단계·7단계·10단계의 평가 항목이 같은 이유로 열려 있다.

## 계약 일치

| 대상 | 결과 |
|------|------|
| MCP 연산 13종 | `toolDefinitions()`의 이름 13개가 `SRS.md` 「MCP 연산 매핑」과 같다 |
| 연산별 필요 등급과 멱등성 | `SDD.md` 「연산 계약」 표 13행을 `contract_integration_test.go`가 그대로 확인한다 |
| 연산별 격리 | `isolation_integration_test.go`가 13종 전수를 확인하고 목록이 어긋나면 실패한다 |
| 웹 경로 여섯 종 | `/graphs`, `/graphs/{id}`, `/graphs/{id}/access`, `/graphs/{id}/deletion`, `/graphs/{id}/audit`, `/operator/restores`가 「HTTP 진입점」의 표와 같다 |
| DOX 체인 | `AGENTS.md` 일곱 개의 Child DOX Index가 실제 디렉터리와 일치한다 |

## 발견 사항

### - [x] 1. `audit` 패키지가 없고 그 책임이 `store`에 있다

`SDD.md`의 「패키지 경계」가 11개 패키지 중 하나로 `audit`을 두고 "관리 연산 기록과 웹 감사 기록의 기록·조회·보존 기간 정리"를 맡겼다. 같은 절의 mermaid 다이어그램에도 `Audit` 노드가 있고 `mcp`와 `web`이 그것을 가리킨다.

구현에는 `internal/audit`이 없다. 관리 연산 기록은 `internal/store/operation.go`가, 웹 감사 기록과 보존 기간 정리는 `internal/store`의 `web.go`와 `periodic.go`가 맡고 있다.

개발 계획 「현재 구현 기준선」의 "11개 업무 패키지를 구현한다"가 이 때문에 문자 그대로는 성립하지 않는다. 실제 패키지는 `model`, `store`, `plan`, `perm`, `search`, `index`, `authz`, `mcp`, `web`, `config` 열 개이며 `migrate`는 「패키지 경계」가 애플리케이션 밖으로 둔 도구다.

- 근거: 판독. `go list`와 디렉터리 확인.
- 판단 필요: `audit`을 분리할지, `SDD.md`의 표와 다이어그램에서 빼고 기록 책임을 `store`에 둔 것을 확정할지 정해야 한다.
- 처리: 문서를 실제에 맞췄다. 「패키지 경계」에서 `audit` 행과 다이어그램 노드를 빼고 업무 패키지를 열 개로 확정했으며, 기록 전용 패키지를 두지 않는 근거를 같은 절에 적었다. 기록은 그것을 만든 쓰기와 같은 트랜잭션에서 남아야 하는데 그 트랜잭션은 `store`가 열고, 다른 패키지가 기록하려면 트랜잭션을 넘겨받아야 해서 「접근 계층」이 막은 우회 경로가 된다. SDD의 설계 결정 표와 개발 계획, `internal/migrate`의 주석에 남아 있던 `11개 패키지` 표현도 함께 고쳤다.

### - [x] 2. `store`가 `plan`을 의존한다

「패키지 경계」는 `store`의 의존을 `model` 하나로 정했다. 실제로는 `internal/store`가 `internal/plan`을 import한다. `graph.go:55`의 `plan.CheckIncrease`, `context.go:483`의 `plan.LimitError`, `context.go:741`의 누적 한도 판정이 그 자리다.

이 의존은 감사에서 고친 결함과 얽혀 있다. 누적 한도를 값을 늘리는 트랜잭션 안에서 판정해야 해서 한도 판정이 `store`까지 내려왔다. 의존 방향 자체는 순환을 만들지 않으나 표와 다르다.

- 근거: 판독. `go list -deps=false`.
- 판단 필요: 표를 실제에 맞출지, 판정을 `store` 밖으로 되돌릴 설계를 찾을지 정해야 한다.
- 처리: 표의 `store` 의존에 `plan`을 더하고 근거를 적었다. 누적 한도는 값을 늘리는 쓰기와 같은 트랜잭션에서 판정해야 하며 접근 계층 밖에서 읽은 값으로 판정하면 한도 직전의 동시 요청이 둘 다 통과한다. `plan`은 아무 패키지도 부르지 않으므로 방향은 한쪽으로 남는다.

### - [x] 3. `plan`이 `model`을 의존한다

「패키지 경계」는 `plan`의 의존을 "없음"으로 정했다. 실제로는 `internal/plan`이 `internal/model`을 import한다.

- 근거: 판독. `go list -deps=false`.
- 처리: 표의 `plan` 의존을 `model`로 고쳤다.

### - [x] 4. `search`가 `index`와 `plan`을 의존하지 않는다

「패키지 경계」와 「패키지 의존 순서」는 `search`의 의존을 `store`, `plan`, `index`로 정했고, 특히 `search`가 `index` 아래에 있는 이유를 "의미 유사도 채널이 질의 벡터를 필요로 하므로 `search`가 `index`를 부른다"로 적었다.

실제 `search`는 `model`과 `store`만 import한다. 질의 임베딩은 `Embedder` 인터페이스로 받고 조립 지점인 `cmd/server`가 `index.Worker`를 넣는다. 의존이 역전되어 있으며 결과적으로 `search`는 `index`를 모른다. 설계가 말한 호출 관계는 실행 시점에는 성립하지만 패키지 의존으로는 성립하지 않는다.

- 근거: 판독. `go list -deps=false`와 `internal/search/search.go`의 `Embedder`.
- 참고: 이 형태 덕분에 `search` 단위 테스트가 제공자 없이 돈다. 표를 실제에 맞추는 쪽이 자연스러워 보이나 판단은 필요하다.
- 처리: 표의 `search` 의존을 `model`, `store`로 고치고, `perm`과 함께 필요한 기능을 인터페이스로 받는다는 것을 같은 절에 적었다. 실행 시점의 호출 방향은 전과 같고 임베딩 제공자 호출의 소유도 `index` 하나로 남는다. 개발 계획의 의존 순서 그림도 이에 맞춰 고쳤다.

### - [x] 5. `mcp`가 `index`와 `audit`을 의존하지 않는다

「패키지 경계」의 다이어그램이 `Mcp --> Index`와 `Mcp --> Audit`을 그린다. 실제 `mcp`는 `model`, `perm`, `plan`, `search`, `store`를 import한다. 색인 등록은 `store`의 저장 트랜잭션 안에서 일어나고 기록도 `store`가 남기므로 `mcp`가 두 패키지를 직접 부를 이유가 없다.

- 근거: 판독. `go list -deps=false`.
- 처리: 표와 다이어그램의 `mcp`·`web` 의존을 실제 import로 적었다. 색인 등록과 기록이 모두 `store`의 저장 트랜잭션 안에서 일어나므로 두 패키지를 직접 부를 이유가 없다.

### - [x] 6. 의도적 미구현 다섯 건이 지켜졌다

0단계가 구현 범위에서 제외한 `TBD-AGENT_CONTEXT-048` 인증 고도화, `-049` 계정 소멸 경로, `-052` 운영자 역할 분리, `-060` Client ID Metadata Documents, `-061` 한국어 형태소 분석이 구현에 들어오지 않았다. 계정 삭제 경로, 역할 분리, CIMD 처리, 형태소 분석 구성 어느 것도 코드에 없다.

- 근거: 판독. 해당 기능의 식별자와 표현을 `internal/`과 `cmd/`에서 검색해 부재를 확인했다.

### - [x] 7. 논리 덤프로 복원한 데이터베이스에서 그래프 질의가 성립하지 않는다

개발 데이터베이스를 `pg_dump -Fc`로 받아 빈 데이터베이스에 `pg_restore`로 되돌렸다. 행은 모두 돌아왔다. `ag_graph` 1건, `ag_label` 10건, `Context` 정점 2,426건, `account` 811건이 원본과 같다.

그런데 복원본에서 `MATCH (n:Context) RETURN count(n)`이 `graph with oid 17379 does not exist`로 실패한다. `ag_catalog.ag_graph`가 그래프를 스키마 OID로 기록하는데 그 값이 원본의 OID이고, 복원이 만든 같은 이름의 스키마는 OID가 달라서다. 대조하면 원본은 저장된 값과 실제 OID가 모두 17379이고 복원본은 저장된 값이 17379, 실제 OID가 19003이다.

- 근거: 재현. 개발 데이터베이스에서 덤프와 복원을 수행하고 두 데이터베이스의 `ag_graph`와 `pg_namespace`를 대조했다.
- 처리: `SDD.md`의 「백업과 복구」에 그래프 백업이 물리 백업이어야 하는 이유로 적었다. `pg_dump`는 관계형 데이터의 이관이나 검사용으로만 쓴다.

### - [x] 8. 물리 백업과 시점 복구는 성립한다

7번이 논리 덤프의 한계를 드러냈으므로 물리 백업 경로를 실제로 밟아 확인했다. 개발 구성에 WAL 보관을 켜고 기본 백업을 받은 뒤, 표식 하나를 만들고 그 시각을 기준으로 잡아 표식을 하나 더 만들었다. 기본 백업과 보관한 WAL만으로 두 번째 인스턴스를 기준 시각까지 복구했다.

| 확인 | 결과 |
|------|------|
| 그래프 질의 | `MATCH (n:Context) RETURN count(n)`이 4,335을 돌려준다. 7번과 달리 OID가 보존되어 성립한다 |
| 시점 정확도 | 기준 시각 앞의 표식만 있고 뒤의 표식은 없다 |
| 관계형 데이터 | `account` 1,507건이 함께 복구된다 |

절차는 다음과 같다. `archive_mode`와 보관 경로는 `compose.yaml`에 두었다.

```shell
docker compose exec -u postgres db pg_basebackup -U agent_context -D <기본 백업 경로> -Xs -Fp
# 복구 대상에 restore_command와 recovery_target_time을 두고 recovery.signal을 만든 뒤
docker compose exec -u postgres db pg_ctl -D <기본 백업 경로> -o '-p 5433 -c shared_preload_libraries=age' start
```

복구 인스턴스도 `shared_preload_libraries=age`가 필요하다. 없으면 기동은 되지만 그래프 질의가 확장을 찾지 못한다.

- 근거: 재현. 개발 데이터베이스에서 기본 백업, WAL 보관, 시점 복구를 수행하고 복구본에서 그래프 질의와 표식을 확인했다.
- 처리: `compose.yaml`에 WAL 보관을 켰다. `archive_mode=on`이면 PostgreSQL이 보관본을 지우지 않으므로 보관 기간이 지난 파일을 지우는 `wal_cleanup` 서비스를 함께 두었다. 기본 7일이며 `WAL_ARCHIVE_RETENTION_DAYS`로 바꾼다. 확인에 쓴 기본 백업과 표식은 지웠다.

## 남은 확인 한계

| 한계 | 내용 |
|------|------|
| 요구사항 단위 확인 | 이 검사는 구현 추적성을 설계 영역 14개 단위로 확인했다. 149건을 하나씩 코드에 대응시키는 일은 [요구사항 단위 구현 대조](2026-09-19-requirement-trace.md)가 이어서 수행했고, 영역 단위로는 드러나지 않던 미구현 두 건(`FR-AGENT_CONTEXT-122`, `-123`)을 찾았다 |
| 비기능 요구사항 측정값 | 13건의 설계 근거는 `SDD.md` 「비기능 요구사항 설계」가 갖고 있으나 측정값은 없다. 성능·재현율 계열은 평가 데이터셋이 있어야 값이 나온다 |
| 평가 미수행 | `FR-AGENT_CONTEXT-112`~`-116`의 측정을 수행하지 않았다. 개발 계획 6·7·10단계의 평가 항목이 모두 이 하나에 걸려 있다 |
| 불안정 테스트 해소 | `2026-09-18-source-audit.md`가 열어 둔 색인 작업 테스트의 간헐 실패를 고쳤다. 원인은 셋 모두 같다. 자기 작업만 앞으로 당기고 다른 테스트가 남긴 대기 작업을 그대로 두어, 남은 행이 더 앞선 시각이면 작업자가 그것을 먼저 집었다. 세 자리 모두 나머지를 함께 미루는 방식으로 확보 순서를 고정했고 전체 실행 다섯 번이 연속으로 통과했다 |
| 브라우저 미확인 | 웹 화면은 경로와 서버 응답으로만 확인했고 실제 브라우저로는 보지 않았다 |

## 종합

문서 수준의 추적성은 빈 곳이 없다. 요구사항 149건과 비기능 요구사항 13건이 모두 설계에 나타나고, 설계의 추적성 표 162행이 모두 반영 상태다. MCP 연산 13종과 웹 화면 여섯 종은 명세의 표와 이름·경로가 같고, 이번 단계에서 더한 두 테스트가 연산 13종의 등급·멱등성·격리를 목록과 대조해 지킨다.

어긋난 곳은 한 자리에 몰려 있었다. 「패키지 경계」가 그린 의존 관계와 실제 import가 다섯 군데에서 달랐고 그중 `audit` 패키지는 아예 없었다. 다섯 건 모두 동작의 결함이 아니라 문서와 구현의 불일치였으므로 문서를 실제에 맞추는 쪽으로 정리했다. 각 의존이 왜 그 모양인지를 근거로 함께 적었고, 개발 계획의 의존 순서 그림과 "11개 업무 패키지" 서술도 같이 고쳤다.

같은 종류의 불일치가 다시 생기지 않도록 「설계 경계 검증」의 테스트를 두었다. 표와 실제 import를 양방향으로 대조하므로 표에 없는 의존이 생기는 것뿐 아니라 표에 적힌 의존이 사라지는 것도 실패로 드러난다. 데이터베이스 핸들이 `store` 밖으로 나가지 않는지도 같은 자리에서 본다.

평가는 별개의 덩어리로 남는다. 설계가 측정을 가능하게 해 두었고 구성 값과 응답 지표가 모두 있으나, 데이터셋이 없어 측정을 시작할 수 없다.
