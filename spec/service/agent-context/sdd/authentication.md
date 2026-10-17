# 계정 인증과 OAuth

계정 자격 증명, 웹 세션, 목표 OAuth 발견·인가·갱신·폐기와 키 검증의 정본이다. 웹 인증과 목표 OAuth의 구현 상태를 구분한다.

[설계 전체](../SDD.md) · [요구사항과 설계 대응](traceability.md)

## 인가 서버

현재 `internal/authz`는 계정 자격 증명·웹 세션 JWT·서명 키와 폐기 목록을 소유한다. `internal/web`가 로그인·가입·로그아웃 화면과 쿠키를 처리하고 `cmd/server/web_adapter.go`가 두 경계를 연결한다. ES256 서명·검증은 `github.com/go-jose/go-jose/v4`에 맡기고 암호 알고리즘을 직접 구현하지 않는다.

표준 OAuth의 인가 코드·갱신·폐기 기능은 [OAuth discovery와 최초 인가](authentication.md#oauth-discovery와-최초-인가)·[갱신·폐기와 공유 저장](authentication.md#갱신폐기와-공유-저장)의 목표로 추가할 예정이며 현재 구현하지 않았다.

## 공개 로그인·가입 자원 보호

`FR-AGENT_CONTEXT-131`의 자격 증명 보호를 위해 `/login`·`/register`의 POST는 출처 검사를 통과한 뒤 양식 해석·계정 조회 전에 같은 시도 예산을 사용한다. GET·로그아웃과 인증된 관리 요청에는 이 예산을 적용하지 않는다. 다음 값은 시험 운영의 보수적인 고정 설계값이며 처리량 보장이 아니다. 배포 환경 변수나 해시 비용 변경으로 우회하는 수단은 열지 않는다.

| 경계 | 정책 |
|------|------|
| 출처별 시도 | 최초 허용 시도부터 60초 고정 창에 로그인·가입 합계 10회 |
| 인스턴스별 시도 | 같은 방식의 60초 창에 모든 출처 합계 60회 |
| 출처 키 | IPv4는 주소 하나, IPv6는 /64 대역. 소스 포트·아이디·쿠키로 분리하지 않는다 |
| 출처 상태 | 최대 4096개. 새 출처 처리 시 만료된 창을 지우며 활성 항목을 밀어내지 않는다. 가득 차면 새 출처를 거부한다 |
| 양식 본문 | 8 KiB 상한. 초과는 HTTP 413, 해석 실패는 기존 HTTP 400 |
| 비밀번호 작업 | 인가 서비스 하나에서 최대 4개, 대기열 없음. 계정 조회 전 자리를 확보하며 등록 해시·더미 대조·기존 대조·구형 재해시가 같은 자리를 쓴다 |
| 초과 응답 | HTTP 429, `Cache-Control: no-store`, 정수 초 `Retry-After`와 같은 한국어 안내. 시도 제한은 남은 창의 최댓값을 올림하며 작업 자리 부족은 1초 후 재시도를 안내한다 |

거부된 시도는 창을 연장하거나 성공 시 카운터를 초기화하지 않는다. 제한은 계정 존재를 조회하기 전에 판정하고 계정별 실패 상태·영구 잠금은 만들지 않는다. 같은 출처의 여러 정상 사용자와 IPv6 /64 사용자는 예산을 공유하므로 NAT·공유 프록시에서 일시적인 거부가 가능하다. 출처를 바꾸는 분산 공격은 인스턴스 전체 예산과 작업 상한으로만 제한하며 계정별 무차별 대입 방어가 완결되었다고 주장하지 않는다. 없는 계정 로그인은 계속 더미 bcrypt를 실행하고 일반 불일치 안내를 사용한다. 중복 가입도 먼저 같은 비용의 해시를 수행한 뒤 기존 중복 응답으로 처리하며 SRS가 허용한 중복 가입의 존재 노출 이상으로 구분을 늘리지 않는다. 비밀번호를 응답·로그·제한 상태에 보관하지 않는다.

주소 해석은 `cmd/server`의 기존 `transportSecurity`가 소유한다. 직접 TLS는 실제 `RemoteAddr`만 사용한다. 프록시 모드에서도 실제 접속 상대가 기존 `TRUSTED_PROXY_CIDRS`에 있고 기존 단일 HTTPS 전달 검사를 통과한 때만, 값이 하나인 유효한 `CF-Connecting-IP`를 사용한다. 누락·복수·목록·zone 포함·잘못된 주소는 실제 상대 주소로 합산하며 `X-Forwarded-For`·`Forwarded`·`X-Real-IP`는 읽지 않는다. IPv4-mapped 주소는 IPv4로 통일한다. 식별 실패는 공통 출처 키로 합산해 무제한 새 키를 만들지 않는다. 신뢰 대역을 넓히지 않고 헤더를 인증·권한 근거로 사용하지 않는다. 현재 Tunnel 경로의 호스트 프로세스는 이미 신뢰 경계 안이므로 헤더를 위조할 수 있으며, 자체 Worker의 헤더 변경·Pseudo IPv4·IP 헤더 제거 정책은 운영자가 별도로 점검한다.

자리가 없거나 요청이 이미 취소되었으면 DB·bcrypt에 들어가지 않는다. bcrypt 자체는 취소할 수 없으므로 시작한 작업은 반환할 때까지 자리를 유지하며 goroutine으로 분리해 취소를 가장하지 않는다. 대조·생성이 끝나면 취소를 다시 확인해 후속 계정 생성·해시 갱신·세션 발급을 진행하지 않고 모든 반환 경로에서 자리를 해제한다. 기동 시 더미 해시 준비는 수신 시작 전 한 번 수행한다.

시도 상태는 인스턴스 메모리의 만료 가능한 자원 보호 상태이며 계정·프로토콜 세션 상태가 아니다. 재시작하면 초기화되고 여러 인스턴스의 상한은 합산된다. Cloudflare WAF·Rate Limiting 등 외부 방어의 실제 적용 여부는 저장소 구성만으로 확인할 수 없으며 애플리케이션 보호의 전제조건으로 삼지 않는다. 대규모·다중 인스턴스 방어, 운영 부하·오탐률, 호스트 접근·Tunnel 경로·헤더 덮어쓰기와 외부 정책의 확인은 운영 개방 전 별도 항목이며 정책·접근 제어를 자동 변경하지 않는다. 고정 창 경계의 순간적인 두 창 합산 시도도 작업 상한은 넘지 못한다.

## OAuth discovery와 최초 인가

보호 리소스 메타데이터는 `resource`를 공개 HTTPS `/mcp` URI로, `authorization_servers`를 인가 서버 issuer로, scope를 `agent-context`로 제공한다. 인가 서버 메타데이터는 표준 endpoint와 `S256`, `token_endpoint_auth_methods_supported=["none"]`, `authorization_response_iss_parameter_supported=true`, `grant_types_supported=["authorization_code","refresh_token"]`을 광고한다.

초기 등록은 대상 호스트별 공개 `client_id`와 허용 callback을 직접 연결용 등록 구성으로 관리한다. 제품 이름이 아니라 실제 표시한 callback을 검증하며 동일 loopback IP의 포트만 예외로 둔다. 등록은 설정 데이터이며 호스트별 코드가 아니다. CIMD는 `TBD-AGENT_CONTEXT-060`을 유지하고, 외부 URL fetch·SSRF 방어 없는 자동 등록은 열지 않는다.

구현 예정인 인가 코드는 해시 저장·60초·1회 소비·PKCE·client·callback·resource 결합과 `iss` 응답을 사용한다. 토큰 교환에서 `token_type=Bearer` 접근 토큰과 새 갱신 계열을 발급한다. 웹 세션은 12시간 정책으로 검증하되 그 만료를 MCP 갱신 계열의 만료로 사용하지 않는다. 인가 코드 재사용 시 최초 교환으로 만든 갱신 계열을 폐기한다.

`/token`의 64 KiB 본문 상한을 갱신 grant에도 적용한다. 응답은 OAuth 토큰 JSON과 `Cache-Control: no-store`, `Pragma: no-cache`이며 도구 응답과 로그에는 토큰을 전달하지 않는다.

## 갱신·폐기와 공유 저장

접근 토큰은 기존 1시간 수명·ES256·JWKS 키 회전을 유지한다. `iss`, `sub`, `aud`, `exp`, `iat`, `jti`, `scope`, `client_id`와 갱신 계열 UUIDv7 `grant_id`를 검증한다. 그래프 등급은 매 요청 조회하고 웹 세션 audience와 분리한다. 토큰 검증 캐시를 사용하더라도 계열·개별 폐기를 공유 DB에서 요청마다 확인하여 다른 인스턴스의 회수를 즉시 적용한다.

갱신 토큰은 암호학적 난수 32바이트를 base64url로 인코딩한 불투명 값으로 발급하고 원문은 응답에서만 전달한다. SHA-256 해시로 조회하며 로그·운영 진단·DB에는 원문을 남기지 않는다.

- `oauth_grant`: `grant_id`, `account_id`, `client_id`, `resource`, `scope`, 최초 인가·최근 회전 시각, 절대·비활동 만료, `revoked_at`, 내부 폐기 사유.
- `oauth_refresh_token`: 유일 `token_hash`, `grant_id`, `issued_at`, `expires_at`, `consumed_at`, 후속 토큰 해시. 소비된 해시는 아래 인증 상태 보존 종료 시점까지 유지한다.
- 인가 코드 저장 (구현 예정): 최초 발급한 `grant_id`와 연결하여 코드 재사용 시 계열을 폐기한다.
- 인덱스: 토큰 해시의 유일 조회, 계정·클라이언트별 grant, 계열·만료별 refresh 정리. 상태는 DB 공유이며 인스턴스별 메모리를 정본으로 삼지 않는다.

갱신 요청은 `client_id`·`resource`·선택 scope를 계열과 대조하고 계열 행 잠금 뒤 토큰 소비·후속 발급·최근 활동을 한 트랜잭션으로 처리한다. scope 확대, 만료·폐기·결합 위반은 `400 invalid_grant` 또는 해당 표준 오류로 거부한다. 소비된 토큰 재사용이면 계열 폐기를 커밋한 후 오류를 반환한다. 오류 반환 때문에 폐기 트랜잭션을 롤백하지 않는다.

한 계열의 동시 갱신 중 하나만 소비에 성공하며 후속 재사용은 계열 전체를 폐기한다. 호스트는 같은 자격의 갱신을 직렬화해야 하므로 이 동작을 호스트별 인수에 포함한다. 응답 유실 뒤 이미 소비한 갱신 토큰을 다시 보낸 경우도 보안상 재인가가 필요할 수 있다. 무조건 재시도나 다중 토큰 무제한 유효 처리는 하지 않는다.

현재 웹 권한 변경은 그대로 유지한다. 목표 웹 연결 관리는 `/connections`의 GET 목록과 POST 폐기로 제공한다. 서명·만료·폐기를 검증한 웹 세션과 기존 웹 출처 검사를 적용하고, 계정에 결합된 grant_id만 조회·폐기한다. 계정 UUID나 client_id를 사용자 입력의 인증 근거로 사용하지 않는다. 다른 계정의 grant 조회·폐기는 허용하지 않으며 MCP 도구로 토큰을 폐기하지 않는다. 호스트의 로컬 연결 삭제만으로 서버 자격이 폐기된다고 가정하지 않는다. OAuth 표준 revocation endpoint의 추가는 필요 시 별도 계약으로 다룬다. 이 경로는 목표 추가이며 아래 현재 여섯 화면 계약을 구현 완료로 바꾸지 않는다.

Bearer 탈취는 만료·폐기 전까지 재사용 가능하다. 공개 HTTPS·대상 서버 제한·유한 수명·회전·폐기·토큰 비노출로 위험을 제한한다. 기존 터널의 로컬 HTTP 시험 예외와 신뢰 대역은 넓히지 않고 실제 배포는 별도 승인 범위다. 갱신의 절대·비활동 수명과 정리는 아래 확정 계약을 적용한다. `TBD-AGENT_CONTEXT-063`은 해소되었고 실제 인수는 대기다.

## 토큰 검증

현재 웹 세션 JWT는 ES256 서명·`iss`·기대 `aud`·`sub`·`iat`·`exp`·`jti`와 폐기 목록을 검증한다. 공개 키와 폐기 상태는 공유 DB에서 읽고 검증 캐시를 사용한다. 키 회전 시 새 `kid`를 재조회하고 과거 키는 유효 세션 검증에 보존한다. 저장소 장애는 `ErrUnavailable`로 구분한다.

목표 Bearer 토큰은 [갱신·폐기와 공유 저장](authentication.md#갱신폐기와-공유-저장)의 scope·인가 계열 검증을 추가한다. 현재 접근 토큰 발급·보호 요청 검증은 구현 대기다. JWT 헤더·저장 키의 알고리즘이 있어도 허용 목록은 ES256으로 고정하고 다른 알고리즘이나 비대칭 키의 대칭 사용을 허용하지 않는다.

## 세션 쿠키 검증

세션 쿠키는 JWT 서명·클레임·폐기 검증기를 사용한다. [`SRS.md`](../SRS.md)의 [SRS 웹 세션](../SRS.md#웹-세션)은 브라우저 쿠키 인증을 별도 채널로 확정했으므로 웹 쿠키와 Bearer의 전달·대상·폐기 규칙을 분리한다.

| 항목  | 접근 토큰              | 세션 쿠키              |
|-------|------------------------|------------------------|
| 전달  | `Authorization` 헤더   | 쿠키                   |
| `aud` | 리소스 서버            | 웹 화면                |
| 수명  | 1시간과 갱신           | 12시간과 갱신 없음     |
| 실패  | `401`과 인증 도전      | 로그인 화면으로 보낸다 |

JWT 서명·공통 클레임 검증을 공유하고 `aud`와 폐기 책임을 채널별로 나누는 것이 [SRS 채널 분리](../SRS.md#채널-분리)의 구현이다. 검증기가 받아들일 `aud`를 채널마다 고정하므로, 웹 세션을 MCP에 제시하면 `aud` 검사에서 걸리고 그 반대도 같다. MCP에서는 추가로 검증된 `client_id`·scope·`grant_id` 결합과 계열 폐기를 확인한다. 웹 세션은 이 계열과 독립적으로 유지한다.

로그아웃은 그 세션의 `jti`를 폐기 목록에 넣고 쿠키를 지운다. 목록은 접근 토큰이 쓰는 것과 같은 테이블이며, [SRS 토큰 형식과 검증](../SRS.md#토큰-형식과-검증)이 확정한 대로 해당 값의 `exp`가 지나면 정리된다. 세션 수명이 12시간이므로 항목이 남는 기간도 거기까지다.

## 계정 생성과 비밀번호 처리

`FR-AGENT_CONTEXT-130`~`133`의 계정은 UUIDv7 `account_id`, `login_id`, `password_hash`, `created_at`을 보존한다. 로그인 아이디는 3~32자의 영문 소문자·숫자·밑줄이고 유일 제약으로 동시 가입 중복도 거부한다. 비밀번호는 8~128자의 유니코드 문자열을 검증하고 원문을 저장하지 않는다. 현재 저장 형식 `bcrypt-sha256-b64-v2:`를 유지한다. 비밀번호 UTF-8 바이트의 SHA-256을 padding 없는 표준 base64로 인코딩한 값에 bcrypt를 적용하여 72바이트 상한과 원시 다이제스트의 NUL 차이를 피한다. 기존 직접 bcrypt·`bcrypt-sha256-v1:` 해시는 기존 방식으로 검증하고 성공 시 같은 비밀번호 작업 자리에서 새 형식으로 재해시한다. 비용은 `BCRYPT_COST`로 구성한다. 오류 안내와 더미 대조는 계정 존재를 추가로 구분하지 않는다.

가입은 출처 검사·공유 시도 예산·양식 검증·비밀번호 작업 자리 확보·해시 생성·계정 저장 순으로 실행한다. 로그인은 같은 보호 경계에서 자격 대조 후 서명된 웹 세션을 발급한다. 취소된 요청에는 계정 저장·세션 발급을 계속하지 않는다. 비밀번호 재설정·실패 잠금·MFA·외부 IdP와 탈퇴는 기존 미정 사항에 남긴다.

## 갱신 자격의 수명과 정리

`FR-AGENT_CONTEXT-167`과 [SRS 토큰 갱신](../SRS.md#토큰-갱신)이 확정한 계약이며 환경 변수로 수명을 바꾸지 않는다.

| 시점 | 계산·판정 |
|---|---|
| 최초 인가 | `authorized_at`과 `absolute_expires_at = authorized_at + 30일`을 기록한다 |
| 최초 발급 | `last_rotated_at = authorized_at`, `idle_expires_at = authorized_at + 7일` |
| 성공 회전 | 절대 만료를 유지하고 `last_rotated_at`을 현재 시각으로, 비활동 만료를 현재 시각 + 7일로 바꾼다 |
| 유효 만료 | `min(absolute_expires_at, idle_expires_at)`에 도달하면 거부한다 |
| 접근 만료 | 발급부터 1시간. 계열·개별 폐기라면 만료 전에도 거부한다 |
| 상태 보존 종료 | 최초 절대 만료 + 1시간과 최종 접근 토큰 만료 + 검증 시각 오차 중 늦은 시점 |
| 물리 정리 | 삭제 가능한 코드·소비 토큰·계열·폐기 기록을 매시간 정리한다 |

도구 호출·실패한 갱신·웹 로그인은 비활동 만료를 연장하지 않는다. 정리가 늦어져 행이 남더라도 요청 시 만료·폐기를 판정한다. 7일 진단 파일 보존으로 인증 상태 보존을 대체하지 않는다.

## OAuth 저장 모델과 원자적 전이

다음 테이블은 목표 추가이며 후속 번호 마이그레이션으로 만든다. 기존 삭제 마이그레이션의 번호나 체크섬을 바꾸지 않는다. 관계형 기본 배치와 적용 절차는 [데이터 설계](data.md#스키마-적용)를 따른다.

| 테이블 | 키와 필수 상태 | 제약·조회 |
|---|---|---|
| `oauth_authorization_code` | SHA-256 `code_hash`, 계정·client·redirect·resource·scope·S256 challenge, 발급·60초 만료·소비 시각, 교환한 `grant_id` | 코드 해시 유일. 소비 후에도 연결한 계열의 보존 종료까지 재사용 탐지 근거를 보존한다 |
| `oauth_grant` | UUIDv7 `grant_id`, 계정·client·resource·scope, 인가·최근 회전·절대·비활동 만료, 폐기 시각·원인, `retain_until` | 계정·client 목록과 보존 종료 조회 인덱스. 결합은 생성 후 불변 |
| `oauth_refresh_token` | SHA-256 `token_hash`, `grant_id`, 발급·만료·소비 시각, 후속 토큰 해시 | 토큰 해시 유일, 계열 참조·정리 인덱스. 원문과 후속 토큰 원문은 미저장 |

회전은 해시로 계열을 찾은 뒤 `oauth_grant` 행, 해당 토큰 행 순으로 잠근다. 잠금 뒤 현재 상태를 다시 읽고 결합·만료·소비를 검증한다. 새 접근·갱신 토큰의 생성, 이전 토큰 소비, 후속 해시 삽입, 최근 회전·비활동 만료·`retain_until` 갱신을 한 트랜잭션에 묶는다. 서명·저장 실패는 모두 롤백하여 이전 토큰을 소비된 상태로 남기지 않는다. 커밋 후 응답 유실은 소비 취소의 근거가 아니다.

소비 토큰 재사용은 계열 폐기를 커밋하고 `400 invalid_grant`를 반환한다. 코드 재사용도 연결한 계열을 폐기한다. 이미 폐기·만료한 계열과 잘못된 결합은 재인가 대상이며 내부 저장·서명 장애는 `500`으로 구분한다. 정리와 웹 명시적 폐기도 같은 계열 잠금 순서를 따른다. 단일 토큰 소비와 계열 폐기가 다른 인스턴스에서도 즉시 판정되어야 한다.

### OAuth 저장 열

다음은 위 저장 모델을 DDL로 옮길 열 이름·타입·NULL 계약이다. 표에서 선택으로 표시하지 않은 열은 `NOT NULL`이다. UUIDv7은 SQL `uuid`, 시각은 `timestamptz`로 저장한다. 사전 등록 client·redirect·resource 목록은 배포 구성이므로 별도 client 테이블을 만들지 않는다. 아래 결합 값은 인가 시점의 검증 결과를 보존하며 이후 요청에서 현재 등록과도 대조한다.

| 테이블 | 열 | SQL 타입·NULL | 의미 |
|---|---|---|---|
| `oauth_authorization_code` | `code_hash` | `bytea`, PK | 원문 코드의 SHA-256, 정확히 32바이트 CHECK |
| `oauth_authorization_code` | `account_id` | `uuid` | 인가한 계정 |
| `oauth_authorization_code` | `client_id`, `redirect_uri`, `resource`, `scope`, `code_challenge` | `text` | 검증한 client·실제 callback URI·대상 리소스·scope·S256 challenge. verifier 원문 미저장 |
| `oauth_authorization_code` | `issued_at`, `expires_at` | `timestamptz` | 발급 시각과 발급 +60초 만료 |
| `oauth_authorization_code` | `consumed_at` | `timestamptz`, 선택 | 성공 교환 시각 |
| `oauth_authorization_code` | `grant_id` | `uuid`, 선택·FK | 성공 교환으로 만든 계열. 소비 시각과 함께 채우며 둘 다 NULL이거나 둘 다 값이 있는 CHECK |
| `oauth_grant` | `grant_id` | `uuid`, PK | 최초 인가 계열 UUIDv7 |
| `oauth_grant` | `account_id` | `uuid` | 계열 소유 계정. 생성 뒤 불변 |
| `oauth_grant` | `client_id`, `resource`, `scope` | `text` | 생성 뒤 불변인 인가 결합 |
| `oauth_grant` | `authorized_at`, `last_rotated_at` | `timestamptz` | 최초 인가와 최근 성공 회전 시각. 최초에는 두 값이 같다 |
| `oauth_grant` | `absolute_expires_at`, `idle_expires_at`, `retain_until` | `timestamptz` | 절대·비활동 만료와 인증 상태 보존 종료. 계산식은 [갱신 수명](#갱신-자격의-수명과-정리) |
| `oauth_grant` | `revoked_at` | `timestamptz`, 선택 | 명시적 폐기·재사용 탐지에 따른 계열 폐기 |
| `oauth_grant` | `revocation_reason` | `text`, 선택 | 폐기 시각과 함께 채우는 내부 고정 사유 코드. 임의 error·자격 원문 미저장 |
| `oauth_refresh_token` | `token_hash` | `bytea`, PK | 원문 갱신 토큰 SHA-256, 정확히 32바이트 CHECK |
| `oauth_refresh_token` | `grant_id` | `uuid`, FK | 소속 계열 |
| `oauth_refresh_token` | `issued_at`, `expires_at` | `timestamptz` | 발급 시각과 해당 토큰의 유효 만료. 발급 시 계열의 두 만료 중 이른 시각을 기록 |
| `oauth_refresh_token` | `consumed_at` | `timestamptz`, 선택 | 성공 회전 시각. 이 값이 있으면 재사용으로 판정 |
| `oauth_refresh_token` | `successor_token_hash` | `bytea`, 선택 | 같은 회전에서 삽입한 후속 토큰의 32바이트 해시. 소비 시각과 함께 채우는 CHECK |

코드·갱신 토큰의 `grant_id`는 `oauth_grant(grant_id)`를 참조하는 외래 키로 두며 삭제 cascade를 사용하지 않는다. 후속 토큰 해시의 존재·동일 계열·선행 토큰의 단일 소비는 회전 트랜잭션에서 검사하고, 해시 체인에 자기 참조 FK를 두지 않는다. 보존 종료 후에는 연결된 코드·갱신 토큰을 먼저 정리한 뒤 계열을 지운다. 미소비 코드는 만료 뒤 독립 정리하며 소비 코드는 자체 60초 만료만으로 지우지 않는다.

`oauth_grant(account_id, client_id, authorized_at DESC, grant_id DESC)`는 본인 연결 목록, `oauth_grant(retain_until)`은 보존 정리, 코드·갱신 토큰의 `grant_id` 인덱스는 계열별 조회와 자식 정리에 쓴다. 미소비 코드 정리에는 `expires_at WHERE consumed_at IS NULL` 부분 인덱스를 둔다. 해시 PK는 동시 동일 자격 삽입을 막지만 회전·재사용 폐기의 원자성을 대신하지 않는다.

DB는 해시 길이·소비 쌍의 NULL 조합·FK와 키 유일성을 검사한다. 60초·1시간·7일·30일 계산, `retain_until` 하한, 현재 시각의 만료, S256 검증, 계정/client/resource/scope 결합과 폐기 상태는 인증 저장 연산이 같은 계열 잠금 아래 판정한다. `expires_at`이 지났다는 이유로 조회 단계에서 소비 기록을 숨기면 재사용 탐지를 잃으므로 유효성 판정과 물리 정리를 구분한다.

## 목표 OAuth HTTP 경로

| 경로 | 메서드 | 계약 |
|---|---|---|
| `/.well-known/oauth-protected-resource/mcp` | GET | 공개 HTTPS `/mcp` resource·issuer·단일 scope 메타데이터 |
| `/.well-known/oauth-authorization-server` | GET | 고정 issuer, authorize·token·JWKS, PKCE S256·grant·공개 client·iss 지원 |
| `/authorize` | GET, POST | 사전 등록·callback·resource 검증, 계정 로그인·명시적 동의, 단일 사용 코드 발급 |
| `/token` | POST | 코드 또는 갱신 grant. form 본문 64 KiB, 성공 응답 `no-store`·`no-cache` |
| `/jwks.json` | GET | 공개 키만 반환하며 개인 키 성분을 제외한다 |
| `/connections` | GET, POST | 본인 계열·활성 등록 목록과 명시적 폐기. 웹 세션·출처 검사 적용 |

인가 성공·오류 리디렉션은 검증한 callback에만 전송하고 정확한 issuer의 `iss`를 포함한다. 등록하지 않은 URI의 오류는 그 URI로 리디렉션하지 않는다. `/authorize`의 상태 변경과 로그인·동의 양식은 기존 웹 출처·자원 보호를 적용하고 임의 `Host`로 issuer·resource를 만들지 않는다. 비밀 값이 담긴 응답을 모델 입력·도구 결과·진단 파일에 남기지 않는다.
