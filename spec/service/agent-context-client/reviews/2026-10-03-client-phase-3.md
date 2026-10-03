# 클라이언트 3단계 구현 검토

- 검토일: 2026-10-03
- 추가 검토 기준 커밋: `ae1b988`
- 범위: `authorize`의 발견·공유 브라우저 인가·JWT·DPoP·갱신과 `serve`의 키 수명
- 기준: [SRS](../SRS.md), [SDD](../SDD.md), [개발 계획](../DEVELOPMENT_PLAN.md)

## 확인한 구현

- HTTPS·resource·단일 issuer·S256·ES256·scope·none·응답 issuer·공개 JWKS를 검증한다. discovery redirect는 3회까지, 토큰 POST는 0회이며 비밀 헤더를 전달하지 않는다.
- 실제 IPv4·IPv6 루프백에 callback을 받고 주소·포트·경로·state·iss·단일 값을 대조한다. 잘못된 요청은 정상 callback을 소비하지 않고, 소비 뒤 중복은 수신기 수명 안에서 거부한다. 코드와 PKCE·redirect·resource를 한 번 교환한다.
- 단일 인가·100명 공유·128명 상한과 대기자별 취소를 시험했다. 100명에게 같은 거부 결과가 전달되며 재교환하지 않는 것도 확인했다. 마지막 취소·전체 시간 초과·거부·opener 실패·Close 뒤 callback 포트가 닫혔음을 확인했다.
- 표준 암호 라이브러리로 ES256 proof를 검증하고 100개 UUIDv7 jti의 고유성, htm·htu·iat·ath와 공개 JWK 결합을 확인했다. 토큰 검증의 서명·issuer·audience·만료·nbf·thumbprint·알고리즘·kid·critical 확장·중복 키·개인 JWK 거부를 시험했다.
- 갱신 헤더 쌍·단일성·JWT exp 일치를 확인하고 잘못된 갱신·취소 응답은 현재 토큰을 유지한다. 늦은 401이 새 토큰을 지우지 않으며 동일 계정의 만료 후 재인가에서 주체 식별자가 유지된다.
- 첫 계정 결합을 고정하고 다른 계정의 재인가 토큰을 저장하지 않는다. 정상 계정으로 재인가하면 복구되며 계정을 바꾸려면 프로세스를 재시작한다.
- 기본 문자열·JSON·오류 표현에 비밀을 출력하지 않으며 `serve`에서 구성 검사 뒤 조정자를 만들고 종료 때 참조를 폐기한다.
- discovery·본문 수신·토큰 교환의 통신 실패는 `ErrTransport`, OAuth 비성공 응답은 `ErrAuthorization`, discovery 실패 응답과 토큰 redirect는 `ErrProtocol`로 구분하고 내부 오류·응답 본문을 노출하지 않는다.

## 미해결 결함

추가 진단에서 확인한 두 구현 결함은 아래 수정과 회귀 검증으로 해결했다. MCP HTTP 연결은 후속 4단계에서 검증했으며 실제 서비스 서버·시스템 브라우저·WSL2 검증은 별도 미완료 경계로 유지한다.

## 해결된 결함

위치의 기존 줄 번호는 추가 검토 기준 커밋 `ae1b988`의 재현 지점을 가리킨다.

- `DEF-20261003-001` — 중간, `client/internal/client/authorize/browser.go:160`, `opener_linux.go:18`: 정상 callback 뒤 opener가 반환하지 않으면 시간 초과·0회 교환으로 끝났다. opener를 별도 실행하고 callback·실행 오류·취소를 함께 기다리도록 수정했다. 이미 받은 callback은 실행기 종료 오류보다 우선하며, 모든 반환 경로에서 opener context 취소·실행 회수 후 listener를 정리한다. 기본 명령의 출력은 폐기하고 `WaitDelay`로 출력 복사 대기를 제한한다. `TestCallbackDoesNotWaitForOpenerExit`, `TestBlockedOpenerCancellationAndIdentity`, `TestBrowserCommandCancellation`에서 정상 callback의 1회 교환·미반환 실행 회수·취소·시간 초과·종료를 검증했다. 실제 `xdg-open`·시스템 브라우저는 실행하지 않았다.
- `DEF-20261003-002` — 낮음, `client/internal/client/authorize/metadata.go:149`, `browser.go:46`, `browser.go:75`, `authorize.go:288`: 구성 URL `https://RESOURCE.TEST:443/mcp`의 발견은 통과하지만 구성 원문 resource·audience가 서버의 정확한 비교와 충돌했다. 발견에서 검증한 보호 리소스의 resource 원문을 보관해 인가·토큰 교환·초기/갱신 JWT audience에 사용하도록 수정했다. 구성 URL은 전송 대상으로 유지하고 다른 리소스 거부는 완화하지 않았다. `TestValidatedResourceUsedThroughoutAuthorization`에서 비정규 구성의 코드 교환·JWT·갱신·보호 요청 헤더 적용을 검증했다.

## 추가 설계 위험의 처리와 남은 검증

- 갱신 만료 역행은 당시 `FR-AGENT_CONTEXT_CLIENT-012`의 마지막 정상 수신 정책과 일치했다. 이번 수정에서 SRS를 먼저 만료 비역행·동일 만료 마지막 검증·저장 정책으로 변경하고 SDD·구현을 정합화했다. `TestRefreshNeverRegressesExpiration`에서 7200초 뒤 3600초의 늦은 갱신 무시, 동일 만료 교체, 오래된 응답의 검증·계정 검사와 100개 동시 갱신의 최대 만료 유지가 통과했다.
- `Identity()`는 비대화형 조회로 변경했다. 토큰 부재·만료 여유·종료 시 `ErrAuthorization`을 반환하고 진행 중 인가도 기다리지 않는다. `Credentials`·`Authenticate`가 명시적 인가 경로를 유지한다. 초기·100개 만료 주체 조회·진행 중 opener 대기에서 브라우저 추가 실행이 없음을 시험했다. 후속 4단계에서는 HTTP Source가 gate 밖에서 자격 증명을 준비하고 gate 안에서는 비대화형 조회·적용만 하도록 연결했다.
- SRS·SDD에 5초 고정 만료 여유를 확정하고 자격 증명 사용·초기/갱신 저장에 적용했다. `TestIdentityIsNonInteractiveAndExpiryHasLeeway`, `TestTokensWithinExpiryLeewayAreRejected`에서 경계 직전 재사용, 경계의 재인가·비대화형 조회 거부와 임박한 초기/갱신 토큰의 `ErrProtocol` 분류·미저장을 검증했다. 서버의 10초 갱신 구간 안에서도 유효 시간이 5초보다 많이 남으면 기존 토큰을 사용할 수 있다.
- WSL2는 실행 파일을 조회해 `wslview`를 우선하고 조회가 `exec.ErrNotFound`일 때만 `powershell.exe`를 선택하도록 보완했다. 고정 PowerShell 명령문에 URL을 표준 입력으로 전달하며 다른 조회 오류나 선택한 실행기의 실행 실패 뒤에는 재시도하지 않는다. `TestLinuxBrowserCommandSelection`의 Linux·WSL 우선/대체·부재·조회 권한 오류·URL 비보간 시험과 Linux 교차 빌드가 통과했다. 실제 WSL2·배포판의 기본 설치 상태는 검증하지 않았으며 6단계의 Windows 브라우저·loopback 검증은 미완료다.

## 검토 중 해결한 항목

- `wellKnown`에서 디코딩된 경로를 다시 조합하면 issuer의 `%2F`가 경로 구분자로 바뀔 수 있었다. escaped path를 보존하도록 보완하고 기본·루트·trailing slash·인코딩 경로의 회귀 시험을 통과했다.
- SDD 오류 표의 광범위한 callback·issuer 실패 문구를 명확히 했다. 잘못된 callback은 400 후 대기, 인가 거부는 `client_authorization`, metadata·JWT 계약 위반은 `client_protocol`, 통신 실패는 `client_transport`다. 토큰 교환 redirect는 인가 거부와 구분해 `client_protocol`로 고정했다.

## 남은 연결과 검증 경계

- 후속 4단계에서 `remote.NewHTTP`에 실제 인증 조정자를 연결했다. 전송 대역과 실제 로컬 TLS 시험 서버·루프백 callback으로 최대 한 번 재인가·재전송, 토큰 갱신·Source.Identity·계정 변경 거부·정상 계정 복구를 검증했다. 결과는 [4단계 구현 검토](./2026-10-03-client-phase-4.md)에 기록하며 아래 3단계 시점의 검증 결과와 구분한다.
- 2단계의 Source 대역 시험과 4단계 로컬 TLS 시험 서버 결과를 실제 서비스 서버의 인증·MCP 종단 간 결과로 대신하지 않는다.
- 수신 측의 다른 키·DPoP proof 재생·토큰만 제시한 요청 거부는 실제 서버 통합 범위다. proof 생성·고유성 시험을 해당 수신 측 시험의 통과로 보고하지 않는다.
- macOS·Linux·WSL2 시스템 브라우저와 지원 호스트, 실제 인가 서버 로그인·TLS·MCP 종단 간 시험은 수행하지 않았다. `doctor`의 인가·진단 비노출도 후속 범위다.

## 검증 결과

- Go `1.27.1`: 클라이언트 `go test -race -count=1 ./...` 통과.
- 공유 인가·거부·마지막 대기자 정리·IPv6 fallback·128명 상한의 race 시험을 10회 반복해 통과했다.
- 서버·클라이언트 각각 `go build ./...`·`go vet ./...`, 서버의 `TestToolManifestMatchesClient`와 Linux amd64 클라이언트 교차 빌드를 통과했다. Linux·WSL2의 실제 실행 결과는 아니다.
- 저장소 전체 `gofmt -l .`과 `git diff --check`에 이상이 없으며 변경 문서의 상대 링크와 SRS→SDD 요구사항 51건의 추적성을 확인했다. 한국어 표현·맞춤법과 DOX 소유권·child index를 검토했다.
- 외부 로그인·시스템 브라우저는 실행하지 않았으며 HTTP는 주입된 transport, callback은 실제 로컬 listener로 검증했다.
- 서버 전체 회귀·데이터베이스 통합 시험은 이번 범위에서 실행하지 않았다. 서버 구현은 변경하지 않았다.
- 단계 상태는 진행 중이며 개발 계획의 미충족 작업·완료 기준은 미완료로 유지한다.

### 추가 진단 결과

- 저장소 구현을 변경하지 않는 임시 Go overlay에서 `TestReviewBlockedOpener`, `TestReviewRefreshExpirationRegression`, `TestReviewNonCanonicalResource`, `TestReviewExpiryHasNoMarginAndIdentityStartsFlow`를 Go `1.27.1`의 `-race -count=1`로 실행했다. 외부 HTTP는 대역, callback은 실제 IPv4 로컬 listener였다.
- 진단 시험은 현재 실패·만료 역행 동작을 관찰하도록 작성했다. 시험 명령의 성공을 결함 수정 또는 정상 동작의 통과로 해석하지 않는다. 정상 callback 뒤 시간 초과·0회 교환, 3600초 만료 역행, 비정규 resource의 두 실패 분류, 만료 직전 재사용과 Identity의 인가 시작을 확인했으며 race 보고는 없었다.
- 최초 resource 재현은 기존 fixture의 정규 resource 선검증도 함께 실패했다. 해당 선검증과 분리한 opener 대역으로 재실행해 실제 인가·토큰·audience 경로의 오류 분류를 확인했다.
- 이 진단 기록 당시에는 코드·SRS·SDD·개발 계획의 계약과 체크박스를 변경하지 않았다. 이후 수정 결과는 위 해결된 결함과 아래 보완 검증에 기록한다.

### 수정 보완 검증

- Go `1.27.1`에서 클라이언트 전체 `go test -race ./... -count=1`과 주요 신규 회귀·공유 인가·정리·상한 시험의 `-race -count=10`을 통과했다. 실제 브라우저 대신 주입 opener, HTTP transport 대역, 실제 로컬 listener와 시험 실행기 프로세스를 사용했다.
- 양쪽 모듈 `go build ./...`·`go vet ./...`, 서버 `TestToolManifestMatchesClient`, Linux amd64 교차 빌드를 통과했다. 서버 전체·데이터베이스·실제 인증·MCP Source·WSL2·호스트 종단 간 시험을 통과한 것으로 보고하지 않는다.
- 저장소 전체 `gofmt -l .`, `git diff --check`, 변경 문서 6개의 상대 링크와 요구사항 51건의 추적성을 확인했다. 한국어 표현·맞춤법을 검토했다.
- 인증 패키지와 서비스 명세의 소유 `AGENTS.md`에 새 주체 조회·갱신·만료·opener 수명 계약을 반영했다. 패키지 소유권·지속 구조·Child DOX Index는 바뀌지 않아 루트·상위 문서는 유지한다. 개발 계획의 후속 연결·실제 환경 미충족 기준은 미완료로 유지한다.
