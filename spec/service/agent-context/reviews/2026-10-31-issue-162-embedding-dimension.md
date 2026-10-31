# #162 후속: 차원 전환 시험의 제공자 차단 가정

- 대상: [PR #227](https://github.com/Alpha-gone/agent-context-sharing/pull/227)이 알려진 기존 실패로 분리한 `TestEmbeddingDimensionMigrationIntegration`. 원래 [검토](2026-10-31-issue-162.md)의 당시 검증 기록은 보존한다.
- 기준: `dev`의 `a3e17c5`, 브랜치 `fix/embedding-dimension-worker-block`.

## 원인과 변경

시험은 429 표본을 처리한 같은 Worker로 400 표본을 즉시 처리한다고 기대했다. [제공자 장애 중 작업 확보 차단](../sdd/indexing.md#제공자-장애-중-작업-확보-차단) 계약은 429를 관측한 Worker의 새 작업 확보를 지터가 적용된 탐침 시각까지 중단한다. 따라서 400 회차가 `processed=false`를 반환한 것은 구현 결함이 아니다. 이 계약을 도입한 `3f75e89` 이후 시험 fixture가 갱신되지 않은 것이 원인이다.

차단 상태는 계약상 Worker 메모리에 있으므로 장애 표본마다 같은 구성의 새 Worker를 만들어 처리한다. 각 표본의 시도 횟수·pending/failed 상태·비밀 비노출 검증과 768차원 재색인·검색·복귀 흐름은 유지한다. 차단 자체의 확보 중단·탐침·회복은 색인 패키지의 기존 회로 시험이 담당한다. 구현·SQL·스키마·SDD는 변경하지 않았으며 DOX pass에서 소유권·경로 변경이 없어 기존 DOX를 유지했다.

## 재현과 검증

- Go 1.27.1과 고정 PostgreSQL/AGE 이미지 `sha256:2b6d51048c03dae147064683f5b078690f1525365d561fd80b78f74de8c3c697`(PostgreSQL 18.6)의 전용 합성 컨테이너를 사용했다. 기존 개발·다른 시험 DB는 사용하지 않았다.
- 수정 전 단일 시험에서 `provider_blocked provider_status=429 retry_after_seconds=44` 기록 직후 `configuration_test.go:305: 장애의 작업 처리: <nil>` 실패를 재현했다.
- 수정 후 `TEST_DATABASE_REQUIRED=1 go test -race -p 1 -json ./internal/migrate -count=1 -timeout=15m`에서 84건이 실패·건너뜀 없이 통과했다. PR #227의 83건 통과·1건 실패와 같은 시험 집합이다.
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `git diff --check`와 변경 문서의 로컬 링크·앵커를 확인했다.
- 시험 종료 후 소유 컨테이너만 정리했으며 기존 개발·다른 시험 자원과 개인 IDE 파일은 유지했다.
