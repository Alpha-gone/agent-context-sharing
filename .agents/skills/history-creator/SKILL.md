---
name: history-creator
description: Create and maintain project history documents from service request logs and suggestions. Use when Codex needs to read each service's prompt history and suggestion files under spec/service, summarize work by date as work journals, write history/HISTORY.md, and create or update history/daily/YYYY-MM-DD.md files.
---

# History Creator

## Overview

서비스별 요청 기록과 제안 문서를 날짜별 작업 일지로 정리하여 `$(project_root)/history/` 아래에 누적한다. 전체 요약은 `HISTORY.md` 표로 유지하고, 날짜별 상세 내용은
`daily/YYYY-MM-DD.md`에 작성한다.

## Project Root

1. 현재 작업 디렉터리의 Git 루트를 `$(project_root)`로 확인한다.
2. 서비스 경로는 기본적으로 `$(project_root)/spec/service/`를 사용한다.
3. 현재 작업 디렉터리가 이미 `spec/` 내부라서 `spec/service/`가 존재하지 않고 `service/`만 존재하면, 해당 `service/`를 서비스 경로로 사용한다.
4. 결과 경로는 항상 `$(project_root)/history/`를 사용한다.

## Required Workflow

1. 적용되는 `AGENTS.md` 체인을 읽고 현재 작업 계약을 확인한다.
2. `dictionary` 스킬을 사용하여 용어를 확인한 후, 새 기획 용어가 필요하면 `dictionary` 스킬로 용어집을 갱신한다.
3. 서비스 폴더 목록을 수집한다. 대상은 `spec/service/{서비스명칭}/` 하위 폴더이다.
4. 각 서비스별로 다음 파일을 확인한다.
	- `logs/prompt.md`
	- `prompt.md`
	- `suggestion.md`
5. 존재하지 않는 파일은 건너뛰되, 같은 역할의 파일명이 섞여 있으면 모두 읽고 중복 내용을 병합한다.
6. 각 파일의 표, 헤딩, 본문에서 날짜를 추출한다. 날짜는 `YYYY-MM-DD` 형식을 우선 사용하고, 불완전한 날짜는 문맥상 확정 가능한 경우에만 변환한다.
7. 날짜별로 서비스, 요청 단위, 출처 파일, 요청 요약, 결과 요약, 제안, 후속 상태를 묶는다.
   - `core.auth`처럼 점으로 구분된 하위 서비스는 상위 서비스와 하위 서비스로 분리한다. `HISTORY.md`에서는 원문 식별자인 `core.auth`로 표시하고, 날짜별 작업 일지에서는 `### core` 아래에 `#### auth`로 묶는다. 같은 상위 서비스의 다른 하위 서비스도 같은 `### core` 아래에 둔다.
8. `history/HISTORY.md`를 만들거나 갱신해 날짜별 진행 요약 표를 최신 상태로 정리한다.
9. `history/daily/`를 만들고 날짜마다 `YYYY-MM-DD.md` 파일을 만들거나 갱신한다.
10. 완료 전 DOX pass를 수행하고, 계약 변경이 있으면 가장 가까운 `AGENTS.md`와 영향받은 부모/자식 문서를 갱신한다.

## HISTORY.md Shape

`HISTORY.md`는 전체 진행 내역을 빠르게 볼 수 있는 색인이다. 날짜는 내림차순으로 정렬하고, 같은 날짜 안에서는 서비스명을 기준으로 읽기 쉽게 묶는다.

```markdown
# 히스토리

| 날짜 | 서비스 | 진행 요약 | 주요 결정/제안 | 상세 |
| :- | :- | :- | :- | :- |
| 2026-06-27 | AIRCraft | 요청과 반영 결과를 요약한다. | 제안 또는 결정 사항을 요약한다. | [2026-06-27](daily/2026-06-27.md) |
| 2026-06-27 | AIRPort | 요청과 반영 결과를 요약한다. | 제안 또는 결정 사항을 요약한다. | [2026-06-27](daily/2026-06-27.md) |
```

## Daily File Shape

날짜별 파일은 작업 일지 형식으로 작성한다. 출처가 여러 서비스에 걸쳐 있으면 서비스별 하위 섹션으로 나누고, 같은 서비스에 여러 요청이 있으면 요청별 기록 표로 구분한다. 점으로 구분된 하위 서비스는 상위 서비스 섹션 아래에 하위 서비스 섹션으로 묶는다.

```markdown
# 2026-06-27 작업 일지

## 요약

| 서비스 | 핵심 논의 | 결과 | 후속 작업 |
| :- | :- | :- | :- |
| AIRCraft | 논의 내용을 요약한다. | 반영 또는 결정 내용을 적는다. | 남은 작업을 적는다. |

## 서비스별 기록

### AIRCraft

| 순번 | 요청 | 결과 | 제안 | 출처 |
| :- | :- | :- | :- | :- |
| 1 | 사용자 요청 또는 prompt 기록의 핵심을 적는다. | 수행 결과와 변경 내용을 적는다. | `suggestion.md`에 있는 관련 제안을 적는다. 없으면 `없음`으로 적는다. | `spec/service/craft/logs/prompt.md`, `spec/service/craft/suggestion.md` |
| 2 | 같은 날짜의 다음 요청을 별도 행으로 적는다. | 요청별 수행 결과를 적는다. | 관련 제안 또는 `없음`을 적는다. | `spec/service/craft/logs/prompt.md` |

### AIRPort

| 순번 | 요청 | 결과 | 제안 | 출처 |
| :- | :- | :- | :- | :- |
| 1 | 사용자 요청 또는 prompt 기록의 핵심을 적는다. | 수행 결과와 변경 내용을 적는다. | `suggestion.md`에 있는 관련 제안을 적는다. 없으면 `없음`으로 적는다. | `spec/service/port/logs/prompt.md`, `spec/service/port/suggestion.md` |

### core

#### auth

| 순번 | 요청 | 결과 | 제안 | 출처 |
| :- | :- | :- | :- | :- |
| 1 | 사용자 요청 또는 prompt 기록의 핵심을 적는다. | 수행 결과와 변경 내용을 적는다. | 관련 제안 또는 `없음`을 적는다. | `spec/service/core/auth/logs/prompt.md` |

## 후속 작업

- 미정 사항이나 확인이 필요한 작업을 적는다.
```

## Summarization Guidance

- 모든 산출물은 자연스러운 한국어로 작성한다.
- 고유명사, 서비스명, 파일명, API명, URL은 원문 표기를 유지한다.
- 원문을 장황하게 복사하지 말고 작업 일지에 필요한 결정, 요청, 결과, 후속 상태를 요약한다.
- 확정되지 않은 제안은 결정처럼 쓰지 말고 `제안` 또는 `미검토`로 표시한다.
- 같은 날짜와 서비스에 요청이 여러 개 있으면 하나로 뭉치지 말고 요청별 행 또는 `#### 요청 {번호/시간/원문 헤딩}` 하위 섹션으로 나눈다.
- 요청 식별 열은 `요청 ID`가 아닌 `순번`으로 쓰고, 서비스 섹션 안에서 `1`, `2`처럼 순차 부여한다.
- 제안은 요청별로 연결 가능한 항목만 같은 행에 적고, 서비스 공통 제안은 별도 행 또는 후속 작업에 적는다.
- `core.auth`처럼 점으로 구분된 서비스는 `HISTORY.md`에서 점을 포함한 원문 식별자를 유지한다. 날짜별 작업 일지에서는 상위 서비스명과 하위 서비스명을 임의로 합치거나 평면화하지 않고, 상위 서비스의 `###` 섹션 아래에 하위 서비스의 `####` 섹션을 사용한다.
- 날짜가 없는 기록은 가능한 한 원본 파일의 인접 날짜나 표 행을 기준으로 판단한다. 판단할 수 없으면 `날짜 미상` 섹션을 별도로 만들고 최종 응답에서 알린다.
- 기존 `HISTORY.md`와 daily 파일의 사용자 작성 내용을 임의로 삭제하지 않는다. 새로 재구성해야 할 때도 의미 있는 기존 메모는 보존한다.
- Markdown 표 정렬과 링크가 깨지지 않도록 검토한다.
