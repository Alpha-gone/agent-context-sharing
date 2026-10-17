# 실행기 영역

## 목적

- `cmd/`는 서버·색인 조립과 마이그레이션·평가·감사 실행기를 소유한다.

## 소유권

- 루트 DOX가 Go 모듈·공통 검증·루트 실행 구성을 소유한다.
- 실행기별 동작은 다음 정본을 따르며, 이 문서는 조립·실행 경계와 검증 선택을 소유한다.

| 대상 | 정본 |
|---|---|
| `server/` | [조립·HTTP](../spec/service/agent-context/sdd/architecture.md), [기동·종료·주기 작업](../spec/service/agent-context/sdd/operations.md) |
| `indexer/` | [색인 배치](../spec/service/agent-context/sdd/architecture.md#배치-조합), [소비·리스](../spec/service/agent-context/sdd/indexing.md#색인-작업-큐), [작업자 정리](../spec/service/agent-context/sdd/operations.md#기동과-종료) |
| `eval/` | [평가 실행·집계·준비·정리](../spec/service/agent-context/sdd/evaluation.md), [평가 시험](../spec/service/agent-context/sdd/testing.md) |
| `audit/` | [읽기 전용 불변식 감사](../spec/service/agent-context/sdd/data.md#그래프-불변식-규칙과-감사) |
| `migrate/` | [마이그레이션 DOX](../migrations/AGENTS.md), [스키마 적용](../spec/service/agent-context/sdd/data.md#스키마-적용) |

## 로컬 계약

- 업무 처리는 `internal/`의 패키지에 두고 실행기는 구성 검증·주입·수신·종료를 조립한다. 패키지 의존과 도구 예외는 [아키텍처](../spec/service/agent-context/sdd/architecture.md#패키지-경계)를 따른다.
- 서버는 작업자를 시작하기 전에 정리 책임을 등록하고 조립·수신 실패와 정상 종료에 같은 정리 경계를 사용한다. 원인 보존·요청/작업자 시간 예산·풀 종료 조건은 [기동과 종료](../spec/service/agent-context/sdd/operations.md#기동과-종료)를 따른다.
- 전송 보안·실제 프록시 상대·방문자 주소 해석은 서버 조립 경계가 소유한다. [공개 로그인·가입 보호](../spec/service/agent-context/sdd/authentication.md#공개-로그인가입-자원-보호)와 웹·MCP 처리기는 검증된 값을 받는다.
- 서버·평가·감사는 DB 연결 전에 [임베딩 제공자 구성](../spec/service/agent-context/sdd/indexing.md#임베딩-제공자-전환)을 검증한다. 읽기 전용 감사는 외부 제공자를 호출하지 않는다.
- 평가의 모드 배타성·색인 준비·필수 지표·독립 표본·정리 실패는 [평가 계약](../spec/service/agent-context/sdd/evaluation.md#검증을-가능하게-하는-설계)을 따른다. 실제 지연·실행 시각을 고정값으로 대체하지 않는다.
- 감사는 규칙·위반 위치를 보고하고 자동 수정하지 않는다. 마이그레이션 실행은 SQL 파일 영역의 적용·치환·이력 계약을 함께 따른다.

## 작업 지침

- 대상 실행기의 정본과 호출하는 내부 패키지의 DOX를 읽고, 새로운 배포 구성은 루트 환경 예시·운영 안내와 함께 유지한다.
- 조립·실행 중 부작용과 정리 결과를 분리하여 기록한다. 실제 DB 적용·외부 API·호스트 인수·운영 배포는 요청된 실행 범위를 따른다.

## 검증

- 실행기의 패키지 시험은 [루트 시험 시점 계약](../AGENTS.md#검증)에 따라 해당 작업 항목의 모듈 시험·분야별 통합 테스트에서 수행한다. 코드 변경 시 루트 공통 Go 정적 검증을 실행한다.
- 서버는 시작·수신·요청·작업자 실패와 연결 반환, 평가는 준비 실패·집계·취소·독립 정리, 감사·마이그레이션은 읽기 전용성·적용 이력·반복 실행을 검증한다.
- 실제 호스트·외부 제공자·배포 인수는 [테스트 설계](../spec/service/agent-context/sdd/testing.md)의 실행 계층과 구분하여 보고한다.

## Child DOX Index

- 현재 하위 `AGENTS.md` 문서는 없다.
