# 최초 인증 시각 승계 회귀 검증

## 대상과 수정

GitHub 이슈 [#55](https://github.com/Alpha-gone/agent-context-sharing/issues/55)의 `auth_time` 초기화를 수정했다. 코드 교환이 현재 시각을 최초 인증 시각으로 사용하여, 수명 12시간인 웹 세션으로 늦게 재인가하면 접근 토큰의 갱신 한도가 다시 12시간 연장됐다.

`FR-AGENT_CONTEXT-092`·`133`의 기존 한도를 유지하도록 SDD를 먼저 보완했다. 검증된 웹 세션의 `auth_time`을 웹 연결 어댑터와 세션 판정 경계를 거쳐 인가 코드의 `authenticated_at`에 저장하고, 교환·갱신 시 같은 시각을 승계한다. 요청 파라미터의 시각은 신뢰하지 않는다. 최초 인증부터 12시간 이상이면 코드 발급·교환·갱신을 거부하며 재인가 요청은 로그인 화면으로 보낸다. 기존 토큰의 원래 만료 시각과 1시간 수명·10초 갱신 구간·DPoP 키 결합은 바꾸지 않는다.

`008_authorization_code_auth_time.sql`은 nullable 열을 추가한다. 기존 코드에는 최초 인증 시각의 근거가 없으므로 값을 추정하여 채우지 않고 미소비 코드의 교환만 거부한다. 소비된 구형 코드의 재사용 감지와 발급 토큰 폐기는 유지한다. 기존 SQL 파일과 적용 이력의 checksum은 변경하지 않았다. 기존 임베딩 전환 시험은 자기 대상인 001·007 파일만 입력하도록 고정하여 후속 마이그레이션과 시험용 복귀 파일 번호의 충돌을 없앴다.

## 재현과 회귀

- 교환 로직 수정 전 약 11시간 59분 전 로그인 시각을 전달한 회귀에서 발급 토큰의 `auth_time`이 교환 시각으로 바뀌어 실패했다. 수정 뒤 반복 재인가·교환도 같은 최초 인증 시각을 보존한다.
- 실제 서명 웹 세션의 검증, HTTP 인가, 코드 교환과 갱신에서 시각·DPoP 키 승계를 확인했다. 어댑터 발급·검증과 웹 세션 판정도 실제 DB를 사용해 별도로 확인했다.
- 고정 시각으로 12시간 직전·정확한 경계·초과를 검사했다. 누락·Unix 0·미래 시각도 거부한다. 세션 판정이 성공했더라도 시각이 잘못되면 로그인으로 보내며 원래 인가 요청을 `next`에 보존한다.
- 아직 수명이 남은 코드라도 인증 한도가 지나면 `invalid_grant`로 거부하고 소비하지 않는다. 코드 발급 뒤의 인증 시각도 거부한다. 한도를 지난 기존 접근 토큰은 원래 `exp`까지 검증되지만 갱신되지 않는다.
- 실제 PostgreSQL 왕복의 최초 인증 시각 보존, NULL인 구형 코드 조회와 소비된 구형 코드의 재사용 감지를 확인했다.
- 빈 시험 DB에 001·007·008을 적용하고 두 번째 실행에 남은 파일이 없음을 확인했다. 별도 회귀는 기존 코드가 있는 상태에서 008을 적용하고 NULL 유지, `public`의 nullable `timestamptz`와 SQL 자체의 재적용을 확인했다.

## 검증 결과

Go 1.27.1과 실행 중인 PostgreSQL·AGE 개발 DB 인스턴스에서 새로 만든 격리 시험 DB를 사용했다. 접속 정보는 `.env`에서 값 노출 없이 읽었으며 모든 최종 DB 실행에 `TEST_DATABASE_URL`·`TEST_DATABASE_REQUIRED=1`을 설정했다.

```shell
go test -race ./internal/authz ./internal/store ./internal/web ./internal/migrate ./cmd/server \
  -run 'AuthenticationTime|RecentAuthentication|WebSessionAuthentication|AuthorizationCodeIntegration|ExpiredCodeReuseReportsUseIntegration|CodeReuse|RenewRequiresVerified' -count=10
# 서버 루트
go test -race -json ./... -count=1
# client/에서
go test -race ./... -count=1
# 서버 루트와 client/에서 각각
go build ./...
go vet ./...
# 별도 시험 DB를 사용하는 실제 서버·클라이언트 시험
GO_BIN=<Go 1.27.1 경로> sh client/internal/client/test-live.sh
# 저장소 루트
gofmt -l .
git diff --check
```

관련 회귀는 race 검사로 10회 통과했다. 서버 전체 테스트 항목 687개에 실패·건너뛰기·race 오류가 없었으며 테스트 파일이 없는 세 패키지에만 패키지 단위 skip 이벤트가 있었다. 클라이언트 전체 race 테스트와 양쪽 모듈 build·vet도 통과했다. `client_live` fixture의 발급 토큰·갱신 구간 두 경로를 포함한 실제 TLS·인가·DPoP·MCP·AGE·클라이언트 종단 간 시험이 건너뛰기 없이 통과했다. 그 시험의 브라우저·세션·임베딩은 대역이므로 실제 시스템 브라우저나 로그인 UI 수동 시험으로 보지 않는다. 형식 미정리 파일과 diff 공백 오류는 없었다.

처음 추가한 시험 fixture의 잘못된 로그인 아이디와 audience 식별자, 웹 서버의 필수 구성을 바로잡고 재실행했다. 초기 DB 접속 정보가 없는 단위 실행에서 건너뛴 DB 계층은 최종 격리 DB 실행에서 모두 확인했다.

## DOX와 범위

루트, `internal/web`·`internal/mcp`·`migrations`, `spec`→`spec/service`→`agent-context` DOX 체인을 재확인했다. 웹 경계의 입력·출력 계약이 바뀌므로 `internal/web/AGENTS.md`에 최초 인증 시각 전달을 명시했다. SDD·개발 계획·요청 기록을 동기화하고 한국어 표현, 용어·맞춤법과 상대 링크를 점검했다. 요구사항 자체·용어 정의·서비스 소유권·마이그레이션 절차·MCP 계약·디렉터리 구조는 바뀌지 않아 SRS·DICTIONARY·그 밖의 AGENTS와 Child DOX Index는 유지했다. 외부 자료를 새로 사용하지 않아 `reference.md`도 변경하지 않았다.

기존 개발 DB·볼륨·운영 DB·설정·비밀·배포는 변경하지 않았다. 새로 만든 시험 DB와 그 합성 데이터는 검증 후 정리했다. 외부 임베딩 API 호출·운영 부하 시험·커밋·푸시·PR 생성·GitHub 이슈 종료는 수행하지 않았다. 기존 환경에서 수정한 서버를 사용하기 전 008 마이그레이션이 적용되어 있어야 한다.
