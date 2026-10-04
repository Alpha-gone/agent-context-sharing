# 계정당 그래프 수 한도 회귀 검증

## 대상과 수정

GitHub 이슈 [#48](https://github.com/Alpha-gone/agent-context-sharing/issues/48)의 같은 계정 동시 그래프 생성 경합을 확인했다. 기존 구현은 Read Committed에서 잠금 없이 소유 그래프 수를 세므로, 서로 다른 멱등성 키의 트랜잭션이 미커밋 생성을 놓치고 함께 한도를 넘길 수 있었다.

한도가 있는 `CreateGraphWithOwner`는 생성 계정의 `account` 행을 `FOR UPDATE`로 잠근 뒤 별도 명령으로 그래프 수를 센다. 그래프와 소유자 등급을 저장하는 트랜잭션의 커밋까지 잠금을 유지하며, 멱등성 경로에서는 저장점 해제 뒤 바깥 트랜잭션의 커밋까지 유지한다. 한도가 없으면 추가 잠금과 집계를 하지 않는다. 공개 API·스키마·의존성은 변경하지 않았다.

## 검증 결과

Go 1.27.1과 실행 중인 PostgreSQL·AGE 개발 DB에서 검증했다. DB 실행에는 `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED=1`을 설정했다.

- 기존 동시 생성 테스트는 수정 전에도 3회 통과했다. 단순 동시 시작만으로는 미커밋 상태의 겹침을 보장하지 못했다.
- 새 회귀는 그래프가 한도보다 하나 적은 상태에서 첫 생성의 바깥 커밋을 보류하고, 다른 멱등성 키로 두 번째 생성을 실행한다. 수정 전에는 첫 커밋 전에 두 번째 생성도 성공하여 테스트가 실패했다.
- 수정 후에는 `pg_blocking_pids`로 두 번째 요청이 첫 트랜잭션의 잠금을 기다리는지 확인한 뒤 첫 생성을 커밋했다. 두 번째 생성은 `graphs_per_account` 한도 초과로 거부됐고 최종 소유 그래프 수는 한도와 같았다.
- 기존 테스트도 한도 직전 상태와 최종 그래프 수 검사로 보강했다. 동시 요청 5건 중 1건만 성공했다.

```shell
go test ./internal/store -run '^TestGraphCountLimit' -count=10
go test -race ./internal/store -run '^TestGraphCountLimit' -count=10
# 서버 루트와 client/에서 각각 실행
go test ./...
go build ./...
go vet ./...
# 저장소 루트
gofmt -l .
git diff --check
```

두 회귀 테스트는 일반 실행과 race 실행에서 각각 10회 통과했다. 서버 전체 테스트는 DB가 필요한 계층을 포함해 통과했고, 클라이언트 전체 테스트와 양쪽 모듈의 build·vet도 통과했다. gofmt 미정리 파일과 diff 공백 오류는 없었다. 별도 `embedding_live` 태그의 실제 제공자 시험과 운영 부하 시험은 실행하지 않았다.

## DOX와 범위

루트와 `spec/`→`spec/service/`→`agent-context/` 계약을 확인했다. SDD의 트랜잭션 경계·판정 직렬화·동시성 검증, 서비스 AGENTS와 공식 근거를 동기화했다. 요구사항·패키지 경계·문서 소유권·Child DOX Index는 바뀌지 않아 SRS와 부모 AGENTS는 그대로 유지했다. 한국어 맞춤법·띄어쓰기·문서 참조와 Markdown 구조를 점검했다.

검증은 합성 시험 데이터를 개발 DB에 기록한다. DB 초기화·볼륨 삭제·외부 임베딩 API 호출·운영 배포는 하지 않았다. 커밋·푸시·PR 생성·GitHub 이슈 종료는 수행하지 않았다.
