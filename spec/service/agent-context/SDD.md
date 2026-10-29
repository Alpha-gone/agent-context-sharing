# 에이전트 컨텍스트 관리 시스템 상세 설계

## 문서 정보

- 서비스 식별자: `AGENT_CONTEXT`
- 기준: 2026-10-11 확정 [SRS](SRS.md), [공식 근거](reference.md), 채택된 결정
- 문서 상태: 서버·평가 기능과 선정 CLI 직접 연결 인수 완료. 정량 효과·전체 운영/배포 인수는 대기
- 이 문서의 책임: 설계 진입점·문서 소유권·상태. 상세 계약은 `sdd/`의 분야별 문서가 소유한다.

## 설계 목적과 범위

AI 에이전트 제텔카스텐을 위한 공유 컨텍스트 그래프의 확정 요구사항을 구현 가능한 구조·데이터·처리·보안·운영·검증 설계로 구체화한다. 요구사항을 신설하거나 채택되지 않은 제안을 확정하지 않는다. FR 171건과 NFR 13건, 총 184건을 [추적표](sdd/traceability.md#요구사항-추적표)로 연결한다.

분야별 문서는 독립된 상세 설계의 정본이다. 같은 계약을 여러 문서에 복사하지 않으며 다른 분야와 만나는 지점은 상대 링크를 사용한다. 변경 시 해당 분야 문서와 영향받은 추적표·상태를 갱신한다. 적용 지침은 [서비스 DOX](AGENTS.md)와 [상세 설계 DOX](sdd/AGENTS.md)를 따른다.

## 분야별 설계 문서

| 분야 | 문서 | 소유하는 상세 계약 |
|---|---|---|
| 아키텍처 | [architecture.md](sdd/architecture.md) | 시스템·배포 경계, 기술 기준, 패키지·의존, 조립, HTTP, DB 풀·예약 |
| 데이터 | [data.md](sdd/data.md) | AGE 사상, 관계형 열·키, 인덱스·격리·불변식, 마이그레이션, 온라인 hot/cold |
| MCP | [mcp.md](sdd/mcp.md) | SDK·표준 수명주기·전송, 14종 도구·입출력, 행위자·멱등성·오류 |
| 인증 | [authentication.md](sdd/authentication.md) | 계정·비밀번호·웹 세션, OAuth 발견·인가·Bearer·회전·폐기·보존 |
| 권한·한도 | [authorization.md](sdd/authorization.md) | 등급·팀 상속, 계정 플랜·요청/누적 한도 |
| 수명주기 | [context-lifecycle.md](sdd/context-lifecycle.md) | 파생 근거·사건·관계, 폐기·삭제·복구·감사, 쓰기·잠금 순서 |
| 검색 | [search.md](sdd/search.md) | 채널·스냅숏, 홉 조회 보호, 라우팅·순위·접기·경로·예산·흐름 |
| 색인 | [indexing.md](sdd/indexing.md) | 큐·세대·리스·재시도·결과 반영, 제공자 검증·재색인 |
| 웹 | [web.md](sdd/web.md) | 출처·세션·등급 처리, 화면·목록·팀·선택·시각화·연결 관리 |
| 운영 | [operations.md](sdd/operations.md) | 기동·종료, 주기 작업, 배포 구성·개발/운영·터널, 백업·복구 |
| 관측성 | [observability.md](sdd/observability.md) | 지표·감사 조회, 여섯 진단 파일·상관·회전·보존·총상한·실패 |
| 평가 | [evaluation.md](sdd/evaluation.md) | 준비·통제·독립 단위 집계, 공개 세트·의미 단위·원본·임베딩·업무 비용 |
| 테스트 | [testing.md](sdd/testing.md) | 단위·DB·격리·대역, 정확한 첫 호스트 인수·이행·복귀 |
| 추적·미정 | [traceability.md](sdd/traceability.md) | 184건의 주 책임 설계·검증 방법, 해소 선택·실제 검증 대기 |

각 분야를 처음부터 구현하는 작업·선행 조건·산출물·완료 기준은 [개발 계획 진입점](DEVELOPMENT_PLAN.md)과 같은 이름의 `development-plan/*.md`에서 관리한다. 계획의 진행 상태는 이 문서의 설계·현재 구현 상태와 별개다.

## 구현과 검증 상태

- 신규 서버·웹·평가 기능의 분야/전체 인수는 [TEST-09](reviews/2026-10-28-test-09.md)에 연결한다. 같은 접속 정보의 전체 필수 DB/race 두 회차와 기준 상태 보존, 격리 파일·WAL·복구·이행·조합 장애 회복을 범위별로 확인했다. [TRACE-04](reviews/2026-10-29-trace-04.md)는 184개 요구사항과 124개 작업의 개별 실행 근거를 연결한다.
- 직접 연결 지원은 [TEST-08](reviews/2026-10-27-test-08-completion.md)의 SDK v1.8.0·MCP 2026-07-28·macOS Codex CLI 0.160.1 exec·기능 활성·파일 자격 저장 구성에 한정한다. 실제 1시간 자연 만료·회전·재시작/재사용을 포함하며 keyring·TUI·앱/IDE·다른 호스트/판과 운영 주소 전환은 미인수다.
- 웹은 [TEST-06](reviews/2026-10-28-test-06-completion.md)의 실제 Chrome·자동 키보드/접근성 트리·사용자 화면 확인 범위다. 추가 수동 키보드·실제 VoiceOver는 사용자 요청으로 제외한 미실행이고 미확인 Safari 조합까지 통과로 확대하지 않는다. 접근성 구현 계약은 유지한다.
- 평가 실행기·결과 저장·판정 기능은 인수했으며 실제 Gemini·공개 데이터 전체 검색 품질·외부 과업/청구·실제 효과와 운영 기본값 승격은 별도 실증 대기다. 격리 운영 인수는 실제 운영 DB 전환·공개 HTTPS/Tunnel·전체 배포 인수를 대신하지 않는다.

현재 작업 집계는 [개발 계획](DEVELOPMENT_PLAN.md), 상세 명령·표본·실패/수정·당시 결과는 [검토](reviews/)·[요청 기록](logs/prompt.md)이 소유한다. 이 진입점에는 문서 책임과 현재 판정만 유지하며 단계별 시험 수·진행 이력은 복제하지 않는다.

[TRACE-05](reviews/2026-10-29-trace-05.md)에서 요구사항·분야별 소유·실행 상태·안내의 최종 문서 정합성을 점검했다. 문서/계획 완료는 실제 외부 효과나 운영 공개의 인수 상태를 바꾸지 않는다.

## 읽기와 변경 순서

1. SRS의 대상 요구사항·확정 결정과 적용 DOX를 확인한다.
2. 위 분야 표에서 주 책임 문서를 읽고 연결된 다른 분야의 계약을 확인한다.
3. 설계 변경은 소유 문서에 반영하고 요구사항 변경이 필요할 때만 SRS를 먼저 갱신한다.
4. 추적표의 설계·검증 연결과 구현/검증 대기 상태를 정합화한다.
5. 파일·앵커 링크·Markdown·용어·DOX를 검사한다. 실제 기능 변경 시 별도 테스트 전략을 따른다.

## 주요 계약 바로가기

- [패키지 경계](sdd/architecture.md#패키지-경계) · [DB 연결·예약](sdd/architecture.md#데이터베이스-연결)
- [배포 구성](sdd/operations.md#배포-구성) · [개발·운영 환경](sdd/operations.md#개발운영-환경-경계)
- [판정의 직렬화](sdd/context-lifecycle.md#판정의-직렬화) · [폐기·복구 채널](sdd/context-lifecycle.md#폐기와-복구의-채널)
- [홉 조회·적재 보호](sdd/search.md#고연결-노드의-조회적재-보호) · [인덱스](sdd/data.md#인덱스)
- [기동·종료](sdd/operations.md#기동과-종료) · [진단 로그](sdd/observability.md#종류별-진단-로그)
- [평가 실행](sdd/evaluation.md#초기-공개-평가-실행-설계) · [색인 준비·집계](sdd/evaluation.md#검증을-가능하게-하는-설계)

## 호스트 직접 연결 전환 설계

[아키텍처의 연결 경계](sdd/architecture.md#호스트-직접-연결-전환-설계), [MCP 계약](sdd/mcp.md), [OAuth 설계](sdd/authentication.md), [첫 Codex CLI 인수](sdd/testing.md#첫-codex-cli-상호운용-시험)에서 분야별로 정의한다.

## 요구사항 추적표

[요구사항별 주 책임 설계·검증 방법](sdd/traceability.md#요구사항-추적표)을 참조한다.

## 설계 미정 사항

[미정·해소 항목](sdd/traceability.md#설계-미정-사항)을 참조한다. 요구사항 수준의 미정은 SRS가 계속 소유한다.
