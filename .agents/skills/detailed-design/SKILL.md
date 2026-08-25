---
name: detailed-design
description: Create and maintain implementation-ready SDD.md files from confirmed service requirements in this repository. Use when Codex needs to refine architecture, interfaces, data models, processing flows, state and error handling, security, deployment, or requirement traceability under spec/service, while preserving requirement status and adopted decisions.
---

# Detailed Design

## 목적

`spec/service/{서비스 경로}/SRS.md`의 확정된 범위와 추적 ID를 구현 가능한 상세 설계로 구체화해 같은 폴더의 `SDD.md`에 기록한다. 근거가 없는 기술 선택이나 관계는 확정하지 않는다.

문서 구조와 표현 규칙은 [상세 설계 문서 작성 기준](references/document-guidance.md)을 따른다. 관계 시각화가 필요하면 `diagram-creator`를 함께 사용한다.

## 입력

| 우선순위 | 파일             | 사용 기준                                                             |
|----------|------------------|-----------------------------------------------------------------------|
| 필수     | `SRS.md`         | 범위, 요구사항, 상태, 정책, 제약과 추적 ID의 기준으로 사용한다.       |
| 조건부   | `reference.md`   | 외부 계약과 기술 판단의 근거로만 사용한다.                            |
| 조건부   | `suggestion.md`  | `상태`가 정확히 `채택`인 행만 설계 입력으로 사용한다.                 |
| 조건부   | `logs/prompt.md` | 사용자 결정과 변경 맥락을 확인하되 현재 `SRS.md`와 충돌하면 보고한다. |

`SRS.md`가 없거나 입력이 충돌해 결과가 달라질 수 있으면 파일을 만들지 말고 필요한 결정과 영향을 보고한다.

## 절차

1. 적용되는 DOX 체인과 `spec/DICTIONARY.md`를 읽는다.
2. 대상 서비스 경로와 기존 `SDD.md`를 확인한다.
3. 요구사항 ID, 상태, 제약, 채택된 제안과 미정 사항을 분리한다.
4. 각 설계 결정을 하나 이상의 요구사항 ID 또는 채택 제안 ID에 연결한다.
5. 근거가 부족한 항목은 가능한 선택지와 영향을 제시하고 `설계 미정 사항`으로 남긴다.
6. 구조·상호작용·상태·데이터 관계를 시각화할 필요가 있으면 `diagram-creator` 계약을 적용한다.
7. 요구사항 추적표에서 모든 설계 대상 요구사항의 반영 위치와 검증 방법을 확인한다.
8. 승인된 범위만 수정하고 작업 로그와 DOX pass를 수행한다.

## 설계 계약

- 요구사항의 `초안`, `검토 필요`, `확정`, `보류`, `폐기` 상태를 보존한다.
- 구현 언어, 프레임워크, 저장소와 통신 방식이 미확정이면 특정 기술을 사실처럼 선택하지 않는다.
- 인터페이스에는 필요한 범위에서 책임, 입력, 출력, 오류, 권한, 멱등성과 트랜잭션 경계를 명시한다.
- 성능, 보안, 가용성, 관측성과 운영 요구사항은 검증 가능한 설계에 연결한다.
- 기존 사용자 작성 내용과 유효한 설계 결정을 보존한다.

## 산출물

`spec/service/{서비스 경로}/SDD.md`에 다음 중 적용되는 내용을 작성한다.

- 문서 정보, 설계 범위와 입력
- 시스템 컨텍스트와 아키텍처
- 구성 요소, 인터페이스와 데이터 모델
- 처리 흐름, 상태, 오류와 복구
- 보안, 권한, 관측성, 배포와 운영
- 요구사항 추적표와 설계 미정 사항
- 필요한 문서 하단 주석

적용되지 않는 섹션은 억지로 채우지 말고 제외 사유를 짧게 기록한다.

## 검증

- 모든 설계 요소를 요구사항 또는 채택된 결정으로 추적할 수 있는지 확인한다.
- 채택되지 않은 제안이 본문, 다이어그램과 추적표에 포함되지 않았는지 확인한다.
- 본문, 표와 다이어그램이 같은 설계를 설명하는지 확인한다.
- Markdown, 내부 링크, 맞춤법과 자연스러운 한국어 표현을 확인한다.
