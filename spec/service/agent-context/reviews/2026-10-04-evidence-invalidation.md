# 근거 무효 표시 전파 회귀 검증

## 대상과 수정

GitHub 이슈 [#49](https://github.com/Alpha-gone/agent-context-sharing/issues/49)의 `invalidateDerivedEvidence` 동시 갱신 유실을 확인했다. 전파 대상의 판을 읽은 뒤 다른 트랜잭션이 파생을 갱신하면, 옛 판을 조건으로 한 AGE `SET`이 0행이어도 성공하여 전파를 건너뛰었다.

조건부 갱신에 `RETURN node`를 붙여 실제 반환 행을 확인한다. 0행이면 같은 트랜잭션에서 현재 파생 판을 읽어 `VersionConflictError`를 반환한다. AGE 동시 갱신 오류도 기존 변환 경로를 통해 판 충돌로 반환한다. 호출자의 폐기·대체 트랜잭션은 롤백되어 실패한 적용을 남기지 않으며 재시도는 호출자의 책임이다. 공개 API·스키마·의존성은 변경하지 않았다.

## 재현과 회귀

- 실제 AGE 조회 결과를 모두 읽은 직후 다른 연결에서 파생 갱신을 커밋하도록 테스트 트랜잭션을 감쌌다. 프로덕션 코드에는 시험용 분기나 지연을 추가하지 않았다.
- 수정 전 원천 폐기·파생 폐기·파생 대체 세 경로 모두 전파가 0행인데 성공을 반환하여 테스트가 실패했다.
- 수정 후 세 경로가 현재 파생 판을 담은 충돌로 끝났다. 근거 폐기 상태, 대체의 새 정점·색인 작업과 적용 기록이 남지 않음을 확인했다.
- 재시도는 동시 갱신의 본문을 보존하고 판을 하나 증가시켜 무효 표시만 켰다. 활성 파생은 자동 폐기되지 않았고 간접 파생에는 표시가 전파되지 않았다. 표시 뒤 옛 판으로 수정하는 반대 순서도 충돌로 거부됐다.
- 별도 회귀에서는 미커밋 파생 갱신과 전파를 겹쳤다. `pg_blocking_pids`로 실제 잠금 대기를 확인한 뒤 먼저 갱신을 커밋했으며, 전파는 내부 오류가 아닌 판 충돌로 끝났다. 근거 폐기는 롤백되고 재시도 후 새 본문·무효 표시·판 번호가 보존됐다.

## 검증 결과

Go 1.27.1과 실행 중인 PostgreSQL·AGE 개발 DB를 사용했다. DB 실행에는 `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED=1`을 설정했다.

```shell
go test -race ./internal/store -run '^(TestEvidenceInvalidation|TestConcurrentDiscard|TestConcurrentUpdate|TestStoreIntegration)' -count=10
# 서버 루트와 client/에서 각각 실행
go test ./...
go build ./...
go vet ./...
# 저장소 루트
gofmt -l .
git diff --check
```

신규 회귀와 기존 컨텍스트 동시 폐기·갱신·핵심 저장소 회귀는 race 검사와 함께 10회 통과했다. 서버 전체 테스트는 DB 통합 계층을 포함해 통과했고 클라이언트 전체 테스트, 양쪽 모듈 build·vet도 통과했다. 서버 전체 `go test -json ./...`도 확인했으며 테스트 항목의 건너뛰기는 없었다. 테스트 파일이 없는 세 패키지의 `skip` 이벤트만 있었다. gofmt 미정리 파일과 diff 공백 오류는 없었다. 별도 `embedding_live` 태그의 실제 제공자 시험과 운영 부하 시험은 실행하지 않았다.

## DOX와 범위

루트와 `spec/`→`spec/service/`→`agent-context/` DOX 체인, 용어와 한국어 맞춤법·띄어쓰기를 점검했다. 기존 SDD의 한 단계 전파·트랜잭션 원자성·충돌 거부를 지키는 구현 수정이므로 SRS·SDD·AGENTS·Child DOX Index는 변경하지 않았다. 그래프 불변식의 새 규칙은 추가하지 않았으며 과거에 유실된 표시의 탐지·일괄 복구는 이번 검증 범위가 아니다.

합성 시험 데이터를 개발 DB에 기록했다. DB 초기화·볼륨 삭제·외부 임베딩 API 호출·운영 배포·커밋·푸시·PR 생성·GitHub 이슈 종료는 수행하지 않았다.
