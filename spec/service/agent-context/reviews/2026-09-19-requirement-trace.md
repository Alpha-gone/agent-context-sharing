# 요구사항 단위 구현 대조

## 문서 정보

- 문서 성격: 특정 시점의 구현 대조 기록이며 명세가 아니다. 계약은 `SRS.md`와 `SDD.md`가 소유한다.
- 검사일: 2026-09-19
- 검사 대상: `feature/phase-10-release-verification` 브랜치
- 검사 범위: `SRS.md`의 기능 요구사항 149건 전부. 비기능 요구사항 13건은 대상이 아니다.
- 추가 대조: `FR-AGENT_CONTEXT-150`~`154`는 12·13·15·16·17단계 구현 뒤 2026-09-27에 같은 방법으로 대조해 표에 더했다. 「결과 요약」의 건수는 최초 검사 149건 기준이다.
- 기준 문서: [SRS](../SRS.md), [SDD](../SDD.md), [개발 계획](../DEVELOPMENT_PLAN.md)
- 선행 검사: [출시 판정 추적성 감사](2026-09-19-release-audit.md)가 남긴 "요구사항 단위 확인" 한계를 닫기 위한 검사다.

## 검사 방법

선행 감사는 추적성을 설계 영역 14개 단위로 확인했다. 그래서 영역 안에서 개별 요구사항이 빠진 경우를 잡지 못했다. 이 검사는 단위를 요구사항 하나로 내린다.

요구사항마다 다음을 밟았다.

1. `SRS.md`의 요구사항 문장과 `SDD.md` 「요구사항 추적표」의 설계 요소를 읽는다.
2. 그 설계 요소를 실제로 수행하는 코드를 찾아 파일과 심볼로 적는다.
3. 찾은 코드가 요구사항 문장이 요구하는 동작을 하는지 확인한다.
4. 찾지 못하면 관련 이름과 구성 값을 전수 검색해 부재를 확인한다.

결함을 새로 찾는 검사가 아니다. 그 일은 [전체 소스 결함 검사](2026-09-18-source-audit.md)가 이미 했다. 여기서는 요구사항마다 대응하는 구현이 있는지만 본다.

## 결과 요약

| 상태 | 건수 | 뜻 |
|------|------|-----|
| 구현·측정 완료 | 147 | 요구사항이 요구하는 동작이 구현됐고 측정 요구가 있는 항목은 본 측정까지 끝났다 |
| 평가 대기 | 2 | 구현은 끝났으나 업무 효과 본 측정이 필요해 요구사항이 닫히지 않았다 |
| 미구현 | 0 | 대응하는 코드가 없는 요구사항은 없다 |

검사 뒤 `FR-AGENT_CONTEXT-122`의 등록 조건 분기와 `FR-AGENT_CONTEXT-123`의 저장 계층화를 구현했다. `FR-AGENT_CONTEXT-112`~`115`는 검색 품질·그래프 효과·지속 평가 본 측정으로 닫았고, `FR-AGENT_CONTEXT-116`은 `cmd/eval -business` 집계·판정 경로까지 구현했다. `FR-AGENT_CONTEXT-061`과 `FR-AGENT_CONTEXT-116`은 실제 통제 과업의 업무 효과 값이 있어야 닫힌다.

의도적 미구현으로 분류된 `TBD-AGENT_CONTEXT-048`·`-049`·`-052`·`-060`·`-061`은 기능 요구사항이 아니므로 이 표에 없다. 그 다섯 건은 선행 감사 6번이 확인했다.

## 대조표

| 요구사항 ID | 요구사항명 | 구현 위치 | 상태 |
|-------------|------------|-----------|------|
| `FR-AGENT_CONTEXT-001` | 핵심 기능 | `cmd/server/server.go`의 `application.handler`가 MCP·웹·인가 경로를 한 프로세스 경계로 묶는다 | 구현 |
| `FR-AGENT_CONTEXT-002` | 외부 연동 | `internal/mcp/transport.go`의 `Server.ServeHTTP`와 `ProtectedResourceMetadata` | 구현 |
| `FR-AGENT_CONTEXT-003` | 관계 관리 | `migrations/001_init.sql`의 간선 label 일곱 종과 `internal/store/relation.go`의 `relationLabel` | 구현 |
| `FR-AGENT_CONTEXT-004` | 정보 계층 | `internal/model/context.go`의 `Layer`와 `internal/store/agtype.go`의 `contextProperties` | 구현 |
| `FR-AGENT_CONTEXT-005` | 관계 관리 | `internal/store/relation.go`의 `createRelation`과 `relationLabel` | 구현 |
| `FR-AGENT_CONTEXT-006` | 수명주기 | `internal/store/context.go`의 `UpdateContextWithOperation`과 `invalidateDerivedEvidence` | 구현 |
| `FR-AGENT_CONTEXT-007` | 검색 | `internal/search/search.go`의 `Service.Flow` | 구현 |
| `FR-AGENT_CONTEXT-008` | 검색 | `internal/search/search.go`의 `combine` | 구현 |
| `FR-AGENT_CONTEXT-009` | 관계 관리 | `internal/store/context.go`의 `replaceEventMembers`와 `internal/model/context.go`의 `EventAttributes` | 구현 |
| `FR-AGENT_CONTEXT-010` | 수명주기 | `internal/store/operation.go`의 `HasAppliedDiscard`와 `internal/store/context.go`의 `changeContextDeletion` | 구현 |
| `FR-AGENT_CONTEXT-011` | 정보 계층 | `internal/model/context.go`의 `SourceAttributes`·`DerivedAttributes`·`EventAttributes` | 구현 |
| `FR-AGENT_CONTEXT-012` | 컨텍스트 조회 | `internal/mcp/handler.go`의 `contextFlow` | 구현 |
| `FR-AGENT_CONTEXT-013` | 업무 연속성 | `internal/mcp/handler.go`의 `flowValue`가 흐름과 메타데이터를 응답에 담는다 | 구현 |
| `FR-AGENT_CONTEXT-014` | 시각화 | `internal/web/web.go`의 `graphDetail`과 `newVisualizationData` | 구현 |
| `FR-AGENT_CONTEXT-015` | 공동 접근 | `internal/store/permission.go`의 `EffectiveGrade` | 구현 |
| `FR-AGENT_CONTEXT-016` | 컨텍스트 공유 | `internal/perm/perm.go`의 `Require`와 `internal/store/agtype.go`의 `parseContext` 안 `graph_id` 대조 | 구현 |
| `FR-AGENT_CONTEXT-017` | 협업 | `internal/search/search.go`의 `Service.Flow`가 기여 계정을 가리지 않고 그래프 단위로 채널을 돌린다 | 구현 |
| `FR-AGENT_CONTEXT-018` | 컨텍스트 흐름 | `internal/mcp/handler.go`의 `contextFlow`가 받는 `scope`와 `internal/search/search.go`의 `Input` | 구현 |
| `FR-AGENT_CONTEXT-019` | 그래프 목록 | `internal/store/graph.go`의 `ListGraphs`와 `internal/store/cursor.go`의 `encodeGraphCursor` | 구현 |
| `FR-AGENT_CONTEXT-020` | 그래프 생성 | `internal/store/graph.go`의 `CreateGraphWithOwner` | 구현 |
| `FR-AGENT_CONTEXT-021` | 그래프 조회 | `internal/mcp/handler.go`의 `getGraph` | 구현 |
| `FR-AGENT_CONTEXT-022` | 그래프 수정 | `internal/mcp/handler.go`의 `updateGraph`와 `internal/store/graph.go`의 `UpdateGraph` | 구현 |
| `FR-AGENT_CONTEXT-023` | 노드 생성 | `internal/store/context.go`의 `createContext` | 구현 |
| `FR-AGENT_CONTEXT-024` | 노드 조회 | `internal/mcp/handler.go`의 `getNode` | 구현 |
| `FR-AGENT_CONTEXT-025` | 노드 수정 | `internal/mcp/handler.go`의 `updateNode`와 `internal/store/context.go`의 `UpdateContextWithOperation` | 구현 |
| `FR-AGENT_CONTEXT-026` | 삭제 경계 | `internal/mcp/transport.go`의 `isWebOnlyTool` | 구현 |
| `FR-AGENT_CONTEXT-027` | 토큰 발급 | `internal/authz/authz.go`의 `Exchange`와 `internal/authz/http.go`의 `Token` | 구현 |
| `FR-AGENT_CONTEXT-028` | 요청 인증 | `internal/authz/authz.go`의 `Verify`와 `internal/mcp/transport.go`의 `bearerToken` | 구현 |
| `FR-AGENT_CONTEXT-029` | 토큰 만료 | `internal/authz/authz.go`의 `verifiedClaims` | 구현 |
| `FR-AGENT_CONTEXT-030` | 토큰 자동 갱신 | `internal/authz/authz.go`의 `VerifyAndRenew`와 `cmd/server/renewal.go` | 구현 |
| `FR-AGENT_CONTEXT-031` | 기본 보관 | `internal/store/permission.go`의 `cancelGraceForConnectedGraphs`. 활성 그래프에는 만료를 걸지 않는다 | 구현 |
| `FR-AGENT_CONTEXT-032` | 보관 유예 | `internal/store/permission.go`의 `startGraceForDisconnectedGraphs` | 구현 |
| `FR-AGENT_CONTEXT-033` | 소프트 삭제 | `internal/store/periodic.go`의 `ExpireGrace` | 구현 |
| `FR-AGENT_CONTEXT-034` | 정보 계층 | `internal/model/context.go`의 `Validate`와 `validateCommon` | 구현 |
| `FR-AGENT_CONTEXT-035` | 식별 | `internal/model/id.go`의 `NewID`와 `IsV7` | 구현 |
| `FR-AGENT_CONTEXT-036` | 불변성 | `internal/model/context.go`의 `ValidateUpdate` | 구현 |
| `FR-AGENT_CONTEXT-037` | 작업자 식별 | `internal/authz/authz.go`의 `Verify`가 푼 계정과 `created_by`·`created_by_agent` 분리 | 구현 |
| `FR-AGENT_CONTEXT-038` | 격리 | `internal/perm/perm.go`의 `NotFoundError`와 `internal/mcp/handler.go`의 `requireActiveGraph` | 구현 |
| `FR-AGENT_CONTEXT-039` | 권한 등급 | `internal/model/context.go`의 `GraphGrade`와 `internal/perm/perm.go`의 `rank` | 구현 |
| `FR-AGENT_CONTEXT-040` | 권한 상속 | `internal/store/permission.go`의 `EffectiveGrade`가 직접 부여와 팀 상속을 한 질의로 계산한다 | 구현 |
| `FR-AGENT_CONTEXT-041` | 권한 관리 | `internal/store/permission.go`의 `RevokeGraphGrant`와 `requireOwner` | 구현 |
| `FR-AGENT_CONTEXT-042` | 소유권 이전 | `internal/store/web.go`의 `GrantGraph` | 구현 |
| `FR-AGENT_CONTEXT-043` | 팀 관리 | `internal/store/web.go`의 `CreateTeam`·`AddTeamMember`·`RemoveTeamMemberWithAudit` | 구현 |
| `FR-AGENT_CONTEXT-044` | 동시 갱신 | `internal/store/context.go`의 `UpdateContextWithOperation`과 `contextVersionConflict` | 구현 |
| `FR-AGENT_CONTEXT-045` | 버전 관리 | `internal/store/store.go`의 `VersionConflictError` | 구현 |
| `FR-AGENT_CONTEXT-046` | 수명주기 | `internal/mcp/handler.go`의 다섯 관리 연산과 `operationRecord` | 구현 |
| `FR-AGENT_CONTEXT-047` | 감사 | `internal/store/operation.go`의 `insertOperation`과 `internal/store/web.go`의 `insertWebAudit` | 구현 |
| `FR-AGENT_CONTEXT-048` | 검색 | `internal/search/search.go`의 `Service.Flow`가 정한 채널 실행과 결합 순서 | 구현 |
| `FR-AGENT_CONTEXT-049` | 그래프 속성 | `migrations/001_init.sql`의 `context_graph`와 `internal/store/graph.go`의 `scanGraph` | 구현 |
| `FR-AGENT_CONTEXT-050` | 목록 조회 | `internal/store/cursor.go` | 구현 |
| `FR-AGENT_CONTEXT-051` | 그래프 검증 | `internal/mcp/handler.go`의 `requireActiveGraph`와 `internal/perm/perm.go`의 `Require` | 구현 |
| `FR-AGENT_CONTEXT-052` | 홉 조회 | `internal/store/hop.go`의 `HopContexts` | 구현 |
| `FR-AGENT_CONTEXT-053` | 순환 처리 | `internal/store/hop.go`의 `hopNeighbors`가 방문 깊이로 최단 홉을 남긴다 | 구현 |
| `FR-AGENT_CONTEXT-054` | 결과 상한 | `internal/search/search.go`의 `applyBudget`와 `internal/store/hop.go`의 `HopResult` | 구현 |
| `FR-AGENT_CONTEXT-055` | 이용 한도 | `internal/plan/plan.go`의 `CheckRequest`·`CheckIncrease`와 `internal/mcp/handler.go`의 `writeLimits` | 구현 |
| `FR-AGENT_CONTEXT-056` | 연산 매핑 | `internal/mcp/transport.go`의 `toolDefinitions`와 `internal/mcp/handler.go`의 `call` | 구현 |
| `FR-AGENT_CONTEXT-057` | 오류 응답 | `internal/mcp/handler.go`의 `mapError`와 `internal/mcp/transport.go`의 `domainError` | 구현 |
| `FR-AGENT_CONTEXT-058` | 입력 검증 | `internal/mcp/schema.go`의 `validateToolCall`와 `internal/model/context.go`의 `Validate` | 구현 |
| `FR-AGENT_CONTEXT-059` | 연결 생성 | `internal/store/relation.go`의 `createProposedRelation`과 `ConfirmRelation` | 구현 |
| `FR-AGENT_CONTEXT-060` | 재구성 | `internal/store/context.go`의 `CreateSupersedingContextWithOperation` | 구현 |
| `FR-AGENT_CONTEXT-061` | 충족 기준 | 그래프 효과와 지속 평가는 통과했다. `NFR-AGENT_CONTEXT-009`의 업무 효과 본 측정이 끝나면 세 기준을 함께 판정한다 | 평가 대기 |
| `FR-AGENT_CONTEXT-062` | 파생 생성 | `internal/store/context.go`의 `createContext`가 받는 파생 계층 경로 | 구현 |
| `FR-AGENT_CONTEXT-063` | 요약 범위 | `internal/mcp/handler.go`의 `contextFlow`가 받는 `summary_scope`와 `internal/search/search.go`의 `globalSummaries` | 구현 |
| `FR-AGENT_CONTEXT-064` | 근거 무효화 | `internal/store/context.go`의 `invalidateDerivedEvidence`와 `internal/model/context.go`의 `ValidateEvidenceInvalidation` | 구현 |
| `FR-AGENT_CONTEXT-065` | 오류 매핑 | `internal/mcp/handler.go`의 `limitError`·`invalidArgument`와 `internal/mcp/transport.go`의 `domainError` | 구현 |
| `FR-AGENT_CONTEXT-066` | 부분 상태 | `internal/search/search.go`의 `channelResult`와 `failureReason` | 구현 |
| `FR-AGENT_CONTEXT-067` | 시간 유효성 | `internal/model/context.go`의 `DerivedAttributes`가 가진 `valid_from`·`valid_to` | 구현 |
| `FR-AGENT_CONTEXT-068` | 만료 취급 | `internal/store/index.go`의 `TimeCandidates`와 `KeywordCandidates`의 시점 비교 | 구현 |
| `FR-AGENT_CONTEXT-069` | 폐기와 복구 | `internal/store/context.go`의 `DiscardContext`와 `RestoreContext` | 구현 |
| `FR-AGENT_CONTEXT-070` | 자동화 범위 | `internal/store/operation.go`의 `OperationKind` 다섯 종과 `internal/store/context.go`의 `KeepContext` | 구현 |
| `FR-AGENT_CONTEXT-071` | 기록 보존 | `migrations/001_init.sql`의 `operation_log`와 `internal/store/periodic.go`의 `CleanupAuditRecords` | 구현 |
| `FR-AGENT_CONTEXT-072` | 사건 경계 | `internal/model/context.go`의 `EventAttributes` | 구현 |
| `FR-AGENT_CONTEXT-073` | 사건 관계 | `internal/model/relation.go`의 `RelationType` | 구현 |
| `FR-AGENT_CONTEXT-074` | 관계 상태 | `internal/model/relation.go`의 `RelationState` | 구현 |
| `FR-AGENT_CONTEXT-075` | 관계 연산 | `internal/mcp/handler.go`의 `listRelations`·`confirmRelation`·`discardRelation` | 구현 |
| `FR-AGENT_CONTEXT-076` | 사건 재구성 | `internal/store/relation.go`의 `discardConfirmedRelationsForContext`와 `internal/mcp/handler.go`의 판단 입력 묶음 | 구현 |
| `FR-AGENT_CONTEXT-077` | 근거 상태 | `internal/model/context.go`의 `EvidenceState` | 구현 |
| `FR-AGENT_CONTEXT-078` | 신뢰 상태 | `internal/model/context.go`의 `ConfidenceState` | 구현 |
| `FR-AGENT_CONTEXT-079` | 상충 처리 | `internal/model/context.go`의 `ConfidenceState`가 가진 `disputed`. 자동 판정과 자동 제외 경로가 없다 | 구현 |
| `FR-AGENT_CONTEXT-080` | 검색 결합 | `internal/search/search.go`의 `combine`이 쓰는 순위 역수 합 | 구현 |
| `FR-AGENT_CONTEXT-081` | 검색 색인 | `internal/store/operation.go`의 `enqueueIndexTask` | 구현 |
| `FR-AGENT_CONTEXT-082` | 만료 조회 | `internal/mcp/handler.go`가 넘기는 `as_of`와 `internal/store/index.go`의 시점 비교 | 구현 |
| `FR-AGENT_CONTEXT-083` | 국소와 전역 | `internal/search/search.go`의 `globalSummaries`와 `graphCandidates` | 구현 |
| `FR-AGENT_CONTEXT-084` | 관계 제안 신호 | `internal/store/relation.go`의 `ProposeEventRelations`와 `internal/store/index.go`의 `ProposeSimilarEventRelations` | 구현 |
| `FR-AGENT_CONTEXT-085` | 검색 예산 | `internal/search/search.go`의 `applyBudget` | 구현 |
| `FR-AGENT_CONTEXT-086` | 예산 배분 | `internal/search/search.go`의 `applyBudget`가 채널 배분 없이 통합 절단한다 | 구현 |
| `FR-AGENT_CONTEXT-087` | 검색 측정 | `internal/search/search.go`의 `Channel`이 후보 수·지연·기여·실패를 담고 완료 로그가 예산과 절단을 남긴다 | 구현 |
| `FR-AGENT_CONTEXT-088` | 흐름 응답 | `internal/mcp/handler.go`의 `flowValue` | 구현 |
| `FR-AGENT_CONTEXT-089` | 흐름 절단 | `internal/search/search.go`의 `Truncation`과 `internal/mcp/handler.go`의 `flowValue` | 구현 |
| `FR-AGENT_CONTEXT-090` | 인증 흐름 | 발급은 `internal/authz/http.go`, 검증은 `internal/mcp/transport.go`와 `internal/authz/authz.go`의 `Verify` | 구현 |
| `FR-AGENT_CONTEXT-091` | 계정 매핑 | `internal/perm/perm.go`의 `Require`가 요청 시점에 판정한다 | 구현 |
| `FR-AGENT_CONTEXT-092` | 갱신 정책 | `internal/authz/authz.go`의 `renewFromClaims` | 구현 |
| `FR-AGENT_CONTEXT-093` | 토큰 검증 | `internal/authz/cache.go`의 `publicKey`와 `internal/authz/authz.go`의 `activeKey` | 구현 |
| `FR-AGENT_CONTEXT-094` | 다중 인스턴스 | `internal/store/auth.go`의 `SigningKeys`·`RevokedTokenIDs`와 `internal/store/periodic.go`의 `WithTryAdvisoryLock` | 구현 |
| `FR-AGENT_CONTEXT-095` | transport | `internal/mcp/transport.go`의 `ServeHTTP`가 확인하는 protocol version | 구현 |
| `FR-AGENT_CONTEXT-096` | 배포 단위 | `internal/config/config.go`의 `ComponentPlacement`와 `cmd/server/main.go`의 `run` | 구현 |
| `FR-AGENT_CONTEXT-097` | 색인 처리 | `internal/store/index.go`의 `ProcessNextIndexTask`와 `indexRetryDelay` | 구현 |
| `FR-AGENT_CONTEXT-098` | 임베딩 모델 | `internal/store/index.go`의 `ReindexOutdatedEmbeddings`와 `internal/index/index.go`의 `ModelID` | 구현 |
| `FR-AGENT_CONTEXT-099` | 관측성 | `cmd/server/server.go`의 `logRequests`와 `internal/search/search.go`의 완료 로그. 본문과 토큰 원문을 넣지 않는다 | 구현 |
| `FR-AGENT_CONTEXT-100` | 그래프 배치 | `internal/store/store.go`의 `New`가 받는 그래프 이름과 `migrations/001_init.sql`의 일반 테이블 | 구현 |
| `FR-AGENT_CONTEXT-101` | 격리 강제 | `internal/store/agtype.go`의 `parseContext` 안 `graph_id` 대조와 `cmd/server/boundary_test.go` | 구현 |
| `FR-AGENT_CONTEXT-102` | 정점과 간선 사상 | `internal/store/agtype.go`의 `contextProperties`와 `relationProperties` | 구현 |
| `FR-AGENT_CONTEXT-103` | 관계의 시간 | `internal/model/relation.go`의 `Relation`에 판 번호와 유효 기간이 없다 | 구현 |
| `FR-AGENT_CONTEXT-104` | 색인 저장과 질의 | `migrations/001_init.sql`의 `context_embedding`과 `internal/store/index.go`의 `SemanticCandidates` | 구현 |
| `FR-AGENT_CONTEXT-105` | 웹 화면 구성 | `internal/web/web.go`의 `ServeHTTP` 라우팅과 화면 여섯 종 | 구현 |
| `FR-AGENT_CONTEXT-106` | 권한 화면 | `internal/store/web.go`의 `ListGraphGrants`와 `internal/web/web.go`의 `graphAccess` | 구현 |
| `FR-AGENT_CONTEXT-107` | 삭제 확인 | `internal/store/web.go`의 `DeletionImpact`와 `internal/web/web.go`의 `renderDeletion` | 구현 |
| `FR-AGENT_CONTEXT-108` | 운영자 복구 | `internal/store/web.go`의 `OperatorRestoreGraph` | 구현 |
| `FR-AGENT_CONTEXT-109` | 웹 감사 기록 | `internal/store/web.go`의 `insertWebAudit`와 `ListAuditEntries` | 구현 |
| `FR-AGENT_CONTEXT-110` | 시각화 제공 | `internal/web/web.go`의 `newVisualizationData` | 구현 |
| `FR-AGENT_CONTEXT-111` | 시각화 표시 | `internal/web/web.go`의 `visualizationNodeData`와 시각화 템플릿 | 구현 |
| `FR-AGENT_CONTEXT-112` | 검증 원칙 | `cmd/eval`이 통제 조건과 반복 회차를 기록하고 같은 질의의 단계별 차이에 대한 95% 신뢰구간으로 판정한다 | 구현·측정 완료 |
| `FR-AGENT_CONTEXT-113` | 검색 품질 평가 | `cmd/eval`과 공개·자체 세트로 사실·연상·전역 검색의 재현율·순위 품질·예산 효율·채널 기여를 측정했다 | 구현·측정 완료 |
| `FR-AGENT_CONTEXT-114` | 그래프 효과 비교 | [검색 품질 본 측정](2026-09-20-search-quality-measurement.md)에서 기준선·국소 그래프·자동 전역 전환을 비교해 운영 구성을 판정했다 | 구현·측정 완료 |
| `FR-AGENT_CONTEXT-115` | 지속 평가 | `cmd/eval -continual`의 여섯 시나리오, 품질 비악화 판정과 `operation_log` 판단 입력 재확인 | 구현·측정 완료 |
| `FR-AGENT_CONTEXT-116` | 업무 효과 평가 | `cmd/eval -business`가 본문·개인 식별 정보 없이 두 조건의 네 집계 지표를 대응 비교한다. 실제 통제 과업 집계값을 넣은 본 측정은 남았다 | 평가 대기 |
| `FR-AGENT_CONTEXT-117` | 플랜 항목 | `internal/plan/plan.go`의 `Limits`와 `Default` | 구현 |
| `FR-AGENT_CONTEXT-118` | 플랜 변경 | `internal/plan/plan.go`의 `CheckIncrease` | 구현 |
| `FR-AGENT_CONTEXT-119` | 한도 초과 | `internal/plan/plan.go`의 `LimitError`와 `internal/mcp/handler.go`의 `limitError` | 구현 |
| `FR-AGENT_CONTEXT-120` | 기록 보존 하한 | `internal/store/periodic.go`의 `CleanupAuditRecords`가 종류별 보존 기간을 나눈다 | 구현 |
| `FR-AGENT_CONTEXT-121` | 벡터 압축 | `internal/config/config.go`의 `EMBEDDING_VECTOR_TYPE`·`EMBEDDING_DIMENSION`과 `internal/store/index.go`의 `ReindexGraph` | 구현 |
| `FR-AGENT_CONTEXT-122` | 색인 대상 비교 | `internal/config/config.go`의 `INDEX_TARGET_LAYERS`와 `internal/store/operation.go`의 `enqueueIndexTask` 계층 분기. 비교 자체는 평가에서 수행한다 | 구현 |
| `FR-AGENT_CONTEXT-123` | 저장 계층화 | `internal/store/tier.go`의 계층 이동·되읽기와 `migrations/001_init.sql`의 `hot`·`cold` 파티션 | 구현 |
| `FR-AGENT_CONTEXT-124` | 출처 구분 | `internal/model/context.go`의 `OriginKind`와 `ValidateUpdate` | 구현 |
| `FR-AGENT_CONTEXT-125` | 출처 전달 | `internal/store/index.go`의 `ContextOriginKinds`와 `internal/search/search.go`의 출처 전파 | 구현 |
| `FR-AGENT_CONTEXT-126` | 오염 사후 탐지 | `internal/store/web.go`의 `ListAuditEntries`가 `operation_log`와 `web_audit_log`를 함께 읽고 전파는 `internal/store/hop.go`의 `HopContexts`로 따라간다 | 구현 |
| `FR-AGENT_CONTEXT-127` | 중복 파생 접기 | `internal/search/search.go`의 `foldSimilarDerived` | 구현 |
| `FR-AGENT_CONTEXT-128` | 채널 실행 방식 | `internal/config/config.go`의 `SearchExecution`과 `internal/search/search.go`의 채널 실행 | 구현 |
| `FR-AGENT_CONTEXT-129` | 검색 채널 의존 | `internal/search/search.go`의 `graphCandidates`가 진입점 부재를 실패 채널로 담는다 | 구현 |
| `FR-AGENT_CONTEXT-130` | 계정 등록 | `internal/authz/authz.go`의 `Register`와 `migrations/001_init.sql`의 `account` | 구현 |
| `FR-AGENT_CONTEXT-131` | 자격 증명 보호 | `internal/authz/authz.go`의 `hashPassword`와 `comparePassword` | 구현 |
| `FR-AGENT_CONTEXT-132` | 계정 소멸 부재 | `internal/store/permission.go`의 `accessibleAccountCount` | 구현 |
| `FR-AGENT_CONTEXT-133` | 웹 세션 | `internal/web/web.go`의 `session`과 `setSessionCookie` | 구현 |
| `FR-AGENT_CONTEXT-134` | 채널 분리 | `internal/authz/authz.go`의 `Verify`가 대조하는 `aud` | 구현 |
| `FR-AGENT_CONTEXT-135` | 운영자 자격 | `internal/web/web.go`의 `operatorRestores`가 인증된 세션만 보고 계정에 별도 판정 값을 두지 않는다 | 구현 |
| `FR-AGENT_CONTEXT-136` | 운영자 화면 | `internal/store/web.go`의 `PendingRestoreRequests` | 구현 |
| `FR-AGENT_CONTEXT-137` | 복구 요청 | `internal/store/web.go`의 `RequestGraphRestore` | 구현 |
| `FR-AGENT_CONTEXT-138` | 흐름 입력 | `internal/mcp/handler.go`의 `contextFlow`가 받는 `work_context` | 구현 |
| `FR-AGENT_CONTEXT-139` | 본문 상한 | `internal/mcp/schema.go`의 `textRange`와 `internal/model/context.go`의 검증 | 구현 |
| `FR-AGENT_CONTEXT-140` | 전송 보안 | `internal/config/config.go`의 `TLSMode`·`TRUSTED_PROXY_CIDRS`와 `cmd/server/server.go`의 `requireTLS` | 구현 |
| `FR-AGENT_CONTEXT-141` | 본문 전송 | `internal/config/config.go`의 `EMBEDDING_BASE_URL`과 `internal/index/index.go`의 `Embed` | 구현 |
| `FR-AGENT_CONTEXT-142` | 제안 정리 | `internal/store/periodic.go`의 `CleanupExpiredProposals` | 구현 |
| `FR-AGENT_CONTEXT-143` | 관계 목록 | `internal/store/cursor.go`의 `encodeRelationCursor` | 구현 |
| `FR-AGENT_CONTEXT-144` | 팀 삭제 | `internal/store/permission.go`의 `DeleteTeam`과 `EffectiveGrade`의 삭제 팀 제외 | 구현 |
| `FR-AGENT_CONTEXT-145` | 파생 불변 | `internal/model/context.go`의 `ValidateUpdate`가 수정 가능한 대상을 목록으로 제한한다 | 구현 |
| `FR-AGENT_CONTEXT-146` | 원천 중복 | `internal/store/context.go`의 `findSource`와 `migrations/001_init.sql`의 `context_source_ref_idx` | 구현 |
| `FR-AGENT_CONTEXT-147` | 클라이언트 등록 | `internal/config/config.go`의 `parseOAuthClients`와 `internal/authz/authz.go`의 `ValidateRedirectTarget` | 구현 |
| `FR-AGENT_CONTEXT-148` | 인가 코드 | `internal/store/auth.go`의 `ConsumeAuthorizationCode`와 `CodeUsedError` | 구현 |
| `FR-AGENT_CONTEXT-149` | 전송 계층 검증 | `internal/mcp/transport.go`의 `ServeHTTP`가 수행하는 여섯 검증 | 구현 |
| `FR-AGENT_CONTEXT-150` | 근거 경로 선택 | `internal/search/evidence.go`의 `selectEvidenceSets`와 `SEARCH_EVIDENCE_PATH_SELECTION_ENABLED`, `cmd/eval`의 `evidence` 단계 | 구현 |
| `FR-AGENT_CONTEXT-151` | 질의 적응형 검색 | `internal/search/route.go`의 `RouteSignals.Decide`와 `SEARCH_QUERY_ADAPTIVE_ROUTING_ENABLED`, `cmd/eval`의 `-adaptive` 비교 | 구현 |
| `FR-AGENT_CONTEXT-152` | 일관 읽기 | `internal/store/consistency.go`의 `BeginReadSnapshot`·`enterReadScope`, `internal/search/search.go`의 `ConsistencySnapshot`, `cmd/eval`의 `-consistency` 비교 | 구현 |
| `FR-AGENT_CONTEXT-153` | 불변식 감사 | `internal/store/invariant.go`의 `invariantRules`·`checkWriteInvariants`·`AuditGraph`, `cmd/audit` | 구현 |
| `FR-AGENT_CONTEXT-154` | 오염 적대적 평가 | `cmd/eval/adversarial.go`의 `runAdversarial`·`diagnosePath`·`summarizeMetrics`, `cmd/eval/adversarial_dataset.go`의 `validateAttackSet`·`convertOwnAdversarialSet` | 구현 |

## 발견 사항

### 1. `FR-AGENT_CONTEXT-122` 색인 대상 비교를 고를 자리가 없었다 (해소)

요구사항은 원천을 포함한 색인과 파생·사건만 색인한 구성을 비교하라고 한다. `SDD.md` 「검증을 가능하게 하는 설계」는 색인 작업 등록 조건에 계층 분기를 두는 것으로 설계했다.

검사 시점에는 `internal/store/operation.go`의 `enqueueIndexTask`가 계층을 보지 않고 저장되는 모든 컨텍스트를 등록했고, 두 구성을 고를 구성 값도 없었다. 배포 구성 26개 이름을 전수로 확인했다.

- 근거: 판독. `enqueueIndexTask`의 삽입문과 `internal/config/config.go`의 구성 이름 전수.
- 처리: 같은 날 등록 조건에 계층 분기를 넣고 배포 구성 `INDEX_TARGET_LAYERS`를 열었다. 재색인이 대상에서 빠진 계층의 임베딩과 대기 작업을 함께 지우므로 구성을 바꾼 뒤에도 두 구성이 등록 조건 말고는 같다. 후속 [검색 품질 본 측정](2026-09-20-search-quality-measurement.md)에서 `all_layers`와 `without_source`를 비교했고, 원천 제외의 재현율·순위 품질 손실이 유의해 `all_layers`를 기본으로 유지했다.

### 2. `FR-AGENT_CONTEXT-123` 저장 계층 이동이 값만 있고 동작이 없었다 (해소)

`internal/plan/plan.go`가 `TierMoveAfterDays`를 플랜 항목으로 받고 검증까지 하지만 이 값을 읽는 곳이 어디에도 없다. `context_embedding`에도 파티셔닝이 없다. `SDD.md` 「저장 계층화」가 설계한 두 대상의 이동 경로가 구현에 없다.

- 근거: 판독. `TierMoveAfterDays` 전수 검색이 `plan` 패키지 안 세 자리만 내고, `migrations/001_init.sql`에 `PARTITION`이 없다.
- 영향: 기본값이 이동하지 않음이므로 현재 동작에는 문제가 없다. 플랜으로 기간을 주면 값이 조용히 무시된다.
- 처리: `context_embedding`을 `storage_tier` 기준 `hot`·`cold` 파티션으로 나누고, 일곱 번째 주기 작업이 접근 차단 대상과 플랜 기간을 넘긴 활성 임베딩을 `cold`로 옮기게 했다. 실제 활성 조회는 접근 시각을 갱신하며 같은 문장으로 `hot`에 되읽는다. 기본값 0은 활성 임베딩을 이동하지 않는다.

### 3. 평가에 걸린 여섯 건

`FR-AGENT_CONTEXT-061`과 `-112`~`-116`은 측정을 거쳐야 닫힌다. 설계가 측정 값을 내는 자리를 모두 만들어 두었으나 데이터셋과 실행기가 없다. 선행 감사의 "평가 미수행" 한계와 같은 원인이다.

## 남은 한계

| 한계 | 내용 |
|------|------|
| 동작 재현 없음 | 대조는 판독으로 했다. 요구사항마다 동작을 실행해 확인하지는 않았다. 동작 확인은 통합 테스트와 선행 결함 검사가 갖고 있다 |
| 결함 미탐색 | 구현이 있는지만 본다. 구현이 요구사항을 잘못 수행하는 경우는 이 검사가 잡지 못한다 |
| 비기능 요구사항 제외 | 13건의 측정값 부재는 선행 감사가 이미 한계로 적었다 |
