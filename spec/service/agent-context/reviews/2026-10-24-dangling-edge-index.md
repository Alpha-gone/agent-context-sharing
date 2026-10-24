# 고아 간선 보충 조회의 전체 스캔 수정

- 기준: 2026-10-24, `dev`의 `26e28ad`에서 만든 `fix/dangling-edge-index-scan` 작업 트리.
- 대상: [PERM-07 조회 인덱스 후속 사항](2026-10-17-perm-07.md#별도-후속-사항), [DATA-05](../development-plan/data.md#data-05), [불변식 주변부 조회](../sdd/data.md#그래프-불변식-규칙과-감사).
- 판정: 해당 후속 사항 해결. DATA-10·SEARCH-10의 전체 인수·성능 측정이나 개발 작업 수를 변경하지 않는다.

## 재현과 수정

Go 1.27.1 darwin/arm64, PostgreSQL 18.6·AGE 1.8.0·pgvector 0.8.6 Linux aarch64에서 정식 마이그레이션 6개를 적용했다. 기존 개발 DB와 분리한 임시 컨테이너·tmpfs·합성 자격·임시 loopback 포트를 사용했다. 이미지 ID는 `2b6d51048c03`이며 외부 API를 호출하지 않았다.

수정 전 `TestQueryIndexPlansIntegration/10000`을 필수 DB·race 조건으로 실행해 같은 실패를 재현했다. 그래프마다 이웃 10,000개인 두 허브에서 고아 간선 보충 조회가 `DERIVED_FROM_start_id_idx`로 간선 20,000개 전체를 읽고 19,999개를 사후 필터로 제거했다. 인덱스가 존재해도 두 끝점의 `OR` 조건이 조회 범위를 좁히지 못했다.

`loadDanglingAuditEdges`의 범위 조회를 `start_id`·`end_id`별로 나눠 `UNION ALL`로 합쳤다. 범위 정점 ID는 같은 SQL의 materialized CTE에서 한 번 구해 모든 방향·label이 재사용한다. 고아 간선은 살아 있는 끝점만 범위에 포함되므로 두 방향에서 중복되지 않는다. 기존 그래프 전체 감사 조건과 단일 문장·스냅숏 경계는 유지했다. 인덱스·스키마·설정 변경은 없다.

기존 고아 간선 시험에 반대 방향을 추가했다. 정상 간선의 양 끝을 범위에 포함한 상태에서도 고아 간선만 정확히 한 번 보충하는지, 쓰기 검사와 전체 감사의 위반 결과가 같은지 확인했다.

## 최종 검증

- 조회 인덱스·불변식·홉 실행 계획의 관련 모듈 시험 116건(하위 사례 포함)이 필수 DB·race 조건에서 실패·건너뜀 없이 통과했다.
- 1,000개 표본의 보충 조회: Context 스캔 3·간선 스캔 1·공유 hit 25·0.115ms.
- 10,000개 표본의 보충 조회: Context 스캔 4·간선 스캔 1·공유 hit 28·0.145ms. `DERIVED_FROM_start_id_idx`·`DERIVED_FROM_end_id_idx`를 각각 사용했고 간선의 사후 탈락 행은 0이었다.
- 두 표본 모두 반환 행 0·임시 I/O 0이다. 수치는 `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF)`의 단일 실행 표본이며 반복 측정한 성능 비교나 운영 지연 보장이 아니다.
- `check-go.sh`의 의존성 검증·전체 build·vet·gofmt가 통과했다.

```sh
TEST_DATABASE_URL='<이번 시험 전용 합성 DB URL>' TEST_DATABASE_REQUIRED=1 AGE_GRAPH_NAME=agent_context GOTOOLCHAIN=local \
  /Users/alphagone/sdk/go1.27.1/bin/go test -race -json -count=1 -timeout=240s \
  -run '^Test(Query.*|Invariant.*|HopHubQueryPlansIntegration)$' ./internal/store
GO_BINARY=/Users/alphagone/sdk/go1.27.1/bin/go sh ./check-go.sh
```

원시 결과는 `/tmp/acs-queryindex-recheck-10000.log`, `/tmp/acs-dangling-fix-final-tests.json`, `/tmp/acs-dangling-fix-static.log`에 있다. 최초 방향 분리만 적용한 결과도 통과했으나 범위 정점 반복 조회가 남아 CTE로 보완한 뒤 최종 범위를 다시 실행했다. 반복 실행을 시험 수에 중복 합산하지 않았다.

재현·수정 검증에 사용한 두 임시 컨테이너를 제거했고 작업 label의 잔류 컨테이너가 없음을 확인했다. 기존 개발 DB·WAL 정리 컨테이너는 보존했다. 변경은 미커밋 상태다.

DOX pass에서 소유권·패키지 경계·서비스 계약·시험 시점의 변경이 없음을 확인했다. 기존 방향별 주변부 조회 계약을 따르므로 SRS·SDD·DOX·용어집은 유지하고 검토·요청·근거와 후속 사항 참조를 갱신했다. 전체 저장소·운영 DB·다른 PostgreSQL 판의 실행 인수는 이번 범위가 아니다.
