---
name: pr-creator
description: Publish already committed and validated changes from the current branch to a GitHub Pull Request. Use when Codex needs to inspect the branch diff, verify linked issues and validation evidence, obtain approval for push and PR mutations, prevent duplicate pull requests, and connect verified work with Closes or Refs.
---

# PR Creator

## 목적

현재 브랜치에 이미 존재하는 커밋과 검증 결과를 근거로 일반 push를 수행하고 GitHub Pull Request를 생성하거나 갱신한다. 제목과
본문은 [PR 작성 기준](references/pr-structure.md)을 따른다.

이 스킬은 브랜치를 만들거나 전환하지 않으며 파일 수정, 스테이징, 로컬 커밋과 커밋 재작성을 수행하지 않는다.

## 사전 조건

- Git 원격과 GitHub 저장소를 확인할 수 있어야 한다.
- 현재 브랜치가 기본 브랜치가 아니어야 한다.
- PR에 포함할 커밋과 검증 결과가 존재해야 한다.
- 원격이나 GitHub 연결이 없으면 push와 PR 쓰기 작업을 수행하지 않는다.

## 절차

1. Git 루트, 현재 브랜치, 원격 URL, upstream과 작업 트리를 확인한다.
2. 적용되는 DOX 체인과 `spec/DICTIONARY.md`를 읽는다.
3. GitHub 연결 도구로 기본 브랜치와 저장소를 확인한다.
4. 현재 브랜치가 upstream보다 뒤처졌으면 경고하고 사용자 승인 없이 pull하지 않는다.
5. 기본 브랜치와 `HEAD` 사이의 커밋·diff를 staged, unstaged, untracked 변경과 분리해 검토한다.
6. 비밀 정보, 개인 정보, 원본 데이터, 대용량·생성 산출물의 포함 가능성을 확인한다.
7. 같은 head와 base의 열린·닫힌 PR을 조회해 중복을 확인한다.
8. 연결 이슈의 범위와 인수 조건을 실제 변경·검증 결과와 비교한다.
9. `references/pr-structure.md`에 따라 제목과 본문을 작성한다.
10. 포함 커밋, 작업 트리 상태, 원격 ref, base·head, 제목, 본문, 라벨과 이슈 연결을 제시해 승인을 받는다.
11. 승인 후 일반 push를 수행하고 GitHub 연결 도구로 PR을 생성하거나 승인된 필드만 갱신한다.
12. 원격 head SHA와 PR의 URL, 상태, base, head, 본문, 라벨, 커밋, 파일과 연결 이슈를 재조회한다.

## 이슈 연결

- PR 병합으로 이슈 전체가 해결되면 `Closes #<번호>`를 사용한다.
- 일부 범위 또는 참고 관계면 `Refs #<번호>`를 사용한다.
- 검증되지 않았거나 미구현인 항목은 완료로 표시하지 않는다.
- PR 생성만으로 이슈를 직접 닫지 않는다.

## 승인 경계

- 저장소, 커밋, 이슈와 기존 PR 조회는 읽기 작업으로 수행한다.
- push, PR 생성·수정과 이슈 수정은 정확한 대상을 제시하고 승인받는다.
- 승인 범위가 달라지면 새 계획과 승인을 받는다.
- force push는 사용자가 정확한 대상과 필요성을 별도로 승인하지 않는 한 수행하지 않는다.

## 실패 처리

- 미커밋 변경: 포함 여부를 추정하지 않고 소유 작업의 완료 상태를 확인한다.
- 원격·GitHub 조회 실패: 최신 상태나 중복 PR 부재를 단정하지 않는다.
- 검증 실패: 실패와 영향을 PR 초안에 표시하고 게시 여부를 다시 확인한다.
- push 실패: PR을 생성하지 않고 로컬 커밋 상태와 오류를 보고한다.
- 비밀 정보 가능성: push와 PR 쓰기 작업을 중단한다.

## 검증

- 작업 트리와 기본 브랜치 대비 커밋·파일 목록을 확인한다.
- PR 본문의 변경과 검증이 실제 diff와 실행 기록에 있는지 확인한다.
- push 후 원격 head SHA가 로컬 `HEAD`와 일치하는지 확인한다.
- PR 생성·갱신 후 모든 승인 필드와 연결 이슈를 재조회한다.
- Markdown, 링크, 체크박스, 맞춤법과 자연스러운 한국어 표현을 확인한다.
