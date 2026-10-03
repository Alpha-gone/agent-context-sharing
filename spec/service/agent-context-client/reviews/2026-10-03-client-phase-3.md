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

초기 단위·모의 계약·race 시험에서는 발견하지 못했으나, 추가 진단에서 아래 두 구현 결함을 재현했다. 수정하지 않았으며 후속 단계의 구현으로 대신해 완료 판정하지 않는다.

- `DEF-20261003-001` — 중간, `client/internal/client/authorize/browser.go:160`, `opener_linux.go:18`: callback의 결과를 기다리기 전에 opener를 동기 호출한다. 정상 callback을 200으로 처리한 뒤 opener가 context 종료까지 기다리면 인가 전체가 약 2초 뒤 `context.DeadlineExceeded`로 끝나고 토큰 교환은 0회다. listener는 정리되지만 정상 인가 결과를 사용하지 못한다. opener 반환·오류와 callback·취소를 함께 처리해야 하며, 수정 시 opener goroutine·자식 프로세스의 수명과 결과 경합도 검증해야 한다. 실제 `xdg-open`·시스템 브라우저의 종료 대기 동작은 이번 시험에서 실행하지 않았다.
- `DEF-20261003-002` — 낮음, `client/internal/client/authorize/metadata.go:149`, `browser.go:46`, `browser.go:75`, `authorize.go:288`: 구성 URL `https://RESOURCE.TEST:443/mcp`는 Load와 정규화된 resource 발견을 통과하지만, 인가·토큰 교환의 resource와 JWT audience 검증에는 구성 원문을 사용한다. 검증한 보호 리소스 문서의 resource는 후속 상태에 보관하지 않는다. 서버는 resource를 정확한 문자열로 비교하므로 정상 인가를 완료할 수 없다. 엄격한 토큰 교환 대역은 `ErrAuthorization`, 정규 audience 토큰을 반환하는 대역은 `ErrProtocol`로 실패했고 토큰은 저장되지 않았다. 실제 서버의 `internal/authz/authz.go:251`, `:291`에서도 정확한 비교를 확인했다. 검증된 resource를 인가·교환·JWT·갱신 검증에 일관되게 사용하거나 기동 구성에서 비정규 표기를 거부하는 방식 중 하나를 확정해야 한다.

## 추가 설계 위험과 확인 필요 항목

- 중간, `client/internal/client/authorize/authorize.go:304`: `exp=현재+7200` 토큰을 저장한 뒤 `exp=현재+3600` 토큰 갱신이 도착하면 이전 토큰으로 교체되고 만료가 3600초 역행한다. 재현했으나 현행 `FR-AGENT_CONTEXT_CLIENT-012`의 「동시 응답에서는 마지막으로 정상 수신한 토큰을 사용」과 일치하므로 구현의 명세 위반과 구분한다. 더 짧은 exp를 무시하려면 SRS의 수신 순서 정책을 먼저 변경하고 SDD·동일 만료 시각의 교체 정책·시험을 정합화해야 한다. refresh token은 없으므로 짧아진 만료가 불필요한 재인가로 이어질 가능성이 있다.
- `Identity()`는 Credentials를 호출해 브라우저 인가를 시작할 수 있다. 토큰 만료 시점의 Identity 호출에서 실제 로컬 callback 대역의 opener 실행이 증가하는 것을 확인했다. `remote.Client`는 gate를 보유한 채 조회 시작과 응답 수신 뒤 Identity를 대조하므로 단순 연결하면 해당 잠금 동안 인가를 기다릴 수 있다. 실제 HTTP Source는 아직 없으므로 현재 운영 경로의 교착 결함으로 단정하지 않는다. 4단계에서 비대화형 주체·자격 증명 상태 조회와 명시적 인가 경로를 나누고, 잠금 밖 준비 뒤 응답 반영 시 재검증하는 설계를 검토해야 한다.
- 만료 판정에는 여유 시간이 없다. 만료 1ns 전에는 현재 토큰을 재사용하고 만료 시점에는 Identity가 인가를 시작하는 것을 시험했다. 현행 SDD의 「현재 시각이 만료 시각 이상」과 일치하며, 전송 지연 중 만료에 대비한 여유를 둘지는 4단계 연결 전에 별도로 결정해야 한다.
- WSL2 감지 시 opener는 `wslview`만 실행하며 대체 경로·사전 가용성 검사는 없다. 코드의 단일 의존은 확인했지만 실제 WSL2와 최신 Ubuntu 배포판의 기본 설치·패키지 제공 상태는 확인하지 않았다. 6단계에서 지원 배포판별 설치 가능성·필수 의존 안내·Windows 브라우저와 loopback 경로를 검증해야 한다.

## 검토 중 해결한 항목

- `wellKnown`에서 디코딩된 경로를 다시 조합하면 issuer의 `%2F`가 경로 구분자로 바뀔 수 있었다. escaped path를 보존하도록 보완하고 기본·루트·trailing slash·인코딩 경로의 회귀 시험을 통과했다.
- SDD 오류 표의 광범위한 callback·issuer 실패 문구를 명확히 했다. 잘못된 callback은 400 후 대기, 인가 거부는 `client_authorization`, metadata·JWT 계약 위반은 `client_protocol`, 통신 실패는 `client_transport`다. 토큰 교환 redirect는 인가 거부와 구분해 `client_protocol`로 고정했다.

## 남은 연결과 검증 경계

- 실제 MCP HTTP 어댑터가 없어 도전 판정 이후의 최대 한 번 재인가·재전송과 토큰 갱신·Source.Identity 연결은 아직 검증하지 않았다. 4단계에서 연결한다.
- `Identity`와 계정 고정은 실제 인증 조정자에서 시험했으나 2단계의 Source 대역 시험을 실제 인증·MCP HTTP 통합 결과로 대신하지 않는다.
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
- 이번 추가 검토는 코드·SRS·SDD·개발 계획의 계약과 체크박스를 변경하지 않았다. DOX 소유권·구조·child index도 바뀌지 않아 해당 `AGENTS.md`는 유지했다.
