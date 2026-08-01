---
name: diagram-creator
description: Create, update, and validate evidence-based Mermaid diagrams in this repository. Use when Codex needs to explain structure, ownership, dependencies, runtime interactions, state transitions, data relationships, or deployment topology while keeping diagrams consistent with Markdown specifications and confirmed terminology.
---

# Diagram Creator

## 목적

Markdown 명세의 근거를 바탕으로 관계 이해를 실질적으로 돕는 최소한의 Mermaid 다이어그램을 작성한다. 입력에 없는 구성 요소, 상태, 관계 또는 기술 결정을 추가하지 않는다.

## 입력

- 다이어그램을 삽입하거나 수정할 대상 Markdown 문서
- 대상 문서가 참조하는 요구사항, 설계, 용어와 적용되는 DOX 체인
- 기존 다이어그램의 명칭, 관계와 설명

입력이 충돌하거나 관계 방향, 전이 조건 또는 데이터 소유권이 불명확하면 선택지와 영향을 보고하고 확정 전 변경하지 않는다.

## 절차

1. 적용되는 DOX 체인과 `spec/DICTIONARY.md`를 읽는다.
2. 다이어그램이 답할 질문과 독자를 한 문장으로 정의한다.
3. 입력에서 노드, 관계, 방향, 조건, 경계와 근거 식별자를 추출한다.
4. 문장이나 표보다 시각화가 관계 이해를 개선하는지 판단한다.
5. 표현 대상에 맞는 가장 작은 Mermaid 유형을 선택한다.
6. 본문 용어와 안정적인 영문 식별자를 사용해 다이어그램을 작성한다.
7. 앞 문단에 목적과 범위를, 뒤 문단에 핵심 흐름·예외·경계·미정 사항을 설명한다.
8. 승인된 문서만 수정하고 구문·의미 검증, 작업 로그와 DOX pass를 수행한다.

## 유형 선택

| 표현 대상                      | Mermaid 유형      |
|--------------------------------|-------------------|
| 서비스·사용자·외부 시스템 경계 | `flowchart`       |
| 구성 요소·책임·의존 관계       | `flowchart`       |
| 요청·응답·비동기 상호작용      | `sequenceDiagram` |
| 생명주기와 전이 조건           | `stateDiagram-v2` |
| 영속 데이터 관계               | `erDiagram`       |
| 배포 단위·인프라·네트워크 경계 | `flowchart`       |

한 단계 관계, 두 항목 비교 또는 짧은 목록은 문장이나 표를 사용한다. 한 다이어그램이 여러 질문을 섞으면 책임이나 시나리오별로 분리한다.

## 작성 계약

- 코드 블록 언어는 `mermaid`로 지정한다.
- 표시 문구는 본문 용어와 일치시키고 화살표는 실제 관계 방향과 일치시킨다.
- 미확정 기술은 논리 요소로 표현하고 특정 제품명을 추가하지 않는다.
- 장식은 의미 구분에 필요할 때만 사용한다.
- 다이어그램만으로 요구사항이나 설계 판단을 대신하지 않는다.

## 검증

- 코드 블록, 식별자, 화살표, 분기와 Mermaid 유형별 문법을 확인한다.
- 저장소에 렌더러나 린터가 있으면 실행하고, 없으면 정적 검증의 한계를 보고한다.
- 모든 요소와 관계가 본문 또는 확인된 입력에 존재하는지 확인한다.
- 본문과 다이어그램의 명칭, 상태, 방향과 예외가 일치하는지 확인한다.
- Markdown, 맞춤법과 자연스러운 한국어 표현을 확인한다.
