# 클라이언트 3단계 구현 검토

- 검토일: 2026-10-03
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

현재 수행한 단위·모의 계약·race 시험에서 확인한 미해결 결함은 없다. 아래 미검증 경계는 완료로 판정하지 않는다.

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
