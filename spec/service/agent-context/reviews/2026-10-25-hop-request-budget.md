# 홉 후보 조회의 요청 예산 초과 원인과 수정

- 기준: 2026-10-25 09:06 KST, `dev`의 `112c157`에서 만든 `investigate/search-request-budget`.
- 대상: [SEARCH-10 잔여 조건](2026-10-25-search-10.md#실행-결과실패잔여-조건)의 상한 2 후보 조회와 [홉 후보 SQL](../../../../internal/store/hop_bounded.go).
- 09:06 조사 판정: 동일 오류 재현과 원인 확인 완료, 당시 제품 수정은 미수행. 후속 수정·검증은 마지막 절에 구분한다.

## 확인된 원인

소형 그래프의 통계가 남은 상태에서 새 대형 허브를 적재하면 `hopCandidateSQL`의 큰 인접 집합 처리 경로가 나쁜 조인 순서를 선택한다. **정렬 전 전체 후보에 대한 상관 존재 확인과, 후보마다 반복하는 허브 간선 탐색이 결합**한다. 30초 예산은 이 질의를 정상적으로 취소하지만 조회 비용 자체를 줄이지는 못한다.

1. 기존 시험이 남긴 `Context` 39행·`DERIVED_FROM` 17행에서 통계를 수집한 뒤 원천 하나·파생 20,000개와 참조 20,000개를 추가한다. 새 표본 뒤에는 통계를 갱신하지 않는다.
2. 상한 2에는 시작점 하나가 포함되므로 남은 자리는 1이다. 식별자 표본 65행이 전환 기준 64를 넘으면 큰 인접 집합의 존재 확인 경로를 실행한다.
3. 느린 계획은 새 그래프의 후보 수를 1행으로 예상한다. `context_active_graph_context_idx`의 순서대로 조기 종료하는 대신 `context_graph_id_text_idx`로 후보를 읽고, `EXISTS`를 통과한 후보 전체를 정렬한 뒤 `LIMIT 2`를 적용한다.
4. `EXISTS`의 들어오는 방향도 허브의 간선을 1행으로 예상한다. 기준 정점→`DERIVED_FROM_end_id_idx`→이웃 정점 순서로 조인하므로 후보마다 허브의 간선을 다시 읽는다. 반대 끝점은 복합 인덱스의 조건이 아니라 뒤의 `Join Filter`가 된다. 이 계획은 복합 끝점 인덱스가 존재해도 발생한다.

반환 노드 상한이나 65행 표본은 DB 내부 작업량의 상한이 아니다. 여러 후보 각각에 대해 큰 인접 집합을 다시 확인하는 것이 직접적인 병목이다. 단순한 Bitmap 접근·일회 전체 정렬 비용만으로 이번 30초 초과를 설명하지 않는다.

## 환경과 재현

- Go 1.27.1 darwin/arm64, PostgreSQL 18.6 linux/aarch64, AGE 1.8.0, pgvector 0.8.6, 보유 DB 이미지 `2b6d51048c03`.
- 전용 tmpfs 컨테이너 `acs-budget-investigation`과 합성 DB 네 개만 사용했다. 정식 `cmd/migrate up`으로 001·009~013의 여섯 마이그레이션을 적용했다. 기존 개발 DB·WAL 컨테이너·볼륨·환경 파일·운영 구성은 변경하지 않았고 외부 제공자를 호출하지 않았다.
- 통계의 전후 상태를 고정하는 통제 DB에서만 `Context`와 `DERIVED_FROM`의 autovacuum을 비활성화했다. 제품 구성이나 권장 운영 설정의 변경이 아니다.
- 관측 시 장시간 후보 질의는 `active`였고 잠금 대기는 없었다. 확보한 계획에는 JIT가 없었다. 주된 원인을 잠금 대기·JIT·Go 마감 계산 오류로 판정하지 않는다.

새 합성 DB에서 다음 기존 시험 집합을 실행하면 공유 label에 39개 정점·17개 간선이 남았다. 별도 DB를 쓰는 형상 시험의 데이터는 그 DB와 함께 정리된다.

```sh
# TEST_DATABASE_URL은 이 조사만을 위한 새 DB에 지정하고 정식 스키마를 적용한다.
TEST_DATABASE_REQUIRED=1 go test -race -count=1 -timeout=180s \
  -run '^(TestHopIncludesLastDepthEdgesIntegration|TestHopImplementationsReturnSameResultIntegration|TestHopConcurrentDiscardIntegration|TestHopStartReadBoundaryIntegration|TestStandaloneHopFitsSmallPoolIntegration|TestHopFullContractIntegration|TestHopFullMaxReferencesIntegration|TestHopBoundedMatchesFullIntegration)$' \
  ./internal/store
```

이후 조사 전용 두 label의 autovacuum을 끄고 `ANALYZE`한다. 임시 Go overlay로 기존 `createHopHub(t, s, 20000)`를 호출한 뒤 다음 요청을 실행했다.

```go
s.HopContextsFrom(ctx, graph, []model.Context{hub}, 2, "both", nil, 2)
```

실패한 fixture를 정리하기 전에 같은 SQL의 계획을 보존하고, 두 label에 `ANALYZE`만 실행하여 같은 요청을 다시 실행했다. 요청의 30초 예산과 표본·인덱스·SQL은 바꾸지 않았다. 제품 소스는 편집하지 않았다.

별도 누적 DB의 기존 `TestHopHubBoundedWorkIntegration`도 race 조건에서 같은 상한 2 오류로 실패했다. 앞선 시험만 단순 재실행한 첫 시도는 통과했으며, 통계 상태를 고정한 추가 비교로 실패 조건을 분리했다. 대형 표본이 하나 있다는 사실만으로 언제나 실패하는 것은 아니다.

## 같은 데이터·SQL의 통계 갱신 전후

| 관측 | 갱신 전 | 두 label의 `ANALYZE` 후 |
|---|---|---|
| 공개 홉 호출 | 30.001409초, `context deadline exceeded`, 빈 결과 | 11.223875ms, 노드 2개·정상 절단 |
| 후보 선택 계획 | `context_graph_id_text_idx`→상관 존재 확인→정렬·LIMIT | `context_active_graph_context_idx`의 순서 접근과 조기 LIMIT |
| 상관 들어오는 간선 확인 | 허브의 `end_id`로 읽고 반대 끝점은 Join Filter | 후보 정점에서 복합 끝점 인덱스로 읽고 기준 정점과 대조 |
| 독립 EXPLAIN ANALYZE | 진단용 90초 제한에도 미완료, 취소 | 실행 0.436ms, 공유 버퍼 hit 251 |

0.436ms는 `EXPLAIN`의 DB 실행 시간이고 11.223875ms는 전체 공개 호출의 벽시계 시간이다. 서로 다른 지표로 유지한다. 통계 갱신 후 결과는 같은 표본의 전체 본문 BFS 기준선과 노드·간선·순서·거리·절단을 대조하여 일치했다.

### 느린 계획의 실제 반복량

전체 느린 질의는 90초에도 끝나지 않아 완료된 실제 행 수를 얻지 못했다. 대신 **동일한 느린 통계와 SQL에 마지막 후보 10개의 정점 ID 필터만 추가**하여 존재 확인의 작업량을 측정했다. 원래 전체 질의의 완료 측정으로 취급하지 않는다.

- 후보의 상관 존재 확인: 10회.
- 들어오는 `DERIVED_FROM_end_id_idx`: 회당 평균 19,995.5행, 10회, 합계 **199,955행**.
- 그 안의 이웃 `context_context_id_agtype_idx`: **199,955회** 실행.
- 이웃과의 Join Filter는 회당 평균 약 19,994행을 제외했다.
- 변경한 진단 질의의 DB 실행 시간: **1,171.641ms**. 상관 부분의 공유 버퍼 hit는 803,316이었다. hit 수를 디스크 읽기 횟수로 해석하지 않는다.

반면 `ANALYZE` 후의 원래 후보 질의는 순서 있는 활성 후보 2개를 확인하고, 상관 들어오는 간선은 각 1행씩 2회 읽었다. 이것이 같은 데이터에서 지연이 급감한 실행상 차이다.

## 영향과 후속 수정 방향

- 새 대형 그래프와 현재 통계의 차이가 큰 경계에서 선택적 후보 조회가 심하게 느려질 수 있음을 재현했다. 고정 이미지·합성 표본의 관측이며 실제 운영 데이터의 발생 빈도·지연 목표는 측정하지 않았다.
- `ANALYZE`는 이번 표본의 회복을 입증하는 통제 조작이다. 그것만으로 질의의 통계 민감성과 모든 형상의 재발 가능성이 제거됐다고 판정하지 않는다.
- 후속 수정에서는 정렬 후보와 간선의 양 끝점을 먼저 좁히고, 허브 간선을 후보마다 반복 탐색하는 계획을 방지하는 질의 구조를 검토해야 한다. 최신 통계와 소형 통계→신규 대형 허브·양방향·다중 시작점에서 결과 완전성과 실제 행·loops를 다시 대조해야 한다.
- 예산 확대·임의 절단·간선 생략은 수행하지 않았다. 제품 코드·스키마·정본 계약·개발 작업의 완료 표시는 유지한다.
- 과거 SEARCH-10 실패 당시의 계획은 보존되지 않았다. 이번에는 같은 제품 코드·표본·오류를 다시 재현하여 결함을 확인했다. 과거 실행의 정확한 행 수나 당시 계획이 이번과 완전히 같았다고 소급 단정하지 않는다.

## 근거·검증·정리

- 기존 시험 집합의 새 준비 실행은 통과했다. 이후 기존 허브 시험에서 실패를 재현했고, 통제 비교에서 30초 실패·90초 진단 미완료와 통계 갱신 후 성공·기준선 동등성을 각각 기록했다. 마지막 진단 helper의 `PASS`는 예상 실패 관측을 포함하는 원인 비교의 완료이며 전체 검색 회귀 통과를 뜻하지 않는다.
- 임시 overlay와 원시 계획·로그는 `/tmp/acs-budget-*`에 남겼다. 주요 근거는 `acs-budget-cumulative.log`, `acs-budget-failure-2.json`, `acs-budget-exact-prepare.log`, `acs-budget-exact-cause.log`, `acs-budget-cause-20000-before-false.json`, `acs-budget-last-ten-plan.json`, `acs-budget-cause-20000-after-true.json`이다. 임시 자료의 영구 보관은 보장하지 않으며 주요 조건·측정값·실패는 이 문서에 남긴다.
- 공식 근거는 [PostgreSQL 18 EXPLAIN](https://www.postgresql.org/docs/18/using-explain.html), [플래너 통계](https://www.postgresql.org/docs/18/planner-stats.html), [CTE 물질화](https://www.postgresql.org/docs/18/queries-with.html#QUERIES-WITH-CTE-MATERIALIZATION)를 확인했다. 설명 자료와 이번 이미지에서 실제 관측한 계획을 구분한다.
- DB 네 개의 남은 클라이언트 연결이 0개임을 확인했다. 자체 라벨 `agent-context.task=budget-investigation`의 전용 컨테이너를 제거했고 같은 라벨의 잔여 컨테이너는 0개다. 기존 개발 DB·WAL 컨테이너 두 개는 유지했다.
- 변경 Markdown 4개의 코드 블록 경계·로컬 링크 9개·앵커와 `git diff --check`를 확인했고 오류는 없었다. DOX 소유 경계도 대조했다. 구현·계약·문서 소유권은 바뀌지 않아 SRS·SDD·개발 계획·DOX·용어집은 유지한다. 조사 결과만 검토·근거·요청 기록에 연결했다. 제품 코드의 변경이 없어 전체 build·vet·회귀 시험은 반복하지 않았다. 기존 `.idea` 미추적 설정 두 파일은 유지했다.

## 후속 수정과 모듈 검증

사용자의 수정 요청으로 [검색 설계](../sdd/search.md#고연결-노드의-조회적재-보호)를 먼저 갱신하고 `hopCandidateSQL`을 수정했다. 현재 브랜치명은 `fix/search-request-budget`이다. 큰 인접 집합은 기준 정점의 UUID·물리 ID를 한 번 읽어 물질화하고, 후보 정점의 식별자 정렬을 상관 존재 확인과 분리한다. 간선은 물리 label 테이블에서 `start_id`·`end_id`를 함께 제한한 뒤 graph_id·confirmed 상태를 대조한다. 방향별 존재 확인을 합치며 정점 재조인을 제거했다. 표본·초과 확인·최초 기준 정점·발견 순서·간선 완전성·스냅숏·30초 예산은 유지한다. 스키마·인덱스·구성·플래너 설정·요청 중 `ANALYZE`는 추가하지 않았다.

[새 회귀 시험](../../../../internal/store/hop_stale_statistics_integration_test.go)은 시험별 독립 DB에서 정점 39개·참조 17개의 통계를 수집한 뒤 2만 간선 허브를 추가한다. autovacuum 비활성화와 통계 수집은 해당 시험 DB에만 적용한다. 수정 전 제품 코드를 Go overlay로 연결하면 첫 `stale/both/2`가 **30.00초에 실패**했다. 같은 새 시험은 수정 코드에서 통계 갱신 전후의 양방향/들어오는 방향·상한 2/10 여덟 경우와 전체 본문 기준선 동등성·연결 반환을 통과했다.

| 통계 / 방향 / 상한 | 전체 요청 | 후보 SQL | 실제 간선 행 | 실제 정점 행 |
|---|---|---|---|---|
| 오래됨 / both / 2 | 78.516ms | 65.933ms | 67 | 20,069 |
| 오래됨 / in / 2 | 78.627ms | 67.858ms | 67 | 20,068 |
| 오래됨 / both / 10 | 97.613ms | 64.372ms | 75 | 20,069 |
| 오래됨 / in / 10 | 95.648ms | 68.797ms | 75 | 20,068 |
| 갱신 / both / 2 | 15.072ms | 0.390ms | 67 | 71 |
| 갱신 / in / 2 | 14.295ms | 0.408ms | 67 | 70 |
| 갱신 / both / 10 | 23.667ms | 0.486ms | 75 | 79 |
| 갱신 / in / 10 | 20.332ms | 0.604ms | 75 | 78 |

행 수는 기존 `explainHop`의 relation 노드별 `(Actual Rows + Rows Removed by Filter) × Actual Loops` 합이다. 오래된 통계에서 식별자 전체 읽기·정렬 비용은 남는다. 정점 접근이 `O(L)`로 바뀌었다고 주장하지 않으며, 제거한 병목은 후보별 허브 간선 반복 탐색이다. 요청과 후보 SQL은 별도 실행이고 이 표는 race 조건의 관측값으로 운영 지연 목표·통계적 개선폭을 대체하지 않는다.

기존 선택 계약 표본에도 실제 끝점은 같지만 graph_id가 다른 확정 간선을 추가했다. 이 간선이 미확정 후보를 확정 이웃으로 승격시키지 않는지 검사하여 큰 인접 집합의 SQL 판정에서도 격리를 확인했다. 삭제·미확정·역순 삽입·다중 시작점·순환·조밀 간선·상한/표본 경계의 기존 비교도 유지했다.

모듈 검증은 필수 DB·race에서 **고유 시험 122건, 최상위 8건**이 실패·건너뜀 없이 통과했다. 허브 1천·2만·5만의 선택적 조회도 통과했으며 수정 전 코드의 의도한 실패를 이 수에 포함하지 않는다. 실행 근거는 `/tmp/acs-budget-fix-regression-before.log`와 `/tmp/acs-budget-fix-module.json`이다.

### 분야 통합과 완료 판정

모듈 검증 이후 나머지 홉·SEARCH-10 저장소 시험 127건, 검색 패키지 331건, 실제 DB·TLS의 MCP-07 읽기와 MCP-09 조립 서버 계약 44건이 통과했다. 모듈과 합쳐 **고유 시험 624건, 최상위 103건**이며 최종 수정 코드에 실패·건너뜀이 없다. 전체 본문 기준선·방향/필터·계층별 참조·최대 참조·동시 폐기·최소 풀·읽기 실패·30초 내부 예산·짧은 호출자 마감·취소·연결/예약 회복·서버 응답 스냅샷을 확인했다. 수정 전 overlay의 예상 실패는 별도로 유지한다.

```sh
# Go 1.27.1, 전용 합성 TEST_DATABASE_URL과 TEST_DATABASE_REQUIRED=1 지정
go test -race -count=1 -json -timeout=600s \
  -run '^(TestHopStaleStatisticsHubIntegration|TestHopBoundedSelectionContractIntegration|TestHopBoundedMatchesFullIntegration|TestHopHubBoundedWorkIntegration|TestHopSelectiveScanIntegration|TestHopSelectiveProbeBoundaryIntegration|TestHopSelectiveMultiAnchorAndDenseEdgesIntegration|TestHopCandidateSQLBoundaries)$' ./internal/store
go test -race -count=1 -json -p 1 -timeout=600s \
  -run '^(TestHop|TestStandaloneHop|TestSEARCH10)' \
  -skip '^(TestHopStaleStatisticsHubIntegration|TestHopBoundedSelectionContractIntegration|TestHopBoundedMatchesFullIntegration|TestHopHubBoundedWorkIntegration|TestHopSelectiveScanIntegration|TestHopSelectiveProbeBoundaryIntegration|TestHopSelectiveMultiAnchorAndDenseEdgesIntegration|TestHopCandidateSQLBoundaries|TestHopProbeIndexCostIntegration)$' ./internal/store
go test -race -count=1 -json -timeout=180s ./internal/search
go test -race -count=1 -json -p 1 -timeout=180s \
  -run '^(TestMCP07|TestMCP09AssembledServerContractIntegration)$|^TestMCP07' ./cmd/server
go build ./...
go vet ./...
gofmt -l .
```

전체 build·vet·gofmt 검사는 통과했다. 최종 분야 통합의 원시 근거는 `/tmp/acs-budget-fix-integration.json`, `/tmp/acs-budget-fix-search.json`, `/tmp/acs-budget-fix-server.json`이다. 같은 이미지·CPU 판의 합성 DB 회귀이며 실제 운영 규모·다른 PostgreSQL/AGE 조합·외부 제공자·브라우저·실제 호스트·통제 품질/비용 평가나 전체 저장소 시험은 이번 검증 범위가 아니다. 124개 개발 작업의 완료 수는 바꾸지 않는다.

수정 검증용 컨테이너 `acs-budget-fix`에는 마지막에 부모 DB `budget_fix`만 남았고, 시험별 DB와 클라이언트 연결은 0개였다. 이 컨테이너만 제거한 뒤 `agent-context.task=budget-fix`의 잔여 컨테이너 0개와 기존 개발 DB·WAL 컨테이너 두 개의 유지를 확인했다. 변경 Markdown 8개·로컬 링크/앵커 212개·코드 블록과 `git diff --check`를 검증했고 오류는 없었다. DOX의 소유권·패키지 경계·구조는 바뀌지 않아 DOX·SRS·용어집·환경 안내는 유지했다. 현재 변경은 미커밋이며 기존 `.idea` 두 파일은 건드리지 않았다.
