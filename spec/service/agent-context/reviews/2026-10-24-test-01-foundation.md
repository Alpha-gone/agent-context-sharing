# TEST-01 시험 기반 착수: 저장소의 격리 DB

- 기준: 2026-10-24, PR #144 병합 뒤 `dev`의 `fb06de4`에서 만든 `feat/test-foundation` 작업 트리.
- 대상: [TEST-01](../development-plan/testing.md#test-01)의 DB 실행 정책·시험별 격리·실패/취소 정리 기반.
- 판정: 첫 DB 기반 모듈 검증 통과, TEST-01 전체는 진행 중이다.
- 후속: 이 기록의 착수 변경은 `a67afe1`로 커밋했으며 [같은 날 TEST-01 모듈 인수](2026-10-24-test-01.md)에서 남은 기반을 완료했다. 아래 미완료 표기는 최초 검토 시점의 상태다.

## 구현 범위

저장소 시험의 `TEST_DATABASE_URL`·`TEST_DATABASE_REQUIRED` 판정을 별도 도우미로 분리했다. URL이 없으면 선택 실행은 건너뛰고, 필수 변수가 비어 있지 않으면 실패한다. 네 하위 프로세스에서 선택 미설정·필수 미설정·문자열 `0`인 필수 설정·URL 제공을 대조했다. 예상한 건너뜀 1회와 실패 2회는 부모 시험이 결과를 검사하는 표본이며 실제 검증 실행의 건너뜀·실패가 아니다.

서명 키 시험의 전용 DB 도우미를 저장소 시험에 재사용할 수 있도록 분리했다. 공유 DB는 생성·삭제 권한을 행사하는 관리 연결로만 사용한다. 해석한 pgx 설정을 복제해 DB 이름을 바꾸고 모든 연결에서 `current_database()`를 확인한 뒤 AGE 연결 준비를 수행한다. 스키마는 정식 마이그레이션으로만 준비하며 저장소 연결 예약도 함께 구성한다.

DB 생성 직후 정리를 등록하고 연결 풀→전용 DB→관리 풀 순서로 닫는다. DB 삭제에는 취소와 독립된 10초 제한 컨텍스트를 사용한다. 두 병렬 표본의 DB 이름이 다르고 실제 그래프 저장·조회가 가능하며 시험 종료 때 취소된 컨텍스트와 무관하게 DB가 제거되는 것을 확인했다. 의도적인 `t.Fatal`을 실행한 하위 프로세스의 DB도 제거됐다.

## 실행과 결과

Go 1.27.1 darwin/arm64, PostgreSQL 18.6·AGE 1.8.0·pgvector 0.8.6 Linux aarch64의 기존 검증 이미지 `sha256:2b6d51048c03dae147064683f5b078690f1525365d561fd80b78f74de8c3c697`을 사용했다. `acs-test01-foundation` 컨테이너·tmpfs·임시 loopback 포트와 합성 자격을 생성했다. 기준 DB에는 스키마를 사전 적용하지 않아 시험 도우미가 공유 스키마를 요구하지 않는지도 확인했다.

```sh
TEST_DATABASE_URL='<이번 시험 전용 합성 DB URL>' TEST_DATABASE_REQUIRED=1 AGE_GRAPH_NAME=agent_context GOTOOLCHAIN=local \
  /Users/alphagone/sdk/go1.27.1/bin/go test -race -json -count=1 -timeout=240s \
  -run '^Test(IntegrationDatabasePolicy|IsolatedIntegrationStore.*Integration|SigningKeyIntegration|RotateSigningKeyIntegration)$' ./internal/store
GO_BINARY=/Users/alphagone/sdk/go1.27.1/bin/go sh ./check-go.sh
```

- 새 정책·병렬 격리·실패 정리와 영향받은 서명 키 시험 12건(하위 사례 포함)이 실패·건너뜀 없이 통과했다.
- 첫 실행의 도우미 이동 후 미사용 import로 발생한 빌드 실패를 제거하고 같은 범위를 다시 실행했다. 성공한 최종 실행만 결과에 포함한다.
- 전체 의존성 검증·build·vet·gofmt가 통과했다.
- 기준 DB의 `public` 테이블은 실행 전후 0개이고 `store_test_` 접두사의 시험 DB는 남지 않았다.
- 시험 컨테이너를 제거하고 작업 label의 잔류 컨테이너가 없음을 확인했다. 기존 개발 DB·WAL 정리 컨테이너는 유지했다.
- 문서 링크·앵커·코드 울타리와 `git diff --check`가 통과했다.
- 원시 결과는 `/tmp/acs-test01-foundation-tests.json`, `/tmp/acs-test01-foundation-static.log`에 있다.

## 남은 범위와 DOX pass

TEST-01의 합성 표본, 시계·장애·동시 커밋 도우미, 다른 패키지의 접속·권한 부족·준비 실패 정리 점검은 후속 범위다. TEST-02~09와 분야별 전체 통합 인수를 완료 처리하지 않으며 전체 93개 완료·1개 진행·30개 미착수다.

업무 코드·패키지·스키마·서비스 동작과 시험 시점 계약은 바뀌지 않았다. 루트·내부·명세 DOX와 SRS·SDD는 기존 책임을 유지하고 개발 계획·검토·요청·근거 기록만 갱신한다. 기존 IDE 파일과 개발 DB는 보존했다. 변경은 미커밋 상태다.
