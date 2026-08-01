---
name: service-design
description: Create and maintain service SRS.md files and their supporting records under spec/service in this repository. Use when Codex needs to define, refine, research, summarize, or log service requirements while preserving requirement IDs, decision status, sources, suggestions, and the service index.
---

# Service Design

## 목적

서비스 요구사항을 `spec/service/{서비스 경로}/SRS.md`에 정의하고 `spec/service/README.md`에서 서비스 경계와 진입점을 관리한다. 외부 근거, 범위 밖 제안과 요청 이력은 보조
문서로 분리한다.

실제 서비스 범위가 확인되기 전에는 `spec/service/`나 예시 문서를 만들지 않는다.

## 경로

```text
spec/service/
├── AGENTS.md
├── README.md
└── {서비스 경로}/
    ├── SRS.md
    ├── reference.md
    ├── suggestion.md
    └── logs/
        └── prompt.md
```

- 단일 서비스는 `spec/service/{서비스명}/`을 사용한다.
- 서비스 그룹이 확인된 경우에만 `spec/service/{그룹}/{서비스명}/`을 사용한다.
- `reference.md`는 외부 자료를 사용했을 때, `suggestion.md`는 범위 밖 제안이 있을 때만 생성한다.
- 설계 작업을 수행하면 `README.md`, `SRS.md`와 `logs/prompt.md`를 함께 유지한다.

## 절차

1. 적용되는 DOX 체인과 `spec/DICTIONARY.md`를 읽는다.
2. 서비스명, 범위, 사용자, 목표와 제외 범위를 확인한다.
3. 기존 서비스 색인, `SRS.md`와 보조 문서를 읽고 기존 ID와 사용자 작성 내용을 확인한다.
4. 외부 정보가 필요하거나 사용자가 요청한 경우에만 조사하고, 현재 사실은 신뢰할 수 있는 출처로 검증한다.
5. 요구사항을 검증 가능한 문장과 추적 가능한 ID로 작성한다.
6. 미정 사항, 확정된 결정과 폐기된 항목을 상태에 맞는 표로 분리한다.
7. 서비스 경계나 관계가 바뀌면 `spec/service/README.md`를 함께 갱신한다.
8. 사용한 외부 자료는 `reference.md`, 범위 밖 제안은 `suggestion.md`, 요청과 결과는 `logs/prompt.md`에 기록한다.
9. 새 프로젝트 용어가 있으면 `dictionary`를 사용한다.
10. 승인된 범위만 수정하고 문서 검증, 작업 로그와 DOX pass를 수행한다.

## 요구사항 ID

- 기능 요구사항: `FR-{서비스식별자}-001`
- 비기능 요구사항: `NFR-{서비스식별자}-001`
- 미정·결정 항목: `TBD-{서비스식별자}-001`
- 제안: `SUG-{서비스식별자}-1`
- 서비스 식별자는 서비스 상대 경로를 대문자로 바꾸고 경로 구분자와 점을 밑줄로 치환한다.
- 기존 ID는 상태가 바뀌어도 재번호화하지 않는다.

## SRS 계약

`SRS.md`에는 적용되는 범위에서 다음 내용을 포함한다.

- 문서 정보, 서비스 개요, 목표와 비목표
- 대상 사용자, 권한과 사용자 시나리오
- 기능·화면·데이터·상태·외부 연동 요구사항
- 정책, 제약, 예외와 오류 처리
- 비기능 요구사항, 추적성, 미정 사항

요구사항 상태는 `초안`, `검토 필요`, `확정`, `보류`, `폐기`를 사용한다. `TBD-*` 항목은 상태에 따라 `미정 사항`, `확정된 결정`, `폐기된 항목`으로 이동하되 ID를 보존한다.

## 보조 문서 계약

- `reference.md`: 조회 날짜·시간, 출처, URL, 핵심 내용과 반영 위치를 기록한다.
- `suggestion.md`: ID, 날짜·시간, 제안, 근거, 기대 효과와 `미검토`·`채택`·`미채택` 상태를 기록한다.
- `logs/prompt.md`: 날짜·시간, 요청자 표기, 요청·결과 요약, 변경 파일과 후속 상태를 기록한다.
- 요청자 이름이 명시되지 않으면 개인 정보를 추정하지 않고 `사용자`로 기록한다.
- 채택되지 않은 제안은 확정 요구사항에 포함하지 않는다.

## 검증

- 요구사항 ID의 중복, 누락, 재번호화와 잘못된 상태 승격이 없는지 확인한다.
- 서비스 색인과 서비스별 문서의 경로, 역할과 링크가 일치하는지 확인한다.
- 외부 정보에 출처와 조회 맥락이 있는지 확인한다.
- Markdown 표, 링크, 맞춤법과 자연스러운 한국어 표현을 확인한다.
