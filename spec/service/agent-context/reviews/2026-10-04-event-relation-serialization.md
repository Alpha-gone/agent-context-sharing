# 사건 폐기와 관계 상태 전이 경합 검증

## 대상과 수정

GitHub 이슈 [#50](https://github.com/Alpha-gone/agent-context-sharing/issues/50)의 사건 폐기와 관계 확정·폐기 경합을 실제 PostgreSQL·AGE 개발 DB에서 재현했다. 사건 폐기는 관계 상태를 읽고 바꾼 뒤에야 활동 시각·저장량 갱신에서 그래프 행을 잠갔다. 관계 연산의 미커밋 상태를 보지 못해 확정 관계가 남거나 같은 간선 갱신에서 AGE `XX000` 오류가 발생했다.

사건 폐기·복구는 관계 연산과 같은 그래프 행을 상태 판정과 AGE 갱신 전에 잠근다. 잠금을 얻은 뒤 사건을 다시 읽어 대기 중 커밋된 본문·판·폐기 상태를 사용한다. 사건 갱신도 그래프를 AGE 정점보다 먼저 잠가 폐기와의 잠금 순서를 맞춘다. 웹 삭제·복구는 동일한 `changeContextDeletion` 경로를 사용한다. 원천·파생의 낙관적 충돌 처리, 공개 API·스키마·의존성은 변경하지 않았다.

## 재현과 회귀

`TestEventDeletionSerializationIntegration`은 선행 연산을 바깥 트랜잭션 안에서 실행하고 저장점만 해제한 뒤, 후행 연산을 별도 연결에서 실행한다. `pg_blocking_pids`로 실제 DB 잠금 대기를 확인한 다음 선행 트랜잭션을 커밋한다. 임의 지연으로 실행 순서를 추정하거나 프로덕션 코드에 시험용 분기를 추가하지 않았다.

- 수정 전 관계 확정이 먼저 실행된 양 끝점 표본에서 사건 폐기 후에도 관계가 `confirmed`로 남았다.
- 수정 전 관계 폐기가 먼저 실행된 양 끝점 표본에서 사건 폐기가 AGE `XX000` 내부 오류로 실패했다.
- 수정 후 관계 연산이 먼저 커밋되면 사건 폐기가 최신 상태를 읽어 확정 관계를 함께 폐기한다. 사건 폐기가 먼저 커밋되면 관계 확정은 `ErrInvalidRelation`, 관계 재폐기는 `ErrInvalidState`로 거부된다. 관계에는 판 번호가 없으므로 판 충돌 오류를 새로 도입하지 않았다.
- 사건 갱신이 먼저 커밋되면 폐기는 새 본문을 보존하고 판을 한 번 더 증가시킨다. 폐기가 먼저 커밋되면 옛 판 갱신은 현재 판을 담은 `VersionConflictError`로 거부된다.
- 양 끝점과 두 실행 순서의 총 12개 표본에서 폐기된 사건의 확정 관계가 남지 않았다. 사건을 복구해도 관계는 자동 재확정되지 않았다.

## 검증 결과

Go 1.27.1을 사용했고 DB 테스트에는 `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED=1`을 설정했다.

```shell
go test -race ./internal/store -run '^(TestEventDeletionSerialization|TestConcurrent.*Relation|TestReconfirmDiscardedRelation|TestEvidenceInvalidation|TestConcurrentDiscard|TestConcurrentUpdate|TestStoreIntegration)' -count=10
# 서버 루트와 client/에서 각각 실행
go test ./...
go build ./...
go vet ./...
# 서버 루트: JSON 결과에서 실패와 테스트 건너뛰기를 별도로 확인
go test -json ./...
# 저장소 루트
gofmt -l .
git diff --check
```

신규 12개 경합 표본과 기존 관계·컨텍스트·근거 무효 표시 회귀가 race 검사로 10회 통과했다. 서버 전체 테스트는 DB 통합 계층을 포함해 통과했고 클라이언트 전체 테스트, 양쪽 모듈 build·vet도 통과했다. 최종 서버 JSON 결과에 실패나 테스트 항목의 건너뛰기는 없었다. 테스트 파일이 없는 세 패키지의 `skip` 이벤트만 있었다. gofmt 미정리 파일과 diff 공백 오류도 없었다. 별도 `embedding_live` 실제 제공자 시험과 운영 부하 시험은 실행하지 않았다.

## DOX와 범위

루트와 `spec/`→`spec/service/`→`agent-context/` DOX 체인, 용어, 한국어 맞춤법·띄어쓰기와 문서 정합성을 점검했다. SDD의 「판정의 직렬화」에 사건·관계 상태 전이의 공통 잠금과 재조회·잠금 순서를 명시하고 서비스 AGENTS에 정본 참조를 추가했다. 기존 `FR-AGENT_CONTEXT-076`의 관계 동시 폐기를 구현하는 수정이므로 SRS는 유지했다. 경계·소유권·하위 문서 구조는 바뀌지 않아 부모 AGENTS와 Child DOX Index도 유지했다.

끝점 활성 상태에 대한 새 불변식 감사 규칙과 과거에 남은 확정 관계의 탐지·일괄 복구는 추가하지 않았다. 합성 시험 데이터를 개발 DB에 기록했으며 DB 초기화·볼륨 삭제·외부 임베딩 API 호출·운영 배포는 수행하지 않았다.
