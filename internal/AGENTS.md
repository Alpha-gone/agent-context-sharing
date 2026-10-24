# 내부 구현 영역

## 목적

- `internal/`은 서버 업무 패키지와 실행기 내부 구현을 소유한다.

## 소유권

- 루트 DOX가 모듈·Go 기준·공통 검증을 소유한다.
- 패키지 배치와 허용 import는 [아키텍처](../spec/service/agent-context/sdd/architecture.md#패키지-경계)가, 각 처리의 상세 계약은 아래 정본이 소유한다.

| 대상 | 정본 |
|---|---|
| `model/` | [SRS 논리 모델](../spec/service/agent-context/SRS.md#컨텍스트-모델) |
| `plan/`, `perm/` | [권한·팀·플랜](../spec/service/agent-context/sdd/authorization.md) |
| `config/` | [배포 구성](../spec/service/agent-context/sdd/operations.md#배포-구성), [목표 추가 구성](../spec/service/agent-context/sdd/operations.md#목표-추가-구성) |
| `store/` | [데이터·격리·불변식](../spec/service/agent-context/sdd/data.md), [쓰기·잠금·복구](../spec/service/agent-context/sdd/context-lifecycle.md) |
| `authz/` | [계정·세션·OAuth·키](../spec/service/agent-context/sdd/authentication.md) |
| `index/` | [제공자·큐·세대·재색인](../spec/service/agent-context/sdd/indexing.md) |
| `mcp/` | [전송·도구·주입 계약](../spec/service/agent-context/sdd/mcp.md), [패키지 경계](../spec/service/agent-context/sdd/architecture.md#목표-추가-패키지와-조립) |
| `diagnostic/` | [진단 이벤트·기록기](../spec/service/agent-context/sdd/observability.md#기록기와-소유-경계) |
| `search/` | [채널·스냅숏·홉·흐름](../spec/service/agent-context/sdd/search.md) |
| `web/` | [웹 DOX](web/AGENTS.md) |
| `migrate/` | [SQL 적용·치환·이력 DOX](../migrations/AGENTS.md) |

`mcp`·`diagnostic`의 주입 계약과 목표 구현의 경계는 [목표 추가 패키지](../spec/service/agent-context/sdd/architecture.md#목표-추가-패키지와-조립), 동작은 [MCP](../spec/service/agent-context/sdd/mcp.md)·[관측성](../spec/service/agent-context/sdd/observability.md)을 따른다. `migrate`는 애플리케이션 업무 패키지 밖의 도구다.

`diagnostic/test-linux.sh`는 기록기의 Linux 격리 파일 인수 실행을 소유한다. 시험 바이너리·자신이 만든 컨테이너/볼륨만 사용하며 실행 범위·장애 주입·근거는 [파일 저장 인수](../spec/service/agent-context/sdd/observability.md#파일-저장-인수)를 따른다.

## 로컬 계약

- 저장 방식과 무관한 타입·논리 검증은 `model`에 두고 저장·임베딩·등급 기능은 아키텍처가 정한 주입 인터페이스를 사용한다.
- 그래프 데이터는 단일 `store` 접근 계층을 통과한다. 필수 `graph_id`·응답 재검사·비공개 DB 핸들과 [연결 예약](../spec/service/agent-context/sdd/architecture.md#데이터베이스-연결)을 유지한다.
- 쓰기·한도·감사·색인 등록·멱등성의 원자성과 잠금 순서는 [트랜잭션 경계](../spec/service/agent-context/sdd/context-lifecycle.md#트랜잭션-경계)·[판정의 직렬화](../spec/service/agent-context/sdd/context-lifecycle.md#판정의-직렬화)를 따른다.
- 인증 자원 보호·홉 적재/취소·색인 세대 검증은 각 정본의 실행 경계를 따른다. 홉의 간선 완전성을 확인하지 못한 결과를 정상 절단으로 표시하지 않으며 요청·정리 오류를 해당 오류 계약으로 전달하고 연결·예약·작업 자리를 반환한다.
- 도메인 오류는 원인을 보존한 타입으로 전달하며 HTTP·MCP 사상은 진입점이 담당한다. 업무 감사와 [진단 기록](../spec/service/agent-context/sdd/observability.md#기록기와-소유-경계)을 분리한다.

## 작업 지침

- 대상 패키지의 정본과 영향받는 교차 분야 계약을 읽고 인터페이스·저장 상태·설계를 함께 정합화한다.
- 검증 코드는 [테스트 설계](../spec/service/agent-context/sdd/testing.md)를 따른다. 서명 키는 시험별 임시 DB, 색인 확보는 시험 그래프 범위로 격리하고 공유 상태를 준비용으로 변경하지 않는다.

## 검증

- 단위·연산·격리 시험은 [루트 시험 시점 계약](../AGENTS.md#검증)에 따라 해당 작업 항목의 모듈 시험·분야별 통합 테스트에서 수행한다. 코드 변경 시 루트 Go 정적 검증을 실행한다.
- DB 경합·스냅숏·자원 상한은 실제 연결로 검증한다. 홉 보호는 결과 완전성·실제 적재량·마감·풀/읽기 자리 회복을 직접 대조하며 후속 조회 성공만으로 반환을 판정하지 않는다.
- 패키지 의존은 SDD의 허용표와 실제 import를 양방향으로 대조한다.

## Child DOX Index

- `web/AGENTS.md`: 웹 처리기·템플릿·브라우저 자산의 작업 계약.
