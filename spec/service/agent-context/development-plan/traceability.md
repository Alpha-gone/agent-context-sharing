# 개발 작업 추적 계획

[전체 계획](../DEVELOPMENT_PLAN.md) · [기준 SDD](../sdd/traceability.md) · [SRS](../SRS.md)

## 목표와 구현 범위

확정 요구사항 184건을 신규 개발 작업과 인수 작업에 연결하고 누락·미정·완료 근거를 관리한다. 이 문서의 작업 표는 설계 추적표를 대체하지 않는다. 추적 작업 중 TRACE-01의 기준·범위 확정, TRACE-02의 요구사항·작업·선행 관계 대조와 TRACE-03의 미정·조건부 채택 범위 점검을 완료했다. TRACE-04~05는 미착수이며, 다른 분야의 현재 구현·인수 상태는 전체 계획에서 구분한다.

- 설계 입력: [요구사항 추적표](../sdd/traceability.md#요구사항-추적표), [설계 미정](../sdd/traceability.md#설계-미정-사항), [해소 선택](../sdd/traceability.md#해소한-설계-선택), [SRS 미정](../SRS.md#미정-사항).
- 예정 산출물: 요구사항→SDD→개발 작업→인수 증거의 대응, 미정 해소 조건과 상태·DOX 점검.
- 다른 분야와의 경계: 요구사항은 SRS, 상세 계약·미정 사항은 SDD, 실행 증거는 각 분야 시험·검토 기록이 소유한다.

## 개발 작업

### TRACE-01

- [x] **신규 구현의 기준과 범위를 고정한다.**
- 선행: 사용자 요청과 현재 SRS·분야별 SDD.

1. FR 001~175 중 폐기된 156~159를 제외한 171건과 NFR 001~013의 13건을 기준 집합으로 고정한다.
2. 기존 소스·테스트·완료 표시·과거 평가 수치를 완료 근거에서 제외하고 채택된 목표·설계 선택을 구현 입력으로 구분한다.
3. 문서의 현재 구현 설명을 신규 범위 제외 근거로 쓰지 않는다. 목표 MCP·OAuth·등록·진단·새 평가도 모두 신규 구현 대상으로 둔다.

- 산출물: 기준 문서 판·요구사항 집합·기존 이력과 신규 실행 증거의 구분.
- 완료 기준: 설계 채택과 현재 코드 존재를 혼동하지 않고 새 실행 범위를 확정한다. 요구사항 ID·상태를 재번호화하지 않는다.

#### 기준 확정과 완료 근거

- 확정일: 2026-10-14. 사용자가 현재 내용을 신규 구현 기준으로 확정하도록 지시했다.
- 기준 문서 판: 커밋 `b82bf51c97da6d2eb82a4f1df45caed7f80a74a4`의 [SRS](../SRS.md), [SDD 진입점](../SDD.md)과 연결된 14개 분야별 상세 문서, [용어 정리집](../../../DICTIONARY.md). 확정 시 작업 트리의 해당 문서는 이 커밋과 동일함을 확인했다.
- 기준 집합: `FR-AGENT_CONTEXT-001`~`175` 중 폐기된 `156`~`159`를 제외한 171건과 `NFR-AGENT_CONTEXT-001`~`013`의 13건, 총 184건. SRS의 요구사항 집합·설계 추적표·개발 대응표를 대조해 누락·추가가 없고 두 추적표에 중복 행이 없음을 확인했다.
- 신규 실행 범위: 기준 문서의 확정 요구사항과 채택된 설계 전체. 표준 MCP·OAuth·회전형 갱신·공통 등록·진단 파일·새 평가를 포함한다. 기존 구현 설명·소스·테스트·완료 표시·과거 측정은 작업 생략이나 신규 완료 근거로 사용하지 않는다. 기존 코드·데이터의 삭제나 운영 전환은 이 확정에 포함하지 않는다.
- 미정·실증 경계: 현재 SRS·SDD의 상태를 유지한다. TBD-048·049·052·060·061과 RLS·플랜 이름/값은 임의로 확정하지 않으며, 해소된 TBD-063의 계약과 TBD-064의 실제 SDK·Codex CLI 검증 대기를 구분한다.
- 완료 판정: 사용자 확정 지시와 위 문서·집합 대조를 TRACE-01의 새 완료 근거로 사용한다. 코드 구현·실행 인수 완료를 뜻하지 않으며, 다음 구현 항목은 [ARCH-01](architecture.md#arch-01)이다.

### TRACE-02

- [x] **요구사항·작업·선행 관계를 유지한다.**
- 선행: [TRACE-01](#trace-01).

1. 아래 대응표의 각 요구사항에 구현 작업과 인수 작업을 연결하고 여러 분야 작업이 만나는 계약을 확인한다.
2. 주 책임 SDD는 기존 추적표의 소유를 유지한다. 실제 구현 패키지나 실행 작업이 다르더라도 설계 소유권을 옮기지 않는다.
3. 작업 추가·분리·삭제 때 진입점·앵커·선행 관계·단계·대응표를 함께 갱신하고 선행 순환·누락을 검사한다.

- 산출물: 184건 작업 대응과 유효한 작업 의존 관계.
- 완료 기준: 모든 요구사항이 정확히 한 행에 있고 각 링크가 실제 개발·인수 작업을 가리킨다. 이 대응표 자체의 존재는 구현 완료 증거가 아니다.

#### 새 대조와 완료 근거

2026-10-27 [TRACE-02 검토](../reviews/2026-10-27-trace-02.md)에서 현재 SRS·SDD 추적표·개발 대응표의 FR 171건/NFR 13건, 총 184건이 각각 정확히 한 행이며 집합·주 책임 링크가 일치하는지 대조했다. 구현 링크 286개와 인수 링크 265개는 모두 실제 작업의 파일·앵커를 가리킨다. 주 책임 설계나 요구사항을 변경하지 않았다.

124개 작업과 선행 링크 341개에 중복 ID·잘못된 대상·순환이 없고 모든 작업이 TRACE-01로 이어진다. 완료 작업에 미완료 선행이 없으며 단계 범위를 풀어 대조한 124개 작업에도 누락·중복이 없다. 직접 대응된 101개 작업 외 공통 기반·추적 작업 23개는 선행 관계와 단계에서 확인했다. 18개 문서의 로컬 링크 1,795개와 Markdown 구조 검사를 통과했다.

이 결과로 TRACE-02를 완료한다. 문서의 추적 관계 검증이며 새 런타임·브라우저·호스트·정량 효과 인수로 확대하지 않는다. 다음 독립 작업은 TRACE-03이며 TEST-06의 수동 인수와 TEST-09·TRACE-04~05는 남아 있다.

### TRACE-03

- [x] **미정과 조건부 채택의 실행 경계를 관리한다.**
- 선행: [TRACE-01](#trace-01).

1. TBD-048·049·052·060·061의 인증 고도화·계정 소멸·운영자 역할·CIMD·형태소 분석은 기존 미정 상태로 둔다. RLS를 새로 도입하거나 플랜 이름·계정별 운영 값을 명세에 고정하지 않는다. SRS가 확정한 14개 플랜 항목·기본값과 운영 구성의 덮어쓰기는 유지한다.
2. TBD-063의 갱신 수명 선택은 해소된 계약으로 구현하고 실제 시험 대기 때문에 선택을 다시 미정으로 만들지 않는다.
3. TBD-064의 SDK 실제 빌드·선정 CLI 인수와 해소 범위를 보존한다. 다른 호스트/저장 모드의 지원과 새 선택이 필요하면 소유 SDD에서 기존 번호 체계를 따라 기록한다.
4. 근거 선택·적응형·색인 대상·모델/압축·그래프 단계의 운영 승격은 새 통제 측정의 조건을 충족한 뒤 결정한다.

- 산출물: 미정별 구현 범위·해소 조건과 조건부 채택의 판정 연결.
- 완료 기준: 미확정 기능이 확정 작업에 숨어 들어가지 않고 실제 지원·효과 없는 항목이 완료로 승격되지 않는다.

#### 범위 대조와 완료 근거

2026-10-28 [TRACE-03 검토](../reviews/2026-10-28-trace-03.md)에서 요구사항 미정 048·049·052·060·061의 현재 최소 계약과 결정 시점을 SRS·SDD·진입점·구성·스키마에 대조했다. 다섯 건을 미정으로 유지하고 RLS·운영 플랜의 임의 채택이 없는지 점검했다. 플랜의 확정 기본값과 운영 값의 미고정을 혼동하지 않도록 작업 표현을 명확히 했다.

TBD-063은 1시간 접근·30일 절대/7일 비활동·공유 회전/폐기·보존/정리 계약과 구현을, TBD-064는 SDK v1.8.0·MCP 2026-07-28·macOS CLI 0.160.1 exec·기능 활성·파일 저장의 실제 인수 근거를 확인해 해소 상태를 유지한다. 다른 호스트 표면이나 저장 모드로 지원을 확대하지 않았다.

근거 선택·적응형 라우팅·색인 대상·모델/압축·그래프 단계의 비교 구현과 운영 승격을 분리했다. 예시 구성의 baseline·all_layers·기존 임베딩 구성과 두 선택 기능의 기본 비활성, 평가 결과의 default_activation=false를 확인했다. 이 정적 대조로 TRACE-03을 완료하며 미정 해소·새 런타임 인수·운영 채택으로 해석하지 않는다. TEST-06의 수동 인수가 남아 있고 TEST-09 뒤 TRACE-04~05를 진행한다.

### TRACE-04

- [ ] **새 실행 증거와 완료 상태를 연결한다.**
- 선행: [TRACE-02](#trace-02), [TEST-09](testing.md#test-09).

1. 각 작업의 산출물·실행 판·명령·결과·표본·미실행·건너뜀·실패를 저장소 검토 기록에 연결한다.
2. 요구사항의 코드/서버 계약·실제 호스트·파일 저장·정량 효과·운영 준비를 별도 상태로 확인한다.
3. 필수 인수·실증이 남은 작업은 완료 체크하지 않는다. 문서 검증 통과와 기능 통과를 구분한다.

- 산출물: 신규 작업 완료 근거와 요구사항별 실행 상태.
- 완료 기준: 모든 완료 표시에 새 산출물과 해당 완료 기준의 관찰 증거가 있으며 과거 측정·다른 표면의 지원으로 대체하지 않는다.

### TRACE-05

- [ ] **최종 정합성·DOX·안내를 점검한다.**
- 선행: [TRACE-03](#trace-03), [TRACE-04](#trace-04).

1. SRS→분야별 SDD→분야별 계획→시험/실증의 경로·책임·상태를 대조한다.
2. 변경한 계약·구성·지속 구조의 소유 DOX·부모/자식 색인·용어·README/OPERATIONS를 정합화한다.
3. 한국어 표현·맞춤법·표·코드 블록·상대 링크·작업 앵커와 요구사항 집합을 검사한다.

- 산출물: 최종 문서·상태·DOX 점검 결과와 남은 조건.
- 완료 기준: 현재 상태와 모순되는 안내·완료 표시가 없고 의도적으로 유지한 문서·미실행 인수의 이유를 보고한다.

## 요구사항별 개발·인수 작업 대응

아래 주 책임 링크는 SDD 추적표의 링크를 그대로 따른다. 구현 작업은 그 계약을 새로 작성할 작업이고 인수 작업은 실행 증거를 확인할 책임이다. 세부 기대 결과는 각 작업의 완료 기준과 주 책임 SDD의 검증 방법을 함께 사용한다.

| 요구사항 ID | 주 책임 SDD·기준 | 신규 구현 작업 | 인수 작업 |
|---|---|---|---|
| `FR-AGENT_CONTEXT-001` | [외부 경계](../sdd/architecture.md#외부-경계) | [ARCH-03](architecture.md#arch-03) | [ARCH-08](architecture.md#arch-08) |
| `FR-AGENT_CONTEXT-002` | [외부 경계](../sdd/architecture.md#외부-경계) | [ARCH-07](architecture.md#arch-07), [MCP-02](mcp.md#mcp-02) | [TEST-02](testing.md#test-02), [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-003` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [DATA-01](data.md#data-01) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-004` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [ARCH-02](architecture.md#arch-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-005` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [DATA-03](data.md#data-03), [LIFE-04](context-lifecycle.md#life-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-006` | [파생 생성과 근거 무효화](../sdd/context-lifecycle.md#파생-생성과-근거-무효화) | [LIFE-03](context-lifecycle.md#life-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-007` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-07](search.md#search-07) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-008` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-03](search.md#search-03) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-009` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [LIFE-04](context-lifecycle.md#life-04), [DATA-03](data.md#data-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-010` | [폐기와 복구의 채널](../sdd/context-lifecycle.md#폐기와-복구의-채널) | [LIFE-07](context-lifecycle.md#life-07), [MCP-06](mcp.md#mcp-06) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-011` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [ARCH-02](architecture.md#arch-02), [LIFE-02](context-lifecycle.md#life-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-012` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-07](mcp.md#mcp-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-013` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-07](search.md#search-07) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-014` | [화면 배치](../sdd/web.md#화면-배치) | [WEB-06](web.md#web-06) | [TEST-06](testing.md#test-06) |
| `FR-AGENT_CONTEXT-015` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-02](authorization.md#perm-02) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-016` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-02](authorization.md#perm-02) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-017` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-07](search.md#search-07) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-018` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-08](search.md#search-08) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-019` | [페이지 처리](../sdd/mcp.md#페이지-처리) | [MCP-07](mcp.md#mcp-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-020` | [트랜잭션 경계](../sdd/context-lifecycle.md#트랜잭션-경계) | [LIFE-01](context-lifecycle.md#life-01) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-021` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-07](mcp.md#mcp-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-022` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-06](mcp.md#mcp-06), [LIFE-01](context-lifecycle.md#life-01) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-023` | [트랜잭션 경계](../sdd/context-lifecycle.md#트랜잭션-경계) | [LIFE-02](context-lifecycle.md#life-02), [IDX-01](indexing.md#idx-01) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-024` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-07](mcp.md#mcp-07), [SEARCH-04](search.md#search-04) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-025` | [트랜잭션 경계](../sdd/context-lifecycle.md#트랜잭션-경계) | [LIFE-03](context-lifecycle.md#life-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-026` | [요청 처리 순서](../sdd/mcp.md#요청-처리-순서) | [MCP-03](mcp.md#mcp-03), [MCP-08](mcp.md#mcp-08) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-027` | [OAuth discovery와 최초 인가](../sdd/authentication.md#oauth-discovery와-최초-인가) | [AUTH-07](authentication.md#auth-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-028` | [호스트 직접 연결 전환 설계](../sdd/architecture.md#호스트-직접-연결-전환-설계) | [ARCH-07](architecture.md#arch-07), [AUTH-08](authentication.md#auth-08) | [TEST-02](testing.md#test-02), [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-029` | [SRS 토큰 갱신](../SRS.md#토큰-갱신) | [AUTH-08](authentication.md#auth-08) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-030` | [호스트 직접 연결 전환 설계](../sdd/architecture.md#호스트-직접-연결-전환-설계) | [AUTH-09](authentication.md#auth-09) | [TEST-02](testing.md#test-02), [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-031` | [소프트 삭제 수명주기](../sdd/context-lifecycle.md#소프트-삭제-수명주기) | [LIFE-06](context-lifecycle.md#life-06), [PERM-02](authorization.md#perm-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-032` | [소프트 삭제 수명주기](../sdd/context-lifecycle.md#소프트-삭제-수명주기) | [LIFE-06](context-lifecycle.md#life-06), [PERM-04](authorization.md#perm-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-033` | [소프트 삭제 수명주기](../sdd/context-lifecycle.md#소프트-삭제-수명주기) | [OPS-04](operations.md#ops-04), [LIFE-06](context-lifecycle.md#life-06) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-034` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [ARCH-02](architecture.md#arch-02), [LIFE-02](context-lifecycle.md#life-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-035` | [물리 배치](../sdd/data.md#물리-배치) | [ARCH-02](architecture.md#arch-02) | [TEST-02](testing.md#test-02), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-036` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [LIFE-03](context-lifecycle.md#life-03), [ARCH-02](architecture.md#arch-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-037` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-02](authorization.md#perm-02), [MCP-04](mcp.md#mcp-04) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-038` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-02](authorization.md#perm-02) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-039` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-02](authorization.md#perm-02) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-040` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-02](authorization.md#perm-02) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-041` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-03](authorization.md#perm-03) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-042` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-03](authorization.md#perm-03) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-043` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-04](authorization.md#perm-04) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-044` | [동시 갱신](../sdd/context-lifecycle.md#동시-갱신) | [LIFE-09](context-lifecycle.md#life-09), [MCP-06](mcp.md#mcp-06) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-045` | [동시 갱신](../sdd/context-lifecycle.md#동시-갱신) | [LIFE-03](context-lifecycle.md#life-03), [LIFE-09](context-lifecycle.md#life-09) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-046` | [사건 재구성](../sdd/context-lifecycle.md#사건-재구성) | [LIFE-04](context-lifecycle.md#life-04), [LIFE-08](context-lifecycle.md#life-08) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-047` | [감사 기록의 조회](../sdd/observability.md#감사-기록의-조회) | [OBS-06](observability.md#obs-06), [LIFE-08](context-lifecycle.md#life-08) | [OBS-08](observability.md#obs-08), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-048` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-07](search.md#search-07), [SEARCH-02](search.md#search-02) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-049` | [관계형 스키마](../sdd/data.md#관계형-스키마) | [LIFE-01](context-lifecycle.md#life-01) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-050` | [페이지 처리](../sdd/mcp.md#페이지-처리) | [MCP-07](mcp.md#mcp-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-051` | [요청 처리 순서](../sdd/mcp.md#요청-처리-순서) | [MCP-08](mcp.md#mcp-08), [PERM-02](authorization.md#perm-02) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-052` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-07](mcp.md#mcp-07), [SEARCH-04](search.md#search-04) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-053` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-07](mcp.md#mcp-07), [SEARCH-04](search.md#search-04) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-054` | [응답 직렬화](../sdd/mcp.md#응답-직렬화) | [MCP-07](mcp.md#mcp-07), [SEARCH-05](search.md#search-05) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-055` | [요청 처리 순서](../sdd/mcp.md#요청-처리-순서) | [MCP-03](mcp.md#mcp-03), [PERM-05](authorization.md#perm-05) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-056` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-03](mcp.md#mcp-03) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-057` | [오류의 생성과 전달](../sdd/mcp.md#오류의-생성과-전달) | [MCP-08](mcp.md#mcp-08) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-058` | [입력 검증](../sdd/mcp.md#입력-검증) | [MCP-03](mcp.md#mcp-03) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-059` | [관계 후보 제안과 확정](../sdd/context-lifecycle.md#관계-후보-제안과-확정) | [LIFE-04](context-lifecycle.md#life-04), [LIFE-05](context-lifecycle.md#life-05) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-060` | [사건 재구성](../sdd/context-lifecycle.md#사건-재구성) | [LIFE-04](context-lifecycle.md#life-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-061` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [EVAL-13](evaluation.md#eval-13) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-062` | [파생 생성과 근거 무효화](../sdd/context-lifecycle.md#파생-생성과-근거-무효화) | [LIFE-02](context-lifecycle.md#life-02), [ARCH-02](architecture.md#arch-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-063` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-08](search.md#search-08) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-064` | [파생 생성과 근거 무효화](../sdd/context-lifecycle.md#파생-생성과-근거-무효화) | [LIFE-03](context-lifecycle.md#life-03), [SEARCH-07](search.md#search-07) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-065` | [부가 정보의 출처](../sdd/mcp.md#부가-정보의-출처) | [MCP-08](mcp.md#mcp-08) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-066` | [장애 시의 동작](../sdd/operations.md#장애-시의-동작) | [OPS-09](operations.md#ops-09) | [OPS-09](operations.md#ops-09), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-067` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [SEARCH-02](search.md#search-02), [ARCH-02](architecture.md#arch-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-068` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [SEARCH-02](search.md#search-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-069` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-06](mcp.md#mcp-06), [LIFE-07](context-lifecycle.md#life-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-070` | [파생 생성과 근거 무효화](../sdd/context-lifecycle.md#파생-생성과-근거-무효화) | [LIFE-03](context-lifecycle.md#life-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-071` | [관계형 스키마](../sdd/data.md#관계형-스키마) | [OBS-06](observability.md#obs-06), [LIFE-08](context-lifecycle.md#life-08) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-072` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [LIFE-04](context-lifecycle.md#life-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-073` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [LIFE-04](context-lifecycle.md#life-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-074` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [LIFE-04](context-lifecycle.md#life-04), [SEARCH-04](search.md#search-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-075` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-06](mcp.md#mcp-06), [LIFE-04](context-lifecycle.md#life-04) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-076` | [사건 재구성](../sdd/context-lifecycle.md#사건-재구성) | [LIFE-04](context-lifecycle.md#life-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-077` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [ARCH-02](architecture.md#arch-02), [LIFE-03](context-lifecycle.md#life-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-078` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [ARCH-02](architecture.md#arch-02), [LIFE-03](context-lifecycle.md#life-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-079` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [SEARCH-07](search.md#search-07), [LIFE-03](context-lifecycle.md#life-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-080` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-03](search.md#search-03) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-081` | [컨텍스트 저장과 색인](../sdd/indexing.md#컨텍스트-저장과-색인) | [IDX-01](indexing.md#idx-01), [IDX-05](indexing.md#idx-05) | [TEST-03](testing.md#test-03), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-082` | [연산 계약](../sdd/mcp.md#연산-계약) | [MCP-07](mcp.md#mcp-07), [SEARCH-01](search.md#search-01) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-083` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-08](search.md#search-08) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-084` | [관계 후보 제안과 확정](../sdd/context-lifecycle.md#관계-후보-제안과-확정) | [LIFE-05](context-lifecycle.md#life-05) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-085` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-07](search.md#search-07) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-086` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-07](search.md#search-07) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-087` | [지표 수집 지점](../sdd/observability.md#지표-수집-지점) | [OBS-05](observability.md#obs-05), [SEARCH-02](search.md#search-02) | [OBS-08](observability.md#obs-08), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-088` | [응답 직렬화](../sdd/mcp.md#응답-직렬화) | [MCP-07](mcp.md#mcp-07), [SEARCH-07](search.md#search-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-089` | [근거 경로 보존 선택](../sdd/search.md#근거-경로-보존-선택) | [SEARCH-07](search.md#search-07), [SEARCH-09](search.md#search-09) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-090` | [토큰 검증](../sdd/authentication.md#토큰-검증) | [AUTH-08](authentication.md#auth-08) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-091` | [등급 검사](../sdd/authorization.md#등급-검사) | [PERM-02](authorization.md#perm-02), [AUTH-08](authentication.md#auth-08) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-092` | [호스트 직접 연결 전환 설계](../sdd/architecture.md#호스트-직접-연결-전환-설계) | [AUTH-09](authentication.md#auth-09) | [TEST-02](testing.md#test-02), [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-093` | [호스트 직접 연결 전환 설계](../sdd/architecture.md#호스트-직접-연결-전환-설계) | [AUTH-08](authentication.md#auth-08) | [TEST-02](testing.md#test-02), [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-094` | [호스트 직접 연결 전환 설계](../sdd/architecture.md#호스트-직접-연결-전환-설계) | [ARCH-08](architecture.md#arch-08) | [TEST-02](testing.md#test-02), [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-095` | [표준 MCP 경계](../sdd/mcp.md#표준-mcp-경계) | [MCP-02](mcp.md#mcp-02) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-096` | [호스트 직접 연결 전환 설계](../sdd/architecture.md#호스트-직접-연결-전환-설계) | [ARCH-08](architecture.md#arch-08), [ARCH-06](architecture.md#arch-06), [OPS-02](operations.md#ops-02) | [TEST-02](testing.md#test-02), [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-097` | [색인 작업 상태](../sdd/indexing.md#색인-작업-상태) | [IDX-04](indexing.md#idx-04) | [TEST-03](testing.md#test-03), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-098` | [임베딩 스키마](../sdd/data.md#임베딩-스키마) | [IDX-07](indexing.md#idx-07), [DATA-07](data.md#data-07) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-099` | [로그에서 제외하는 것](../sdd/observability.md#로그에서-제외하는-것) | [OBS-01](observability.md#obs-01), [OBS-08](observability.md#obs-08) | [OBS-08](observability.md#obs-08), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-100` | [물리 배치](../sdd/data.md#물리-배치) | [DATA-02](data.md#data-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-101` | [격리 강제의 구현](../sdd/data.md#격리-강제의-구현) | [DATA-04](data.md#data-04), [TEST-04](testing.md#test-04) | [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-102` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [DATA-03](data.md#data-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-103` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [DATA-01](data.md#data-01) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-104` | [임베딩 스키마](../sdd/data.md#임베딩-스키마) | [DATA-07](data.md#data-07), [SEARCH-02](search.md#search-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-105` | [화면 배치](../sdd/web.md#화면-배치) | [WEB-03](web.md#web-03), [WEB-09](web.md#web-09) | [TEST-06](testing.md#test-06) |
| `FR-AGENT_CONTEXT-106` | [화면 배치](../sdd/web.md#화면-배치) | [WEB-04](web.md#web-04) | [TEST-06](testing.md#test-06) |
| `FR-AGENT_CONTEXT-107` | [화면 배치](../sdd/web.md#화면-배치) | [WEB-05](web.md#web-05) | [TEST-06](testing.md#test-06) |
| `FR-AGENT_CONTEXT-108` | [복구](../sdd/operations.md#복구) | [OPS-06](operations.md#ops-06), [WEB-05](web.md#web-05) | [TEST-03](testing.md#test-03), [WEB-09](web.md#web-09) |
| `FR-AGENT_CONTEXT-109` | [감사 기록의 조회](../sdd/observability.md#감사-기록의-조회) | [OBS-06](observability.md#obs-06), [LIFE-08](context-lifecycle.md#life-08) | [OBS-08](observability.md#obs-08), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-110` | [시각 표현 규칙](../sdd/web.md#시각-표현-규칙) | [WEB-06](web.md#web-06) | [TEST-06](testing.md#test-06) |
| `FR-AGENT_CONTEXT-111` | [시각 표현 규칙](../sdd/web.md#시각-표현-규칙) | [WEB-07](web.md#web-07), [TEST-06](testing.md#test-06) | [TEST-06](testing.md#test-06) |
| `FR-AGENT_CONTEXT-112` | [비그래프 기준선](../sdd/evaluation.md#비그래프-기준선) | [EVAL-05](evaluation.md#eval-05) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-113` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [EVAL-04](evaluation.md#eval-04), [SEARCH-07](search.md#search-07) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-114` | [비그래프 기준선](../sdd/evaluation.md#비그래프-기준선) | [EVAL-05](evaluation.md#eval-05) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-115` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [EVAL-07](evaluation.md#eval-07) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-116` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [EVAL-12](evaluation.md#eval-12) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-117` | [계정 플랜 값](../sdd/authorization.md#계정-플랜-값) | [PERM-01](authorization.md#perm-01) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-118` | [계정 플랜 값](../sdd/authorization.md#계정-플랜-값) | [PERM-05](authorization.md#perm-05) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-119` | [계정 플랜 값](../sdd/authorization.md#계정-플랜-값) | [PERM-05](authorization.md#perm-05) | [TEST-03](testing.md#test-03), [TEST-04](testing.md#test-04) |
| `FR-AGENT_CONTEXT-120` | [관계형 스키마](../sdd/data.md#관계형-스키마) | [OPS-04](operations.md#ops-04), [OBS-06](observability.md#obs-06) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-121` | [임베딩 스키마](../sdd/data.md#임베딩-스키마) | [DATA-07](data.md#data-07) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-122` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [EVAL-05](evaluation.md#eval-05), [IDX-07](indexing.md#idx-07) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-123` | [저장 계층화](../sdd/data.md#저장-계층화) | [DATA-08](data.md#data-08), [ARCH-04](architecture.md#arch-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-124` | [기억 오염 방어](../sdd/context-lifecycle.md#기억-오염-방어) | [LIFE-02](context-lifecycle.md#life-02), [ARCH-02](architecture.md#arch-02) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-125` | [기억 오염 방어](../sdd/context-lifecycle.md#기억-오염-방어) | [LIFE-08](context-lifecycle.md#life-08), [SEARCH-07](search.md#search-07) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-126` | [사후 탐지](../sdd/context-lifecycle.md#사후-탐지) | [LIFE-08](context-lifecycle.md#life-08) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-127` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-07](search.md#search-07), [SEARCH-09](search.md#search-09) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-128` | [검색 채널 실행기](../sdd/search.md#검색-채널-실행기) | [SEARCH-02](search.md#search-02), [SEARCH-03](search.md#search-03) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-129` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-02](search.md#search-02) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-130` | [인가 서버](../sdd/authentication.md#인가-서버) | [AUTH-03](authentication.md#auth-03) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-131` | [인가 서버](../sdd/authentication.md#인가-서버) | [AUTH-04](authentication.md#auth-04), [AUTH-01](authentication.md#auth-01), [WEB-02](web.md#web-02) | [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-132` | [소프트 삭제 수명주기](../sdd/context-lifecycle.md#소프트-삭제-수명주기) | [LIFE-06](context-lifecycle.md#life-06) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-133` | [웹 요청 처리 순서](../sdd/web.md#웹-요청-처리-순서) | [WEB-02](web.md#web-02), [AUTH-05](authentication.md#auth-05) | [TEST-06](testing.md#test-06) |
| `FR-AGENT_CONTEXT-134` | [세션 쿠키 검증](../sdd/authentication.md#세션-쿠키-검증) | [AUTH-05](authentication.md#auth-05), [AUTH-08](authentication.md#auth-08) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-135` | [복구](../sdd/operations.md#복구) | [OPS-06](operations.md#ops-06), [WEB-05](web.md#web-05) | [TEST-03](testing.md#test-03), [WEB-09](web.md#web-09) |
| `FR-AGENT_CONTEXT-136` | [웹 화면 계약](../sdd/web.md#웹-화면-계약) | [WEB-05](web.md#web-05) | [TEST-06](testing.md#test-06) |
| `FR-AGENT_CONTEXT-137` | [복구](../sdd/operations.md#복구) | [OPS-06](operations.md#ops-06), [WEB-03](web.md#web-03) | [TEST-03](testing.md#test-03), [WEB-09](web.md#web-09) |
| `FR-AGENT_CONTEXT-138` | [작업 컨텍스트 흐름 조회](../sdd/search.md#작업-컨텍스트-흐름-조회) | [SEARCH-01](search.md#search-01) | [TEST-02](testing.md#test-02), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-139` | [입력 검증](../sdd/mcp.md#입력-검증) | [MCP-03](mcp.md#mcp-03) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-140` | [배포 구성](../sdd/operations.md#배포-구성) | [OPS-03](operations.md#ops-03), [OPS-09](operations.md#ops-09) | [OPS-09](operations.md#ops-09), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-141` | [임베딩 제공자 전환](../sdd/indexing.md#임베딩-제공자-전환) | [IDX-03](indexing.md#idx-03) | [TEST-03](testing.md#test-03), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-142` | [관계 후보 제안과 확정](../sdd/context-lifecycle.md#관계-후보-제안과-확정) | [OPS-04](operations.md#ops-04), [LIFE-05](context-lifecycle.md#life-05) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-143` | [페이지 처리](../sdd/mcp.md#페이지-처리) | [MCP-07](mcp.md#mcp-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-144` | [테이블 열 정의](../sdd/data.md#테이블-열-정의) | [PERM-04](authorization.md#perm-04) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-145` | [그래프 스키마](../sdd/data.md#그래프-스키마) | [LIFE-03](context-lifecycle.md#life-03) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-146` | [컨텍스트 저장과 색인](../sdd/indexing.md#컨텍스트-저장과-색인) | [LIFE-02](context-lifecycle.md#life-02), [IDX-01](indexing.md#idx-01) | [TEST-03](testing.md#test-03), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-147` | [OAuth discovery와 최초 인가](../sdd/authentication.md#oauth-discovery와-최초-인가) | [AUTH-06](authentication.md#auth-06), [AUTH-07](authentication.md#auth-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-148` | [OAuth discovery와 최초 인가](../sdd/authentication.md#oauth-discovery와-최초-인가) | [AUTH-07](authentication.md#auth-07) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-149` | [표준 MCP 경계](../sdd/mcp.md#표준-mcp-경계) | [MCP-02](mcp.md#mcp-02) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-150` | [근거 경로 보존 선택](../sdd/search.md#근거-경로-보존-선택) | [SEARCH-09](search.md#search-09), [EVAL-05](evaluation.md#eval-05) | [TEST-02](testing.md#test-02), [EVAL-13](evaluation.md#eval-13) |
| `FR-AGENT_CONTEXT-151` | [질의 적응형 라우팅](../sdd/search.md#질의-적응형-라우팅) | [SEARCH-08](search.md#search-08), [EVAL-06](evaluation.md#eval-06) | [TEST-02](testing.md#test-02), [EVAL-13](evaluation.md#eval-13) |
| `FR-AGENT_CONTEXT-152` | [요청 단위 일관 읽기 구현 비교](../sdd/search.md#요청-단위-일관-읽기-구현-비교) | [SEARCH-01](search.md#search-01), [SEARCH-06](search.md#search-06), [EVAL-06](evaluation.md#eval-06) | [TEST-05](testing.md#test-05), [EVAL-13](evaluation.md#eval-13) |
| `FR-AGENT_CONTEXT-153` | [그래프 불변식 규칙과 감사](../sdd/data.md#그래프-불변식-규칙과-감사) | [DATA-06](data.md#data-06), [DATA-05](data.md#data-05) | [TEST-03](testing.md#test-03) |
| `FR-AGENT_CONTEXT-154` | [관계·경로 오염 적대적 평가와 홉별 진단](../sdd/testing.md#관계경로-오염-적대적-평가와-홉별-진단) | [EVAL-08](evaluation.md#eval-08), [TEST-03](testing.md#test-03) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-155` | [호스트 직접 연결 전환 설계](../sdd/architecture.md#호스트-직접-연결-전환-설계) | [MCP-05](mcp.md#mcp-05), [MCP-08](mcp.md#mcp-08) | [TEST-02](testing.md#test-02), [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-160` | [검색 단위 완전성 비교](../sdd/evaluation.md#검색-단위-완전성-비교) | [EVAL-09](evaluation.md#eval-09), [EVAL-02](evaluation.md#eval-02) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-161` | [외부 원본 재현성 비교](../sdd/evaluation.md#외부-원본-재현성-비교) | [EVAL-10](evaluation.md#eval-10) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-162` | [임베딩 순위와 임계값 비교](../sdd/evaluation.md#임베딩-순위와-임계값-비교) | [EVAL-11](evaluation.md#eval-11), [IDX-07](indexing.md#idx-07) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-163` | [에이전트 업무 비용과 축소 비교](../sdd/evaluation.md#에이전트-업무-비용과-축소-비교) | [EVAL-12](evaluation.md#eval-12), [EVAL-04](evaluation.md#eval-04) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-164` | [호스트 직접 연결 전환 설계](../sdd/architecture.md#호스트-직접-연결-전환-설계) | [ARCH-08](architecture.md#arch-08), [MCP-09](mcp.md#mcp-09) | [TEST-08](testing.md#test-08) |
| `FR-AGENT_CONTEXT-165` | [SDK와 수명주기](../sdd/mcp.md#sdk와-수명주기) | [MCP-09](mcp.md#mcp-09), [MCP-02](mcp.md#mcp-02) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-166` | [OAuth discovery와 최초 인가](../sdd/authentication.md#oauth-discovery와-최초-인가) | [AUTH-07](authentication.md#auth-07), [AUTH-06](authentication.md#auth-06) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-167` | [갱신 자격의 수명과 정리](../sdd/authentication.md#갱신-자격의-수명과-정리) | [AUTH-09](authentication.md#auth-09), [AUTH-10](authentication.md#auth-10), [OPS-04](operations.md#ops-04) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-168` | [첫 Codex CLI 상호운용 시험](../sdd/testing.md#첫-codex-cli-상호운용-시험) | [TEST-08](testing.md#test-08) | [TEST-08](testing.md#test-08) |
| `FR-AGENT_CONTEXT-169` | [공통 행위자·도구·쓰기 안전성](../sdd/mcp.md#공통-행위자도구쓰기-안전성) | [MCP-04](mcp.md#mcp-04), [AUTH-10](authentication.md#auth-10) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-170` | [멱등성 저장과 경합](../sdd/mcp.md#멱등성-저장과-경합) | [MCP-05](mcp.md#mcp-05), [MCP-08](mcp.md#mcp-08) | [TEST-07](testing.md#test-07) |
| `FR-AGENT_CONTEXT-171` | [첫 Codex CLI 상호운용 시험](../sdd/testing.md#첫-codex-cli-상호운용-시험) | [TEST-08](testing.md#test-08) | [TEST-08](testing.md#test-08) |
| `FR-AGENT_CONTEXT-172` | [인수·마이그레이션·복귀](../sdd/testing.md#인수마이그레이션복귀) | [TEST-09](testing.md#test-09), [OPS-08](operations.md#ops-08) | [TEST-09](testing.md#test-09) |
| `FR-AGENT_CONTEXT-173` | [종류별 진단 로그](../sdd/observability.md#종류별-진단-로그) | [OBS-03](observability.md#obs-03), [OBS-02](observability.md#obs-02), [OBS-08](observability.md#obs-08) | [OBS-08](observability.md#obs-08), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-174` | [이벤트 외피와 흐름 추적](../sdd/observability.md#이벤트-외피와-흐름-추적) | [OBS-05](observability.md#obs-05), [IDX-01](indexing.md#idx-01) | [OBS-08](observability.md#obs-08), [TEST-05](testing.md#test-05) |
| `FR-AGENT_CONTEXT-175` | [회전·보존과 총상한](../sdd/observability.md#회전보존과-총상한) | [OBS-04](observability.md#obs-04), [OBS-07](observability.md#obs-07), [OBS-08](observability.md#obs-08) | [OBS-08](observability.md#obs-08), [TEST-05](testing.md#test-05) |
| `NFR-AGENT_CONTEXT-001` | [버전 요구](../sdd/architecture.md#버전-요구) | [ARCH-01](architecture.md#arch-01), [DATA-10](data.md#data-10) | [DATA-10](data.md#data-10), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-002` | [비기능 요구사항의 성립 근거](../sdd/architecture.md#비기능-요구사항의-성립-근거) | [LIFE-03](context-lifecycle.md#life-03), [LIFE-08](context-lifecycle.md#life-08) | [TEST-03](testing.md#test-03), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-003` | [설명 가능성](../sdd/evaluation.md#설명-가능성) | [SEARCH-07](search.md#search-07), [EVAL-04](evaluation.md#eval-04) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-004` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [EVAL-04](evaluation.md#eval-04) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-005` | [비그래프 기준선](../sdd/evaluation.md#비그래프-기준선) | [EVAL-05](evaluation.md#eval-05) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-006` | [비기능 요구사항의 성립 근거](../sdd/architecture.md#비기능-요구사항의-성립-근거) | [SEARCH-07](search.md#search-07), [EVAL-04](evaluation.md#eval-04) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-007` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [LIFE-08](context-lifecycle.md#life-08), [EVAL-07](evaluation.md#eval-07) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-008` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [EVAL-07](evaluation.md#eval-07) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-009` | [검증을 가능하게 하는 설계](../sdd/evaluation.md#검증을-가능하게-하는-설계) | [EVAL-12](evaluation.md#eval-12) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-010` | [감사 가능성](../sdd/evaluation.md#감사-가능성) | [LIFE-08](context-lifecycle.md#life-08), [EVAL-04](evaluation.md#eval-04) | [EVAL-13](evaluation.md#eval-13), [TEST-09](testing.md#test-09) |
| `NFR-AGENT_CONTEXT-011` | [비기능 요구사항의 성립 근거](../sdd/architecture.md#비기능-요구사항의-성립-근거) | [LIFE-09](context-lifecycle.md#life-09) | [TEST-03](testing.md#test-03) |
| `NFR-AGENT_CONTEXT-012` | [인스턴스 무상태성](../sdd/architecture.md#인스턴스-무상태성) | [ARCH-08](architecture.md#arch-08) | [ARCH-08](architecture.md#arch-08), [TEST-08](testing.md#test-08) |
| `NFR-AGENT_CONTEXT-013` | [갱신·폐기와 공유 저장](../sdd/authentication.md#갱신폐기와-공유-저장) | [AUTH-09](authentication.md#auth-09), [OBS-08](observability.md#obs-08), [OPS-03](operations.md#ops-03) | [TEST-05](testing.md#test-05), [TEST-08](testing.md#test-08) |
