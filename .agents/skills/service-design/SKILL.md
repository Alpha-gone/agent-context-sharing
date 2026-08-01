---
name: service-design
description: Design and maintain service Software Requirements Specification documents in this repository. Use when Codex needs to create, update, research, summarize, or log service specs under spec/service, including the service index README.md and each service folder's SRS.md, reference.md, suggestion.md, and logs/prompt.md.
---

# Service Design

## Overview

서비스별 요구사항 명세서를 `$(project_root)/spec/service/{서비스명칭}/SRS.md` 또는 `$(project_root)/spec/service/{서비스그룹}/{서비스명칭}/SRS.md`에 작성하고 유지한다. `$(project_root)/spec/service/README.md`에는 하위
서비스의 전체 목록과 관계를 요약한다. 각 서비스의 상세 요구사항은 서비스별 `SRS.md`에 표 중심으로 정리하며, 조사 근거와 제안, 요청 이력은 서비스별 보조 문서에 지속적으로 누적한다.

## Project Root

1. 우선 현재 작업 디렉터리의 Git 루트를 `$(project_root)`로 취급한다.
2. 현재 위치가 Git 루트 하위라면 `git rev-parse --show-toplevel` 기준 경로를 사용한다.
3. 서비스 색인 경로는 항상 `$(project_root)/spec/service/README.md` 형태가 되도록 확인한다.
4. 최종 서비스별 경로는 기본적으로 `$(project_root)/spec/service/{서비스명칭}/` 형태가 되도록 확인한다.
5. 공통 기반 서비스처럼 묶음이 필요한 경우 `$(project_root)/spec/service/{서비스그룹}/{서비스명칭}/` 형태를 사용할 수 있다. `core.storage`, `core.auth`, `core.studio`처럼 `core.` 접두어가 붙은 서비스는 각각 `spec/service/core/storage/`, `spec/service/core/auth/`, `spec/service/core/studio/` 경로를 사용한다.

## Required Workflow

1. 적용되는 `AGENTS.md` 체인을 읽고 현재 작업 계약을 확인한다.
2. `$(project_root)/spec/DICTIONARY.md`를 확인한다. 새 프로젝트 용어가 필요하면 `dictionary` 스킬을 사용해 용어집을 갱신한다.
3. 사용자의 서비스명 또는 서비스 범위를 식별한다. 불명확하면 최소 질문으로 확인한다.
4. 서비스 디렉터리를 만들거나 기존 디렉터리를 읽고, `$(project_root)/spec/service/README.md`가 있으면 함께 읽는다.
5. 사용자의 별도 지시가 없다면 추가적으로 인터넷 검색을 수행하고 출처를 검증한다.
6. 서비스별 상세 요구사항 명세를 서비스 폴더의 `SRS.md`에 반영한다.
7. `TBD-*` 행은 상태별로 분리하되 기존 ID를 유지한다. `미확정`, `검토 필요`, `보류`는 `미정 사항`, `확정`은 `확정된 결정`, `폐기`는 `폐기된 항목`에 배치한다.
8. `spec/service/README.md`에 하위 서비스 목록, 각 서비스의 역할, 서비스 간 관계, 주요 진입 문서 링크를 최신 상태로 정리한다.
9. 검색에 사용한 자료는 서비스별 `reference.md`에 취합한다.
10. 검색 중 사용자 요청 범위를 넘지만 설계에 도움이 되는 내용이 있으면 서비스별 `suggestion.md`에 기록하고, 최종 응답에서 추천된 내용이 있음을 알려 확인을 요청한다.
11. 작업마다 서비스별 `logs/prompt.md`에 요청자, 사용자 요청, 결과를 표로 추가한다.
12. 위 작업이 완료된 후 `dictionary` 스킬을 사용하여 용어집의 갱신이 필요한지 판단 후 필요한 경우 갱신한다.
13. 완료 전 DOX pass를 수행하고, 계약 변경이 있으면 가장 가까운 `AGENTS.md`와 영향받은 부모/자식 문서를 갱신한다.

## Service Directory Shape

```text
spec/service/{서비스명칭}/
├── SRS.md
├── reference.md
├── suggestion.md
└── logs/
    └── prompt.md
```

```text
spec/service/{서비스그룹}/{서비스명칭}/
├── SRS.md
├── reference.md
├── suggestion.md
└── logs/
    └── prompt.md
```

```text
spec/service/
├── README.md
├── {서비스명칭}/
│   ├── SRS.md
│   ├── reference.md
│   ├── suggestion.md
│   └── logs/
│       └── prompt.md
├── {서비스그룹}/
│   └── {서비스명칭}/
│       ├── SRS.md
│       ├── reference.md
│       ├── suggestion.md
│       └── logs/
│           └── prompt.md
└── AGENTS.md
```

필요한 파일만 먼저 만들되, 설계 작업을 수행했다면 `spec/service/README.md`, 서비스별 `SRS.md`, 서비스별 `logs/prompt.md`는 반드시 유지한다. 인터넷 검색을 사용했다면
서비스별 `reference.md`를 만들거나 갱신한다. 추가 제안이 있을 때만 서비스별 `suggestion.md`를 만들거나 갱신한다.

`core.storage`, `core.auth`, `core.studio`처럼 `core.` 접두어가 붙은 서비스는 점 표기 디렉터리를 만들지 않고 다음 구조를 사용한다.

```text
spec/service/core/
├── storage/
│   ├── SRS.md
│   └── logs/
│       └── prompt.md
├── auth/
│   ├── SRS.md
│   └── logs/
│       └── prompt.md
└── studio/
    ├── SRS.md
    └── logs/
        └── prompt.md
```

## Service Index README.md Guidance

`spec/service/README.md`에는 하위 서비스 내용을 한눈에 볼 수 있도록 현재 상태 기준으로 정리한다. 다음 내용을 우선 포함한다.

- 서비스 묶음 개요
- 하위 서비스 목록
- 서비스별 역할 요약
- 서비스 간 관계 또는 사용자 흐름
- 주요 문서 링크
- 공통 미정 사항

새 서비스가 추가되거나 기존 서비스의 역할, 범위, 관계가 바뀌면 이 파일을 함께 갱신한다. 하위 서비스의 상세 요구사항을 길게 반복하지 말고, 각 서비스별 `SRS.md`로 이동할 수 있게 요약한다.

## Service SRS.md Guidance

서비스별 `SRS.md`에는 서비스 전체 요구사항을 현재 상태 기준으로 정리한다. 문서는 요구사항 명세서 형식을 따르며, 설명형 문단보다 표를 우선 사용한다. 서비스 성격에 맞게 섹션을 조정하되 다음 내용을 우선
포함한다.

- 문서 정보
- 서비스 개요
- 목표와 비목표
- 대상 사용자와 권한
- 사용자 시나리오
- 기능 요구사항
- 화면 또는 플로우 요구사항
- 데이터와 상태 요구사항
- 외부 연동 요구사항
- 정책, 권한, 제약
- 예외 상황과 오류 처리
- 비기능 요구사항
- 추적성
- 미정 사항

기존 내용이 있으면 보존하면서 최신 요청에 맞게 갱신한다. 확정되지 않은 내용은 단정하지 말고 `미정 사항`에 남긴다.

### 미정 사항 상태 분리

- `미정 사항`에는 `미확정`, `검토 필요`, `보류` 상태만 둔다.
- 기존 `TBD-*` 행이 `확정`되면 ID와 추적 관계를 유지한 채 `확정된 결정` 표로 이동한다.
- `폐기`된 행은 ID와 폐기 근거를 유지한 채 `폐기된 항목` 표로 이동한다.
- 확정 또는 폐기된 행을 삭제하거나 새 ID로 다시 만들지 않는다.
- 결정이 다시 열리면 같은 ID의 행을 `미정 사항` 표로 되돌리고 상태를 갱신한다.
- 각 표 안에서는 요구사항 ID의 숫자 순서로 정렬한다.
- 해당 상태의 행이 없으면 빈 표를 만들지 않는다.

### SRS.md Table Format

`SRS.md`는 다음 표 형식을 우선 사용한다. 필요한 경우 섹션을 추가하되, 요구사항은 추적 가능한 ID를 가진 표 행으로 작성한다.

```markdown
# {서비스명칭} 요구사항 명세서

## 문서 정보

| 항목 | 내용 |
| :- | :- |
| 문서 상태 | 초안 |
| 최종 수정일 | YYYY-MM-DD |
| 담당 범위 | 서비스 범위를 적는다. |

## 기능 요구사항

| ID | 구분 | 요구사항 | 설명 | 우선순위 | 상태 | 출처 | 관련 이슈 |
| :- | :- | :- | :- | :- | :- | :- | :- |
| FR-{서비스식별자}-001 | 기능 | 요구사항을 짧게 적는다. | 세부 조건과 예외를 적는다. | 상 | 초안 | 사용자 요청 | #123 |

## 비기능 요구사항

| ID | 구분 | 요구사항 | 기준 | 우선순위 | 상태 | 출처 | 관련 이슈 |
| :- | :- | :- | :- | :- | :- | :- | :- |
| NFR-{서비스식별자}-001 | 성능 | 요구사항을 짧게 적는다. | 측정 가능하거나 검증 가능한 기준을 적는다. | 중 | 초안 | 설계 판단 | - |

## 미정 사항

| ID | 항목 | 확인 필요 내용 | 영향 범위 | 상태 | 관련 이슈 |
| :- | :- | :- | :- | :- | :- |
| TBD-{서비스식별자}-001 | 항목명 | 확인해야 할 내용을 적는다. | 관련 요구사항 ID를 적는다. | 미확정 | - |

## 확정된 결정

| ID | 항목 | 확정 내용 | 영향 범위 | 상태 | 관련 이슈 |
| :- | :- | :- | :- | :- | :- |
| TBD-{서비스식별자}-002 | 항목명 | 확정된 결정 내용을 적는다. | 관련 요구사항 ID를 적는다. | 확정 | #123 |
```

요구사항 ID의 `{서비스식별자}`는 서비스 경로를 기준으로 하며 영문은 대문자로 변환하고 경로 구분자와 점은 언더스코어로 바꾼다. 예를 들어 서비스 경로가 `spec/service/port/`라면 `FR-PORT-001`,
`NFR-PORT-001`, `TBD-PORT-001` 순서로 부여하고, `spec/service/core/auth/`라면 `FR-CORE_AUTH-001`, `NFR-CORE_AUTH-001`, `TBD-CORE_AUTH-001` 순서로 부여한다. 요구사항 상태는 기본적으로 `초안`, `검토 필요`, `확정`, `보류`, `폐기` 중 하나를 사용한다. 우선순위는 기본적으로 `상`,
`중`, `하` 중 하나를 사용한다. `관련 이슈`에는 연결된 이슈 번호나 URL을 적고, 아직 연결된 이슈가 없으면 `-`로 둔다.

## reference.md Guidance

인터넷 검색, 공식 문서, 기사, 경쟁 서비스, 표준 문서 등 외부 정보를 사용했다면 서비스별 `reference.md`에 누적한다. 출처는 가능한 한 원문 링크와 조회 시각을 남긴다.

```markdown
# 레퍼런스

| 날짜 | 시간 | 출처 | URL | 핵심 내용 | 설계 반영 |
| :- | :- | :- | :- | :- | :- |
| 2026-06-27 | 14:30 KST | 출처명 | https://example.com | 참고한 내용을 간략히 요약한다. | 반영 위치 또는 미반영 사유를 적는다. |
```

## suggestion.md Guidance

사용자 요청 범위 밖에서 발견했지만 서비스 설계 품질을 높일 수 있는 내용은 `suggestion.md`에 작성한다. 제안은 사용자가 채택 여부를 판단할 수 있도록 근거와 영향을 짧게 적는다. 각 제안에는 `ID`를
부여하고, 값은 `SUG-{서비스식별자}-{순열}` 형식으로 작성한다. `{서비스식별자}`는 서비스 경로를 기준으로 하며 영문은 대문자로 변환하고 경로 구분자와 점은 언더스코어로 바꾼다. 예를 들어 서비스 경로가 `spec/service/port/`라면
`SUG-PORT-1`, `SUG-PORT-2` 순서로 부여하고, `spec/service/core/auth/`라면 `SUG-CORE_AUTH-1`, `SUG-CORE_AUTH-2` 순서로 부여한다.

```markdown
# 제안

| ID | 날짜 | 시간 | 제안 내용 | 근거/출처 | 기대 효과 | 채택 여부 |
| :- | :- | :- | :- | :- | :- | :- |
| SUG-PORT-1 | 2026-06-27 | 14:30 KST | 제안 내용을 적는다. | 출처 또는 판단 근거를 적는다. | 기대 효과를 적는다. | 미검토 |
```

`채택 여부`는 기본값을 `미검토`로 두고, 사용자가 명시적으로 수락하면 `채택`, 거절하면 `미채택`으로 갱신한다. 제안이 새로 추가되면 최종 응답에
`추천된 내용이 suggestion.md에 있으니 확인해 주세요.`라는 취지의 문장을 포함한다.

## logs/prompt.md Guidance

서비스 설계가 진행될 때마다 `logs/prompt.md`에 요청자, 사용자 요청, 결과를 간략히 기록한다. 시간은 실제 작업 시점의 날짜, 시간, 시간대를 사용한다.

요청자는 현재 작업 중인 컴퓨터에서 `git config user.name`으로 확인한다. 확인한 값 앞에 `@`를 붙여 `@{user.name}` 형식으로 기록한다. 예를 들어 `user.name`이 `SOPLAY`이면 `@SOPLAY`로 기록한다. `user.name`이 비어 있거나 확인할 수 없으면 값을 추정하지 말고 사용자에게 요청자 표기를 확인한다.

```markdown
# 요청 기록

| 날짜 | 시간 | 요청자 | 요청 요약 | 결과 요약 | 변경 파일 | 후속 상태 |
| :- | :- | :- | :- | :- | :- | :- |
| 2026-06-27 | 14:30 KST | @SOPLAY | 사용자의 요청을 간략히 적는다. | 수행 결과를 간략히 적는다. | SRS.md, logs/prompt.md | 완료 |
```

## Quality Bar

- 모든 산출물은 자연스러운 한국어로 작성한다.
- 고유명사, 라이브러리명, API명, 코드 식별자, URL은 원문을 유지한다.
- Markdown 구조와 표 형식을 안정적으로 유지한다.
- 외부 정보는 출처와 조회 맥락을 남긴다.
- 사용자가 확정하지 않은 추천 사항을 서비스 설계 본문에 확정 사실처럼 반영하지 않는다.
- 기존 사용자 작성 내용을 임의로 삭제하지 않는다.
