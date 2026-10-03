# 클라이언트 6단계 구현 검토

## 판정

- 검증일: 2026-10-03, Go `1.27.1`, macOS `26.6.2` arm64.
- 작업 브랜치: `feature/client-phase-6-verification`, 기준 commit `5effcce56fc17395b3ee30591e60ed36ed40c160`. 아래 후보는 작업 트리 변경을 포함하며 공식 배포본이 아니다.
- 실제 서버 연동·동시성·성능·배포 후보 생성 검증을 수행했다. **6단계는 진행 중이며 출시 판정은 보류한다.**
- [SRS](../SRS.md)의 기능 35건·비기능 16건을 아래 결과에 대응시켰다. 패키지 시험 통과는 운영체제·호스트·공식 적합성의 출시 완료를 뜻하지 않는다.

## 실제 서버와 클라이언트

`internal/mcp/client_live_integration_test.go`의 `TestClientServiceLive`가 실제 PostgreSQL·AGE 저장소, `authz` 두 인스턴스와 MCP 처리기를 로컬 TLS에 연결하고 클라이언트 모듈의 `TestClientLiveService`를 별도 프로세스로 실행한다. 모듈 사이에는 HTTP와 시험 자료만 전달하며 서버 DB 접속 정보는 자식 환경에서 제거한다.

- 실제 PKCE 교환·루프백 callback·JWT/JWKS·DPoP·발견·13종 도구·호스트 stdio를 연결했다. 원격 `content`, `structuredContent`, `isError`와 호스트 요청 ID를 대조했다.
- 여섯 도구의 `created_by_agent` 주입, 쓰기 8종의 서로 다른 UUIDv7 키, `graph_create` 처리 후 응답 유실을 확인했다. 같은 논리적 재시도는 같은 키·새 원격 ID·새 proof를 사용하며 최초 결과를 재생한다.
- 다른 키의 올바르게 서명된 proof, 이미 사용한 proof와 Bearer 하향 요청이 거부됐다. 같은 DB를 사용하는 두 인가 인스턴스에 동일 proof 100개를 보내 1개 성공·99개 거부를 확인했다.
- `issued_token`, `renewal_window`를 각각 실행했다. 갱신 시험은 교환된 토큰의 계정·audience·키 결합을 유지하며 실제 DB 키로 만료 9초 전 fixture를 서명한다. 한 시간 경과를 기다린 시험은 아니다. 이후 갱신 판정·발급·헤더·클라이언트 검증·저장·다음 요청 적용은 실제 구현이다.
- 동일 갱신 구간에서 준비한 100개 요청을 두 인가 인스턴스에 보내 모든 새 토큰의 클라이언트 키 결합 검증을 통과했다.
- 실제 서비스 `doctor`가 통과하고 도구를 호출하지 않으며 비밀이나 인가 URL을 출력하지 않았다.
- 주요 종단 간 race 시험을 `-count=3`으로 반복했고, 최종 `test-live.sh`도 건너뜀 없이 통과했다.
- 접속 정보 없는 실행기의 종료 코드 2와 실제 서버 fixture 없는 클라이언트 시험의 실패를 확인해 잘못된 통과를 차단했다.

세션 판정과 브라우저 opener는 시험 대역이다. 사용자 로그인 화면·2FA·시스템 브라우저는 검증하지 않았다. 검색 embedding 실패는 의도적으로 주입한 fallback이며 실제 LLM 품질·외부 모델 서비스 검증도 아니다. 서버의 기존 연산·멱등성·DPoP 오류 주입 시험은 별도로 실행했다. 상세 서버 추적은 [상호 운용 검토](../../agent-context/reviews/2026-10-03-client-interop.md)에 있다.

## 발견한 결함

### DEF-20261003-010: 최초 JWKS가 비어 클라이언트 인가를 중단함

- 중요도: 중간. 상태: 해결.
- 빈 DB에서 클라이언트는 토큰 발급 전에 JWKS를 읽지만 서버의 서명 키 초기화는 첫 토큰 발급에만 있었다.
- 서버 SDD를 먼저 정합화하고 `JWKSHandler`에서 기존 `activeKey` 원자적 초기화를 재사용했다. 최초 조회의 공개 키 생성·두 번째 조회의 같은 kid·개인 키 비공개를 `TestJWKSBeforeFirstTokenInitializesPublicKeyWithoutRotation`과 실제 빈 DB 연동으로 확인했다.

### DEF-20261003-011: 무도전 진단이 루트 보호 리소스 문서를 발견하지 못함

- 중요도: 중간. 상태: 해결.
- 실제 서버는 루트 well-known 문서를 제공하지만 `doctor`의 도전 없는 발견은 `/mcp` 전용 문서만 조회했다.
- 클라이언트 SDD에 대체 조회 순서를 정하고 암시적 경로 조회의 HTTP 404만 루트 조회로 전환했다. 명시한 `resource_metadata`, HTTP 500과 잘못된 문서는 우회하지 않는다. `TestProtectedResourceFallsBackOnlyOnImplicitPathNotFound`와 실제 서비스 `doctor`로 확인했다.

## 동시성·성능·자원

- 전체 클라이언트 `go test -race -count=1 ./...`와 실제 DB를 강제한 전체 서버 race가 통과했다. 양쪽 모듈 build·vet, `client_live` 태그의 vet와 저장소 `gofmt -l .`도 통과했다.
- 전체 서버 JSON 결과에서 test 함수의 skip은 없었다. `cmd/audit`, `cmd/migrate`, `migrations`의 패키지 skip은 `[no test files]`로, DB 통합 시험의 건너뜀이 아니다.
- 100개 호출의 ID 상관, 8개 진행 상한·128개 대기열·취소·출력 backpressure·각 크기 경계·재시도 오류 주입은 기존 패키지 시험과 함께 실행했다.
- `TestRelayPerformance1KiB100Concurrent`는 준비된 실제 클라이언트 인증 조정자·HTTP·호스트 stdio와 모의 로컬 TLS 서버를 사용한다. 요청·응답은 줄바꿈 제외 각각 정확히 1,024바이트이며 100개를 동시에 시작한다.
- 측정은 호스트 입력 대기열·proof 생성·직렬화·출력 및 로컬 TLS 왕복까지 포함하므로 자체 처리 시간의 보수적 상한이다. 일반 빌드의 `CLIENT_PERFORMANCE_REQUIRED=1`, `-count=5`에서 p95는 각각 **29.143292, 21.516792, 23.768375, 24.994417, 23.042041ms**로 50ms 이하였다. race 계측은 성능 판정에 쓰지 않았다.

## 배포 후보

`client/cmd/release`가 새 디렉터리에 `darwin-amd64`, `darwin-arm64`, `linux-amd64`, `linux-arm64` 후보를 생성한다. 각 실행 파일의 Go build information으로 target·Go 판·CGO 비활성·trimpath·revision을 검증하고 실행 파일·SPDX·로컬 빌드 출처 기록 총 12개 파일을 `SHA256SUMS`에 넣는다.

- 네 후보를 실제 빌드하고 SPDX 2.3 공식 JSON schema로 네 SBOM을 검증했다. 연결된 모듈과 Go 런타임을 포함하고 미확인 라이선스는 `NOASSERTION`으로 표시한다.
- 로컬 출처 JSON은 빌드 설정·revision·dirty 상태·digest를 기록한다. SLSA attestation이나 독립적으로 검증한 공급망 출처 증명을 주장하지 않는다.
- 임시 Ed25519 시험 키로 manifest를 서명하고 별도 경로의 시험 공개 키로 검증했다. 서명 부재·다른 키·변조·누락·경로 우회 거부를 확인했다. 동봉 공개 키를 자동 신뢰하지 않는다. 공식 서명자·배포 신뢰 정책은 미정이다.
- macOS arm64 네이티브와 amd64 번역 실행의 시작·로컬 발견, 네트워크 차단·읽기 전용 컨테이너의 Linux arm64·amd64 시작·발견을 확인했다. 이 컨테이너는 지원 Ubuntu/Debian 최소 판 행렬이 아니다.
- 임시 후보와 시험 서명 자료는 `/private/tmp/agent-context-client-phase6.NhCWnh/`에 있다. 최신 후보는 `final-fixture/`이며 공개 업로드하지 않았다.

재현 명령은 `client/`에서 실행한다. `-key`는 별도로 관리한 서명 키이며 시험 키를 공식 키로 사용하지 않는다.

```sh
go run ./cmd/release build -dir <새-출력-디렉터리> -version <후보-판> -go <Go-1.27.1-경로> -key <Ed25519-PKCS8-PEM>
go run ./cmd/release verify -dir <후보-디렉터리> -public-key <별도로-신뢰한-공개-키>
CLIENT_PERFORMANCE_REQUIRED=1 go test -count=5 -run '^TestRelayPerformance1KiB100Concurrent$' ./internal/client/remote
```

실제 연동 실행은 저장소 루트에서 수행한다. 접속 정보는 시험 DB에 한정하며 원문을 기록하지 않는다.

```sh
TEST_DATABASE_URL=<전용-시험-DB> AGE_GRAPH_NAME=agent_context GO_BIN=<Go-1.27.1-경로> sh client/internal/client/test-live.sh
```

## 요구사항 51건 대조

아래 `FR-NNN`, `NFR-NNN`은 각각 `FR-AGENT_CONTEXT_CLIENT-NNN`, `NFR-AGENT_CONTEXT_CLIENT-NNN`을 뜻한다. `패키지`는 해당 패키지의 전체 race 시험, `실서비스`는 위의 제한이 명시된 실제 연동 시험이다. 미검증 내용은 별도 열에 남긴다.

| ID | 실행 근거 | 남은 검증 또는 범위 |
|----|-----------|--------------------|
| FR-001 | host 패키지: stdio·EOF·프레이밍, 실서비스 | 지원 Codex 호스트 |
| FR-002 | host/remote: 발견·구형 handshake 거부·메타데이터, 실서비스 | 공식 conformance |
| FR-003 | 공유 manifest·host 13종·실서비스 | 없음 |
| FR-004 | contract/remote: 전체 목록 검증·변경 거부, SDK 오류 노출 | 없음 |
| FR-005 | contract/host: 입력 제약·전송 전 거부 | 없음 |
| FR-006 | host 13종 계약·실서비스 여섯 주입 | 없음 |
| FR-007 | authorize: 메타데이터·전환 거부·루트 fallback, 실서비스 | 없음 |
| FR-008 | authorize PKCE·실서비스 실제 교환 | 시스템 브라우저·로그인 화면 |
| FR-009 | authorize: 주소·포트·경로·state·중복·IPv6 fallback | 운영체제별 실제 callback |
| FR-010 | authorize: 일회 교환·issuer 검증, 실서비스 | 없음 |
| FR-011 | authorize/observability: 메모리·비노출·새 키 | OS 행렬의 종료 후 확인 |
| FR-012 | authorize: 원자 갱신·비역행·결합 거부, 실서비스 100개 갱신 | 없음 |
| FR-013 | authorize/remote: 공유 인가·동시 요청 race | 실제 브라우저 행렬 |
| FR-014 | authorize/remote: 5초 여유·401 분류·한 번 재인가 | 없음 |
| FR-015 | remote 전송 헤더·proof, 실서비스 | 없음 |
| FR-016 | host/remote: 100개 ID 상관, 성능 시험 | 없음 |
| FR-017 | contract/host 원문 결과·도메인 오류, 실서비스 13종 | 없음 |
| FR-018 | host/remote: 취소·별도 인증 시간·잔여 요청 시간 | 지원 Codex 취소 |
| FR-019 | remote 전달 시점·협상/비협상·예산, 실서비스 응답 유실 | 없음 |
| FR-020 | remote 읽기 재시도·jitter·완전 응답 미재시도 | 없음 |
| FR-021 | contract/host/remote: 결과 보존·도메인 오류 미재시도 | 없음 |
| FR-022 | contract/host: 커서·부분 상태·context_flow_get 보존 | 없음 |
| FR-023 | config: 필수 값·HTTPS·경로·UUIDv7 거부 | 없음 |
| FR-024 | authorize/remote: 계정 결합·계정 전환 거부 | 없음 |
| FR-025 | authorize 실행기 실패·URL 비노출·취소 정리 | 실제 시스템 실행기 |
| FR-026 | host/app/authorize: EOF·종료·listener 정리 | OS 행렬·실제 강제 종료 |
| FR-027 | host/remote lifecycle·확장 협상, 실서비스 | 공식 conformance·지원 Codex |
| FR-028 | remote: TTL·scope·fingerprint·원자 재검증 | 없음 |
| FR-029 | authorize: 대기자별 취소·마지막 대기자 정리 | 없음 |
| FR-030 | config/contract/host: list·call 정책 일치 | 없음 |
| FR-031 | doctor: 프록시·TLS·순서·오류·JSON·실서비스 | 실제 OS 브라우저·loopback |
| FR-032 | remote 8종 키·폐기·비협상, 실서비스 키 유지·재생 | 없음 |
| FR-033 | authorize: 프로세스 키·thumbprint·proof | 없음 |
| FR-034 | authorize metadata·토큰 결합·하향 거부, 실서비스 | 없음 |
| FR-035 | remote 시도별 새 proof·nonce 거부, 실서비스 재시도 | 없음 |
| NFR-001 | authorize/observability/doctor 비밀 비노출 | OS 종료·파일 시스템 행렬 |
| NFR-002 | authorize callback·PKCE 오류 주입, 실서비스 | 없음 |
| NFR-003 | config/authorize/doctor TLS·호스트 검증 | 없음 |
| NFR-004 | observability/doctor/host 비밀·본문 비노출 | 없음 |
| NFR-005 | SDK·host/remote 계약과 DPoP 별도 실행 | 공식 stdio conformance |
| NFR-006 | 전체 race·100개 호출·갱신·인가 패키지 | 실제 브라우저 행렬 |
| NFR-007 | 1 KiB·100개 중계 p95 일반 빌드 5회 | 없음 |
| NFR-008 | host/remote/authorize 요청별 오류·취소 격리 | 없음 |
| NFR-009 | 네 target 교차 빌드·현재 OS/컨테이너 시작 | 지원 최소 OS·WSL2 전체 흐름 |
| NFR-010 | observability/app/doctor 안전한 구조화 로그 | 없음 |
| NFR-011 | app/authorize/remote 종료·새 프로세스 상태 | 실제 강제 종료·재실행 행렬 |
| NFR-012 | 모의 계약과 실서비스 별도 실행·skip 차단 | 없음 |
| NFR-013 | host/remote 바이트 경계·8/128 상한·복구 | 없음 |
| NFR-014 | authorize/remote redirect·TLS·자격 증명 비전달 | 없음 |
| NFR-015 | release 네 후보·SBOM schema·checksum·시험 서명 | 공식 서명자·출처 신뢰·공식 배포 |
| NFR-016 | 실서비스 다른 키·Bearer·재생 100개 거부 | 없음 |

SDD 추적표는 동일한 51건을 유지한다. 위의 `없음`은 해당 자동화 범위에 추가 실패가 없다는 뜻이지 전체 출시 승인이 아니다.

## 남은 출시 검증

- 공식 conformance의 확인한 서버 CLI는 HTTP `--url` 진입점이다. 직접 stdio 실행 경로를 확보하고 고정 revision의 공식 시나리오를 실행해야 한다. SDK 회귀 시험을 이를 대신한 것으로 표시하지 않았다.
- 설치된 Codex는 `0.159.3`이며 지원 기준 `0.156.1`에서 발견·목록·읽기·쓰기·취소는 아직 실행하지 않았다.
- 지원 최소 macOS·Ubuntu/Debian·Windows 11/WSL2에서 실제 브라우저 인가·callback·읽기·쓰기·종료를 확인해야 한다.
- 공식 서명자와 출처 신뢰를 정하고 깨끗한 출시 revision에서 산출물을 재생성해야 한다. 임시 시험 키와 dirty 후보는 출시 근거가 아니다.

## DOX 및 시험 데이터

`client/AGENTS.md`, `authorize/AGENTS.md`, `internal/mcp/AGENTS.md`의 시험·발견 계약과 새 `client/cmd/release/AGENTS.md` 및 부모 index를 갱신했다. 루트·spec 부모와 서비스 소유 AGENTS는 영역 소유권·요구사항·지원 범위가 바뀌지 않아 유지했다. SRS 요구사항과 운영 구성은 바꾸지 않았다. 문서 링크·추적 ID·한국어 표현·diff를 확인했다.

기존 개발 DB나 볼륨을 초기화하지 않았다. 전체 서버용 `agent_context_client_phase6_20261003`, 실제 클라이언트 fixture용 `agent_context_client_live_20261003` 두 시험 DB를 생성해 분리했고 검증 후 유지했다. 후속 실행에서도 서버 저장소의 비정상 키 fixture와 실제 클라이언트 DB를 섞지 않는다.
