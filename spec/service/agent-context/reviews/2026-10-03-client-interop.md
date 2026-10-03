# 서버 18·19단계 클라이언트 상호 운용 검토

## 판정과 범위

2026-10-03, Go `1.27.1`에서 서버의 실제 PostgreSQL·AGE·인가·MCP 구현과 별도 클라이언트 모듈의 HTTP·stdio를 연결했다. 18단계의 클라이언트 멱등성 재시도와 19단계의 발급·보호 호출·갱신·재생 연결 항목을 완료했다. 클라이언트 6단계 전체와 공식 배포 판정은 완료하지 않았다.

`internal/mcp/client_live_integration_test.go`의 `TestClientServiceLive`가 같은 DB를 사용하는 두 `authz.Service`를 만들고 클라이언트 `TestClientLiveService`를 자식 프로세스로 실행한다. 실제 TLS·PKCE·JWT·DPoP·멱등성 예약·도메인 처리를 사용하며 세션 판정·브라우저 실행·검색 embedding은 대체한다. 운영 로그인 화면·실제 브라우저나 LLM 품질을 검증한 결과가 아니다.

## 요구사항 추적

| 요구사항 | 구현·실행 근거 | 판정 |
|----------|----------------|------|
| FR-AGENT_CONTEXT-155 | MCP 멱등성 처리·실제 13종 연산 계약·저장소 멱등성 시험. 클라이언트 8종 키와 처리 후 `graph_create` 응답 유실 뒤 같은 키·최초 결과 재생 확인 | 18단계 연결 완료 |
| FR-AGENT_CONTEXT-156 | authz 실제 PKCE·proof 교환·DPoP 토큰 및 metadata. 클라이언트가 JWT 서명·`cnf.jkt`를 검증 | 발급 연결 완료 |
| FR-AGENT_CONTEXT-157 | authz proof 검증·MCP 도전, 실제 보호 호출·다른 키·Bearer 거부. JOSE·ath·대상·메서드 오류는 기존 authz 계약 시험 | 보호 연결 완료 |
| FR-AGENT_CONTEXT-158 | 실제 DB의 proof 단일 사용 예약. 순차·교차 인스턴스 거부, 두 인가 인스턴스에 동일 proof 100개 중 1개 성공·99개 거부. 시간 창·정리는 기존 authz/store 시험 | 재생 연결 완료 |
| FR-AGENT_CONTEXT-159 | 실제 DB 키로 갱신 구간 fixture를 서명한 뒤 실제 VerifyAndRenew·헤더·클라이언트 저장·다음 호출 검증. 동시 갱신 100개 모두 원래 키 결합 유지 | 갱신 연결 완료 |
| NFR-AGENT_CONTEXT-013 | 실제 서비스의 다른 키·proof 재생·Bearer 거부와 클라이언트 메모리 수명·안전한 로그 회귀 | 서버 DPoP·클라이언트 계약 확인 |

갱신 fixture는 실제 교환된 토큰의 계정·audience·키 결합을 유지하고 만료를 9초 앞으로 조정해 서명한다. 한 시간 대기를 거치지 않으며 운영 토큰 발급 동작을 변경하지 않는다. 이후 갱신 경로는 실제 구현이다.

재생 저장 장애의 HTTP 500 분류는 기존 `TestDPoPProofReservationFailureIsUnavailable`의 저장소 대역으로 확인했다. 이것을 실제 DB 장애 주입으로 표시하지 않는다. 데이터베이스 통합 시험에서는 원자적 예약·동시 재생·만료 정리를 별도로 확인했다.

## 검증 결과

- `TestClientServiceLive`의 발급·갱신 두 경로를 race `-count=3`으로 반복 통과했다.
- 최종 `client/internal/client/test-live.sh`가 실제 연결과 서버 MCP·authz·server 회귀를 건너뜀 없이 통과했다.
- DB 접속과 `TEST_DATABASE_REQUIRED=1`을 설정한 서버 전체 `go test -race -count=1 ./...`가 통과했다. JSON 기록에 test 함수의 skip은 없었다. 시험 파일 없는 도구 패키지는 DB 시험 건너뜀과 구분했다.
- 양쪽 모듈 build·vet, tagged fixture vet와 전체 클라이언트 race를 통과했고 `gofmt -l .`은 비어 있었다.
- 첫 JWKS에서 기존 active-key 초기화를 수행하도록 서버 SDD와 구현을 정합화했다. 최초 공개 키·반복 조회의 같은 kid·개인 키 비공개 회귀와 빈 DB 실제 연결을 확인했다.

## 적합성 경계와 기록

위 DPoP는 MCP `2026-07-28`의 핵심 Bearer 인증을 대체한 **서비스 전용 프로필**의 시험이다. MCP 핵심 인증 conformance로 보고하지 않는다. 호스트 stdio 공식 conformance·지원 Codex 판·최소 운영체제·WSL2·실제 브라우저·공식 서명자는 미검증이며 [클라이언트 6단계 검토](../../agent-context-client/reviews/2026-10-03-client-phase-6.md)에 구분했다.

기존 데이터·볼륨을 초기화하지 않았고 별도 시험 DB 두 개를 유지했다. 전체 저장소의 비정상 키 fixture가 실제 클라이언트의 JWKS를 오염시키지 않도록 DB를 분리했다. 서버 SRS와 MCP 도구 계약은 유지했고 SDD의 최초 JWKS 초기화와 MCP 로컬 DOX 시험 계약만 갱신했다. 루트·spec 소유권과 child index는 바뀌지 않아 유지했다.
