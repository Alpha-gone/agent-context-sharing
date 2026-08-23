---
name: issue-based-spec
description: Apply the confirmed scope of an existing GitHub issue to this repository's service specifications while preserving requirement IDs and traceability. Use when Codex needs to classify an issue, coordinate service-design, detailed-design, diagram-creator, or dictionary changes, validate approved stages, and synchronize only verified checklist results back to the source issue.
---

# Issue Based Spec

## 목적

기존 GitHub 이슈의 확정 범위를 요구사항, 상세 설계, 결정, 제안, 미정 사항과 구현 작업으로 분류해 명세에 추적 가능하게 반영한다. 분류
기준은 [이슈-명세 매핑 기준](references/spec-mapping.md)을 따른다.

이 스킬은 파일 변경과 이슈 동기화를 조정하지만 커밋, push와 Pull Request를 직접 수행하지 않는다.

## 사전 조건

- GitHub 저장소와 이슈 번호를 확인할 수 있어야 한다.
- 대상 서비스와 `SRS.md` 또는 `SDD.md` 경로를 식별할 수 있어야 한다.
- 원격 저장소나 이슈 원문을 조회할 수 없으면 캐시나 기억을 최신 상태로 간주하지 않는다.

## 절차

1. Git 루트, 현재 브랜치, upstream과 작업 트리 상태를 확인한다.
2. 적용되는 DOX 체인과 `spec/DICTIONARY.md`를 읽는다.
3. GitHub 연결 도구로 이슈 본문, 체크리스트, 댓글, 라벨과 상태를 재조회한다.
4. 댓글을 포함한 입력을 확정 사항, 제안과 미정 사항으로 구분한다.
5. 기존 명세와 보조 문서를 읽고 ID, 상태, 결정과 충돌을 확인한다.
6. 충돌하면 위치, 선택지와 영향을 보고하고 사용자 결정 전 변경하지 않는다.
7. 단계별 대상 파일, 반영 항목, 보존·신규 ID, 완료 조건, 검증, 이슈 갱신 후보와 제외 범위를 체크박스 계획으로 제시한다.
8. 승인 후 요구사항은 `service-design`, 상세 설계는 `detailed-design`, 시각화는 `diagram-creator`, 용어는 `dictionary` 계약으로 반영한다.
9. 단계별 검증에 성공한 항목만 완료 근거로 수집한다.
10. 승인된 이슈 체크리스트 중 근거가 충족된 항목만 수정하고 이슈를 재조회한다.
11. 사용자가 커밋을 요청하면 정확한 변경 범위와 검증 결과를 정리해 전달하고 커밋 자체는 이 스킬의 범위 밖으로 둔다.
12. 프로젝트 작업 로그와 DOX pass를 수행한다.

## 승인 경계

- 이슈와 명세의 조회·분류·충돌 분석은 읽기 작업으로 수행한다.
- 로컬 파일 변경과 GitHub 이슈 수정은 각각 정확한 대상을 제시하고 승인받는다.
- 파일 변경 승인을 이슈 수정, 커밋, push 또는 PR 승인으로 확대 해석하지 않는다.
- 이슈 종료는 자동으로 수행하지 않는다.
- 사용자가 확정하지 않은 제안을 명세의 확정 내용으로 승격하지 않는다.

## 소유 스킬

| 변경 유형                                    | 소유 스킬         |
|----------------------------------------------|-------------------|
| 요구사항, 정책, 사용자 흐름, 서비스 색인     | `service-design`  |
| 아키텍처, 인터페이스, 데이터 모델, 처리 흐름 | `detailed-design` |
| 구조, 상호작용, 상태, 데이터 관계 시각화     | `diagram-creator` |
| 신규 프로젝트 용어                           | `dictionary`      |
| 명시적으로 요청된 로컬 커밋                  | 이 스킬의 범위 밖 |

## 실패 처리

- GitHub 조회 실패: 외부 쓰기 작업을 수행하지 않고 확인하지 못한 항목을 보고한다.
- 대상 서비스 불명확: 후보 경로와 차이를 제시하고 확인 전 변경하지 않는다.
- 추적 ID 충돌: 기존 ID를 재번호화하지 않고 신규 ID 후보와 원인을 보고한다.
- 검증 실패: 실패 항목을 체크리스트 완료 후보에서 제외한다.
- 작업 트리 충돌: 승인 범위 밖 변경을 보존하고 파일별 충돌을 보고한다.

## 검증

- 변경 전후 요구사항 ID, 상태와 이슈 추적 링크를 비교한다.
- 소유 스킬의 문서별 검증을 수행한다.
- 실제 근거가 없는 이슈 체크리스트를 완료 처리하지 않는다.
- 이슈 재조회 결과에서 승인 범위 밖 본문·라벨·상태 변경이 없는지 확인한다.
- Markdown, 링크, 맞춤법과 자연스러운 한국어 표현을 확인한다.
