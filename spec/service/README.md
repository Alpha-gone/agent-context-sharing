# 서비스 명세 색인

## 목적

이 문서는 서비스의 범위·현재 상태와 요구사항·설계·개발·검증의 소유 문서 진입점을 제공합니다.

## 서비스 목록

| 서비스 | 역할 | 현재 상태 | 주요 문서 |
| --- | --- | --- | --- |
| 에이전트 컨텍스트 관리 | 공유 컨텍스트 그래프와 호스트 내장 MCP의 직접 연결·표준 OAuth를 제공한다. | 신규 구현·분야/전체 인수 완료. 선정 CLI·사용자 화면·격리 운영 범위만 검증했으며 실제 외부 효과·운영 공개는 대기한다. | [SRS](agent-context/SRS.md), [SDD](agent-context/SDD.md), [개발 계획](agent-context/DEVELOPMENT_PLAN.md) |

## 서비스 관계

웹 관리·계정 인증·보호 MCP·저장·검색·색인은 하나의 서버 서비스가 소유하며 감사·평가는 별도 실행기로 동작한다. 호스트는 표준 OAuth와 MCP로 직접 연결하며 별도 프로젝트 클라이언트 설치를 요구하지 않는다. 상세 계약은 [서버 경계](agent-context/sdd/architecture.md#외부-경계)와 [MCP 계약](agent-context/sdd/mcp.md)이 소유한다.

## 구현·실행 판정

[TRACE-05 최종 문서 점검](agent-context/reviews/2026-10-29-trace-05.md)까지 신규 개발 계획 124개 작업을 완료했다. 현재 미착수 계획 작업은 없고 아래의 실환경 실증/공개 대기는 별도로 유지한다.

- [TEST-09](agent-context/reviews/2026-10-28-test-09.md): 같은 접속 정보의 전체 필수 DB/race 6,422건씩 두 회차·기준 상태 보존과 전용 복구/이행·실제 파일·격리 장애 회복을 구분한다. 일반 실행에서 건너뛴 전용 복구/이행 두 건은 소유 스크립트에서 통과했다. Linux DB 시험 한 건은 미실행이며 같은 DB 계약의 macOS 필수 DB 결과와 구분한다.
- [TEST-08](agent-context/reviews/2026-10-27-test-08-completion.md): SDK v1.8.0·MCP 2026-07-28, macOS Codex CLI 0.160.1 exec·기능 활성·파일 자격 저장의 실제 인수다. keyring·TUI·앱/IDE·다른 호스트/판·운영 주소 전환은 미인수다.
- [TEST-06](agent-context/reviews/2026-10-28-test-06-completion.md): 실제 Chrome·자동 키보드/접근성 트리와 사용자 화면 확인의 조정 범위다. 추가 수동 키보드·실제 VoiceOver는 제외한 미실행이며 Safari의 미확인 조합까지 넓히지 않는다.
- [TRACE-04](agent-context/reviews/2026-10-29-trace-04.md): 요구사항 184건과 작업 124개의 개별 근거·판·실행 상태를 연결한다. 코드/서버 계약·실제 호스트·진단 파일·정량 효과·운영을 별도 판정하며 요구사항 전면 충족을 선언하지 않는다.
- 실제 Gemini·공개 데이터 전체 검색 효과·외부 업무/비용·조건부 기능의 운영 승격·실제 배포/공개 HTTPS/Tunnel은 별도 인수 대기다. 절차와 보존·복귀 조건은 [운영 매뉴얼](../../OPERATIONS.md)을 따른다.

단계별 표본·명령·실패/수정·당시 집계는 [검토 기록](agent-context/reviews/)과 [요청 기록](agent-context/logs/prompt.md)이 소유한다. 과거 시점의 상태를 현재 안내의 진행 상태로 사용하지 않는다.

## 공통 미정 사항

현재 요구사항 미정은 `TBD-AGENT_CONTEXT-048` 인증 고도화, `049` 계정 소멸, `052` 운영자 역할 분리, `060` CIMD, `061` 한국어 형태소 분석의 다섯 건이다. 내용과 결정 조건은 [SRS 미정](agent-context/SRS.md#미정-사항)을 따른다. 설계 수준 미정은 0건이며 [해소한 설계 선택](agent-context/sdd/traceability.md#해소한-설계-선택)에서 갱신 수명 `063`과 선정 SDK/CLI 범위 `064`를 구분한다. 다른 지원 표면·미채택 기술·운영 플랜 값을 임의로 확정하지 않는다.

[TRACE-03](agent-context/reviews/2026-10-28-trace-03.md)의 미정/조건부 채택 경계와 [개발 계획](agent-context/DEVELOPMENT_PLAN.md)의 작업 상태를 함께 확인한다. 작업 완료 수는 설계 선택의 재확정이나 실제 효과·운영 공개 판정을 대신하지 않는다.
