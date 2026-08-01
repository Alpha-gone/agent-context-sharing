---
name: spec-issue-creator
description: Draft traceable GitHub issues from confirmed service specifications in this repository and register only explicitly approved issues. Use when Codex needs to turn SRS.md, SDD.md, references, adopted suggestions, or prompt records into independently implementable, investigatory, or decision-oriented issue drafts.
---

# Spec Issue Creator

## 목적

서비스 명세의 범위와 추적 ID를 보존하면서 독립적으로 구현·검증할 수 있는 GitHub 이슈 초안을 작성한다. 형식과 분할 기준은 [이슈 작성 기준](references/issue-structure.md)을
따른다.

이 스킬은 기존 이슈를 명세에 반영하거나 브랜치 변경을 PR로 게시하지 않는다.

## 사전 조건

- `spec/service/README.md`와 대상 서비스의 `SRS.md` 또는 `SDD.md`를 확인한다.
- 조건부로 `reference.md`, 채택된 `suggestion.md`, `logs/prompt.md`를 근거로 사용한다.
- GitHub 저장소와 원격 연결을 확인할 수 없으면 초안까지만 제공하고 이슈를 등록하지 않는다.

## 절차

1. Git 루트, 원격 저장소, 현재 브랜치와 upstream을 확인한다.
2. 적용되는 DOX 체인과 `spec/DICTIONARY.md`를 읽는다.
3. 대상 명세에서 확정된 사실, 사용자 결정, 채택된 제안, 미정 사항과 추적 ID를 분리한다.
4. 구현 산출물, 제외 범위, 선행 조건과 검증 가능한 인수 조건을 추출한다.
5. 구현을 막는 미정 사항은 조사·결정 이슈 후보로 분리한다.
6. GitHub 연결 도구로 열린 이슈와 닫힌 이슈를 조회해 중복을 확인한다.
7. 독립적으로 완료할 수 있는 단위로 분할하고 `references/issue-structure.md` 형식의 초안을 작성한다.
8. 저장소에 실제로 존재하는 라벨과 마일스톤만 후보로 제시한다.
9. 생성할 저장소, 제목, 본문, 라벨, 마일스톤과 담당자를 제시해 승인을 받는다.
10. 승인된 경우에만 이슈를 생성하고 번호, URL, 본문, 상태와 메타데이터를 재조회한다.
11. 명세의 관련 이슈 링크를 갱신해야 하면 별도 파일 변경 계획을 제시하고 해당 문서 소유 스킬을 사용한다.
12. 로컬 파일을 변경했다면 작업 로그와 DOX pass를 수행한다.

## 승인 경계

- 초안 작성과 중복 조회는 읽기 작업으로 수행한다.
- 이슈 생성·수정, 라벨·마일스톤 변경과 명세 갱신은 각각 정확한 대상을 제시하고 승인받는다.
- 이슈 생성 승인과 명세 링크 갱신 승인을 서로 대신하는 것으로 해석하지 않는다.
- 명세 근거가 없는 일정, 버전, 우선순위나 메타데이터를 추가하지 않는다.

## 검증

- 모든 범위와 인수 조건을 명세 또는 확인된 사용자 결정으로 추적한다.
- 기존 이슈와의 중복, 라벨과 마일스톤의 실제 존재 여부를 확인한다.
- 생성 후 대상 저장소, 번호, 제목, 본문, 상태, URL과 메타데이터를 재조회한다.
- Markdown 링크, 체크박스, 맞춤법과 자연스러운 한국어 표현을 확인한다.
- 수행하지 못한 조회나 검증을 성공으로 기록하지 않는다.
