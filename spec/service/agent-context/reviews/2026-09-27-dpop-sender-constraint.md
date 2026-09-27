# DPoP 발신자 제한 서버 구현 검토

## 범위

- 검토일: 2026-09-27
- 요구사항: `FR-AGENT_CONTEXT-156`~`159`, `NFR-AGENT_CONTEXT-013`의 서버 범위
- 대상: DPoP 토큰 발급, 보호 요청 검증, 재생 방어, 자동 갱신과 전송 challenge

이 문서는 서비스 전용 DPoP 인증 프로필의 서버 구현 검토다. MCP `2026-07-28` 핵심 Bearer 인증 적합성이나 일반 MCP 클라이언트와의 인증 상호 운용을 주장하지 않는다.

## 구현 대조

- `migrations/005_dpop_proof_replay.sql`은 `(jwk_thumbprint, proof_id_hash)` 기본 키와 `expires_at` 인덱스를 두며, 원문 `jti`를 저장하지 않는다.
- `internal/authz/dpop.go`는 ES256, `typ=dpop+jwt`, 공개 P-256 JWK, 서명, `htm`, 정규 `htu`, 서버 시각 ±60초 `iat`, 비어 있지 않은 `jti`를 확인한다. 보호 요청에서는 `ath`와 토큰 `cnf.jkt` 결합도 확인한다.
- `htu`는 RFC 3986의 구문·scheme 기반 정규화 뒤 대조한다. scheme·host 소문자, 기본 포트 제거, 빈 경로의 `/`, 비예약 문자의 퍼센트 인코딩 해제와 16진수 대문자화만 적용하며 `%2F` 같은 예약 문자 인코딩은 풀지 않는다.
- `internal/store/auth.go`의 `ReserveDPoPProof`는 `INSERT ... ON CONFLICT DO NOTHING`으로 단일 사용을 원자적으로 예약한다. 충돌은 `invalid_dpop_proof`, 저장소 장애는 `ErrUnavailable`으로 분리해 전송 경계에서 HTTP 500으로 답한다.
- `internal/authz/http.go`는 새 DPoP proof 없이는 토큰을 발급하지 않고 `token_type=DPoP`와 검증한 JWK thumbprint의 `cnf.jkt`를 발급한다.
- `internal/mcp/transport.go`는 `Authorization: DPoP`와 `DPoP` 헤더를 각각 하나만 받고 스킴 이름의 대소문자는 구분하지 않으며, 인증 정보 없음·`invalid_token`·`invalid_dpop_proof`를 분리한 `WWW-Authenticate: DPoP` 도전을 만든다. 모든 도전은 `algs="ES256"`과 보호 리소스 메타데이터 위치를 포함한다.
- 자동 갱신은 이미 결합을 확인한 토큰의 `cnf.jkt`를 그대로 승계한다. 서버는 `DPoP-Nonce`나 `use_dpop_nonce` 흐름을 만들지 않는다.
- `cmd/server/periodic.go`는 독립 자문 잠금으로 만료 proof 행을 1분마다 정리하는 아홉 번째 주기 작업을 등록한다. 정리는 인스턴스 사이 시계 차이로 기록이 먼저 사라지지 않도록 `expires_at`에서 여유 1분이 더 지난 행만 지운다.

## 검증

- `TEST_DATABASE_REQUIRED=1 go test ./...`: 통과 (Go 1.27.1 도구 체인, 로컬 Docker 개발 데이터베이스). DPoP proof 재생, 키·대상·메서드·토큰 결합, 허용 시간 창, 갱신 결합, challenge 분류와 로그 비노출을 포함한다.
- `go build ./...`, `go vet ./...`, `gofmt -l .`: 통과.
- `TestDPoPProofRejectsMalformedProof`는 `typ`, 개인 키 JWK, ES256 외 `alg`, `jti` 누락, 허용 창 밖 과거·미래 `iat`, query·fragment가 있는 `htu`를 거부하고 허용 창 안의 미래 `iat`를 받는지 확인한다. `TestNormalizedDPoPTarget`은 `htu` 정규화의 같음과 다름을, `TestDPoPAuthorizationSchemeIsCaseInsensitive`는 스킴 대소문자를 확인한다.
- `TestReserveDPoPProofIntegration`은 실제 PostgreSQL에서 동시 예약 하나만 성공하고, 정리가 여유 안의 만료 행은 남기고 여유까지 지난 행만 지우는지 확인한다.
- 빈 임시 데이터베이스에서 `go run ./cmd/migrate up`으로 001~005를 적용하고, 두 번째 실행이 아무것도 적용하지 않으며 `dpop_proof_replay`가 `public`에 만들어지는지 확인했다.

## 남은 범위

- 별도 MCP 클라이언트 구현이 없으므로 토큰 발급, 요청별 새 proof, 재시도, 갱신 결합과 Bearer 하향 거부의 종단 간 시험은 남아 있다.
