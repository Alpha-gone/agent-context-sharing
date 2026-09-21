# 비기능 요구사항 검증

## 목적과 범위

- 검증일: 2026-09-21
- 대상: `NFR-AGENT_CONTEXT-001`~`013`
- 목적: 비기능 요구사항 13건을 현재 구현, 자동화 테스트와 본 측정 결과에 하나씩 대응시켜 출시 판정의 실제 미완료를 식별한다.
- 제외: 실제 브라우저 시각 확인은 사용자가 직접 수행하므로 이 검증에서 다루지 않는다.

수치 평가를 요구하는 항목은 본 측정 결과를 사용하고, 저장·추적·동시성·연결·기밀성처럼 관찰 가능한 계약을 요구하는 항목은 통합·계약 테스트를 근거로 삼았다. 설계 요소가 존재하는 것만으로 통과시키지 않았다.

## 결과

| 요구사항 | 판정 | 근거 |
|----------|------|------|
| `NFR-AGENT_CONTEXT-001` 데이터 저장 | 통과 | PostgreSQL 18·Apache AGE 1.8.0·pgvector 0.8.6 개발 구성에서 빈 `001_init.sql` 적용, 재실행과 DB 필수 전체 테스트를 완료했다. `TestStoreIntegration`이 실제 AGE의 저장·조회 경계를 지난다. |
| `NFR-AGENT_CONTEXT-002` 추적성 | 통과 | `TestStoreIntegration`이 `source_ref`, 판 번호와 `SUPERSEDES` 경로를 실제 AGE에서 확인하고, 지속 평가가 유효 기간과 상태 변경 기록을 확인했다. |
| `NFR-AGENT_CONTEXT-003` 설명 가능성 | 통과 | `TestHopResultsCarryReferencesIntegration`, `TestFlowAddsGraphCandidatesAndKeepsOnlyIncludedEdges`, `TestFlowValueCarriesHopBoundaryAndDistance`가 응답의 출처·참조·진입 거리와 기여 채널을 확인한다. |
| `NFR-AGENT_CONTEXT-004` 검색 검증 가능성 | 통과 | [검색 품질 본 측정](2026-09-20-search-quality-measurement.md)이 사실·연상·전역 사용 사례를 나눠 재현율, 순위 역수, 예산 효율과 채널 기여를 산출했다. |
| `NFR-AGENT_CONTEXT-005` 그래프 비교 가능성 | 통과 | 같은 입력·표현·예산·모델에서 비그래프 기준선과 그래프 단계를 비교해 국소 확장은 제외하고 자동 전역 전환을 채택했다. |
| `NFR-AGENT_CONTEXT-006` 효율성 | 통과 | 검색 품질 본 측정 결과가 문자 예산, 단계 지연과 검색 품질을 같은 조건에 기록했다. `TestApplyBudgetDoesNotFillWithLowerRankedShortContext`가 예산 절단 계약을 고정한다. |
| `NFR-AGENT_CONTEXT-007` 신뢰성 | 통과 | [지속 평가 본 측정](2026-09-20-continual-evaluation.md)이 여섯 시나리오에서 품질 비악화와 `operation_log` 판단 기록을 확인했다. |
| `NFR-AGENT_CONTEXT-008` 장기 품질 검증 가능성 | 통과 | 지속 평가가 온라인 갱신·재생·전이·복구·망각·상충 해소를 각각 독립 그래프에서 세 번 실행해 상태 전이와 품질을 판정했다. |
| `NFR-AGENT_CONTEXT-009` 업무 효과 | 본 측정 대기 | `cmd/eval -business`가 과업 완료 시간·재작업·세션 재개·협업 활용의 대응 비교와 95% 신뢰구간 판정을 수행한다. 실제 통제 과업의 두 조건 집계값은 아직 없다. |
| `NFR-AGENT_CONTEXT-010` 감사 가능성 | 통과 | `TestOwnershipTransferAuditMatchesSpecIntegration`, `TestWebContextDeletionAuditSharesTransactionIntegration`과 지속 평가의 판단 기록 검사가 생성·변경 주체와 시점의 기록을 확인한다. |
| `NFR-AGENT_CONTEXT-011` 동시 갱신 일관성 | 통과 | `TestConcurrentUpdateReportsVersionConflictIntegration`, `TestConcurrentDiscardAppliesOnceIntegration`과 관계 확정 동시성 테스트가 유실 대신 충돌 또는 단일 적용으로 끝나는지 확인한다. |
| `NFR-AGENT_CONTEXT-012` 연결 독립성 | 통과 | Streamable HTTP의 요청별 검증·처리 계약 테스트, 프로토콜 세션 부재와 내장·분리 배치 네 조합의 구성 검사가 서버 요청 사이에 연결 상태를 요구하지 않음을 확인한다. |
| `NFR-AGENT_CONTEXT-013` 토큰 기밀성 | 서버 범위 통과·클라이언트 범위 대기 | `TestRequestLogExcludesSensitiveValues`, TLS 강제 테스트, JWT 원문 미저장 구조가 서버의 전송·저장·로그 경계를 확인한다. MCP 클라이언트의 프로세스 메모리 전용 보관은 별도 클라이언트 구현 범위라 아직 실행 근거가 없다. |

현재 서버 구현과 검증 범위에서는 11건이 통과했다. `NFR-AGENT_CONTEXT-009`는 실제 업무 효과 본 측정이, `NFR-AGENT_CONTEXT-013`은 별도 MCP 클라이언트의 메모리 전용 토큰 보관 검증이 남았다.

## 업무 효과 집계 경로

`cmd/eval -business <입력.json> -out <결과.json>`은 배열 항목 하나에 같은 통제 과업의 조회 없는 조건과 조회 있는 조건을 함께 받아 비교한다. 입력에는 다음 값만 허용한다.

- 데이터셋 판
- 에이전트 버전, 모델, 컨텍스트 예산
- 두 조건의 과업 완료 시간, 재작업 횟수, 세션 재개 상호작용 수, 협업 활용 비율

세 표본보다 적은 입력, 음수 횟수, 범위를 벗어난 협업 활용 비율과 정의되지 않은 필드를 거부한다. 결과에는 개별 표본을 남기지 않고 네 지표의 조건별 평균, 차이, 95% 신뢰구간과 판정만 기록한다.

```json
{
  "version": "controlled-tasks-v1",
  "conditions": {
    "agent_version": "agent-v1",
    "model": "model-v1",
    "context_budget": 4000
  },
  "samples": [
    {
      "without_context": {
        "completion_time_ms": 120000,
        "rework_count": 2,
        "resume_interactions": 4,
        "collaboration_usage": 0
      },
      "with_context": {
        "completion_time_ms": 90000,
        "rework_count": 1,
        "resume_interactions": 2,
        "collaboration_usage": 0.5
      }
    }
  ]
}
```

위 예시는 필드 형식만 보여 주며 실제 실행에는 같은 구조의 표본이 세 개 이상 있어야 한다.

```shell
go run ./cmd/eval -business <업무 효과 집계>.json -out <업무 효과 결과>.json
```

## 남은 판정

1. 동일한 통제 과업을 컨텍스트 조회 없는 조건과 있는 조건에서 각각 최소 세 번 수행한다.
2. 본문과 개인 식별 정보 없이 집계값만 입력해 `cmd/eval -business`를 실행한다.
3. 네 지표가 모두 유의하게 개선되는지 판정한다.
4. 결과로 `FR-AGENT_CONTEXT-116`과 `NFR-AGENT_CONTEXT-009`를 닫고, 그래프 효과·지속 평가 결과와 함께 `FR-AGENT_CONTEXT-061`의 비전 충족 여부를 확정한다.

MCP 클라이언트 구현 시에는 접근 토큰이 프로세스 메모리에만 있고 종료·재시작 뒤 남지 않는지 검증해 `NFR-AGENT_CONTEXT-013`의 남은 범위를 닫는다.

## 실행 검증

- `TEST_DATABASE_REQUIRED=1 go test ./...`: 실제 AGE 데이터베이스를 요구하는 조건으로 통과
- `go build ./...`: 통과
- `go vet ./...`: 통과
- `gofmt -l .`: 출력 없음
- 업무 효과 단위 테스트: 네 지표의 유의한 개선·미개선 판정, 최소 표본 수, 지표 범위와 미정의 필드 거부를 확인
