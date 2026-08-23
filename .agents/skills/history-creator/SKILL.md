---
name: history-creator
description: Create and maintain spec/history project history documents from dated service request logs and suggestion records in this repository. Use when Codex needs to summarize confirmed work by date, update the project history index, or create daily work journals without exposing private .personal logs.
---

# History Creator

## 목적

`spec/service/` 아래의 날짜가 있는 요청 기록과 제안 문서를 근거로 공개 가능한 프로젝트 이력을 `spec/history/`에 정리한다. 비공개 운영 기록인 `.personal/logs/`는 입력이나
산출물로 사용하지 않는다.

## 입력과 산출물

| 구분        | 경로                                        |
|-------------|---------------------------------------------|
| 기본 입력   | `spec/service/{서비스 경로}/logs/prompt.md` |
| 조건부 입력 | `spec/service/{서비스 경로}/suggestion.md`  |
| 전체 색인   | `spec/history/HISTORY.md`                   |
| 날짜별 기록 | `spec/history/daily/YYYY-MM-DD.md`          |

`spec/service/`가 없거나 날짜와 결과를 확인할 입력이 없으면 빈 이력 문서를 만들지 않고 상태를 보고한다.

## 절차

1. 적용되는 DOX 체인과 `spec/DICTIONARY.md`를 읽는다.
2. 서비스 폴더별 `logs/prompt.md`와 `suggestion.md`의 존재 여부를 확인한다.
3. 날짜, 서비스, 요청 요약, 결과, 제안, 출처와 후속 상태를 추출한다.
4. 불완전한 날짜는 문맥으로 확정 가능한 경우에만 `YYYY-MM-DD`로 정규화한다.
5. 같은 날짜와 서비스의 중복 기록을 병합하되 서로 다른 요청은 별도 행으로 보존한다.
6. `spec/history/HISTORY.md`를 날짜 내림차순 색인으로 갱신한다.
7. 해당 날짜의 `spec/history/daily/YYYY-MM-DD.md`를 요청별 작업 일지로 갱신한다.
8. 승인된 범위만 수정하고 링크·표·맞춤법 검증, 작업 로그와 DOX pass를 수행한다.

## 문서 형식

`HISTORY.md`는 다음 열을 사용한다.

| 날짜       | 서비스     | 진행 요약          | 주요 결정·제안        | 상세                  |
|------------|------------|--------------------|-----------------------|-----------------------|
| YYYY-MM-DD | {서비스명} | {요청과 결과 요약} | {결정 또는 제안 상태} | `daily/YYYY-MM-DD.md` |

날짜별 문서는 `요약`, `서비스별 기록`, `후속 작업` 순서를 기본으로 사용한다. 서비스별 기록에는 순번, 요청, 결과, 제안과 출처를 포함한다.

## 기록 계약

- 원문을 복사하지 않고 결정, 요청, 결과와 후속 상태를 요약한다.
- 확정되지 않은 제안은 `제안` 또는 `미검토`로 표시한다.
- 날짜를 판단할 수 없는 기록은 임의 배치하지 않고 `날짜 미상`으로 분리해 보고한다.
- 기존 사용자 작성 내용과 의미 있는 메모를 보존한다.
- 서비스 그룹이 있으면 실제 디렉터리 계층과 서비스 식별자를 그대로 유지한다.

## 검증

- 모든 이력 행을 원본 서비스 기록으로 추적할 수 있는지 확인한다.
- 날짜, 서비스명, 제안 상태와 출처 링크가 입력과 일치하는지 확인한다.
- 비공개 `.personal/` 내용이 포함되지 않았는지 확인한다.
- Markdown 표, 상대 링크, 맞춤법과 자연스러운 한국어 표현을 확인한다.
