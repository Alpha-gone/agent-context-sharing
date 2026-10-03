# 클라이언트 5단계 구현 검토

- 검토일: 2026-10-03
- 범위: 호스트 stdio 중계·오류·응답 보존, 독립 진단과 운영 수명
- 기준: [SRS](../SRS.md), [SDD](../SDD.md), [개발 계획](../DEVELOPMENT_PLAN.md)
- 이 기록은 해당 시점의 구현 검증 결과이며 동작 계약의 정본은 아니다.

## 확인한 구현

- `serve`가 기동 검증 후 인증 조정자·원격 HTTP·`host.Tools`를 조립한다. 로컬 발견은 외부 인증 없이 SDK가 처리하며 도구 요청은 원격 계약 검증·공개 정책·입력 검증을 거친다.
- 세 공개 정책의 목록·호출 판정은 공통 정책을 사용한다. 여섯 도구의 호스트 스키마에서 `created_by_agent`를 제거하고 호출에는 구성 UUIDv7만 주입한다. 우회 입력과 정책 밖 호출은 원격 도구 호출 전에 거부한다.
- 호스트 전송은 결과·원격 오류의 JSON을 유지하고 호스트 ID를 사용한다. 줄 프레이밍의 공백과 MCP 외피의 클라이언트 `serverInfo`·`complete` 유형만 적용한다. 다른 metadata·큰 숫자·필드 순서·문자열 표현·커서·부분 상태를 재계산하지 않는다.
- 로컬 호출 오류는 일곱 고정 코드·안전한 문구·재시도 힌트로 `structuredContent.client_error`에 제공하고 목록 오류는 JSON-RPC `-32603`의 `data.client_error`에 제공한다. 도메인 오류 8종은 재포장하지 않으며 검증된 원격 JSON-RPC 오류는 code·message·data를 유지한다. 잘못된 구성은 기동 전에 거부하며 stdout을 읽거나 오염시키지 않는다.
- 준비·발견·목록·도구 전송과 호스트 응답 기록은 하나의 원격 호출 범위를 공유한다. 느린 출력에도 호출 slot을 유지하고 호스트 미기록 도구 요청은 136개로 제한한다. 요청별 취소·EOF·신호를 구분하고 EOF의 접수된 응답은 기록한다.
- `doctor`는 구성, DNS·TLS, 보호 리소스 metadata, 인가 서버 metadata, 브라우저·루프백, 인가, 발견, 도구 목록의 여덟 검사를 순서대로 실행한다. 실패 종속 단계는 건너뛰며 사람 읽기·JSON 출력과 성공 0·실패 1·사용법 오류 2·건너뜀 3을 구분한다.
- 브라우저 진단은 비밀 없는 임시 루프백 경로를 사용한다. 인증·원격 계약은 기존 검증 경계를 재사용하고 도메인 도구를 호출하지 않는다. 종료 후 listener·opener·인증 조정자·HTTP transport를 정리하고 상태를 영속화하지 않는다.
- 호스트·인가·원격 시도·재시도 로그는 내부 생성 상관 ID와 허용된 필드만 사용한다. 호스트 ID·도구 입력·원격 오류 원문·URL·인가 비밀은 로그에 넣지 않는다.

## 검증 결과

- Go `1.27.1`에서 클라이언트 전체 `go test -race ./... -count=1`, 주요 패키지 반복 race 시험과 느린 출력 상한 회귀 10회 반복을 통과했다.
- `TestStdioAllToolsPoliciesResultsAndInjection`에서 실제 stdio 왕복으로 세 정책과 도구 13종의 성공·도메인 오류 8종, 여섯 주입과 우회 거부를 확인했다. `TestStdioClientErrorsAndRemoteProtocolError`에서 13종에 일곱 로컬 오류·인증 주체 변경·비밀을 포함한 미확인 오류를 주입해 코드·문구·재시도 힌트와 비밀 비노출을 확인했다.
- `TestInvalidRemoteCatalogBecomesStdioClientProtocolWithoutPartialTools`에서 실제 HTTP 전송 어댑터·계약 검증·stdio 연결로 누락·추가·스키마 변경의 부분 공개 없는 실패를 확인했다. `TestRemoteProtocolErrorPreservedThroughActualStdio`에서 유효한 HTTP 500·JSON-RPC 오류의 호스트 ID·원문 오류 객체를 확인했다.
- `TestHostEnvelopeOnlyReplacesServerInfo`, `TestContextFlowAndOutputLimitPreserveFraming`에서 MCP 외피·확장 metadata·이스케이프 문자열·큰 정수·평면 구조·검색 순서·커서·부분 상태, 초과 결과의 단일 오류와 후속 프레이밍을 확인했다.
- 공식 Go SDK 클라이언트를 실제 stdio에 연결해 `2026-07-28` 발견·도구 13종 목록·호출·클라이언트 오류와 클라이언트 `serverInfo`를 해석하는지 확인했다. 초기 시험은 목록 성공만 확인해 아래의 오류 누락 결함을 놓쳤으며 추가 회귀에서 일곱 목록 오류 노출을 확인했다. 이 시험은 지원 호스트나 공식 conformance 검증과 구분한다.
- 기존 실제 로컬 TLS 시험에 stdio를 연결해 100개 동시 호출의 원래 호스트 ID·응답 상관과 8개 전송 상한을 확인했다. 기존 실제 인증 조정자의 PKCE·JWT·DPoP·갱신·재인가·계정 결합 회귀도 유지했다.
- `TestHostPreparationSharesRetryBudgetAndSafeLogs`에서 목록 준비와 도구 전송이 같은 최대 3회 재시도 예산·상관 ID를 공유하는지 확인했다. 느린 stdout 시험에서 실행 8개·대기 128개가 유지되고 취소 뒤 종료되는지 확인했다.
- `TestStdioCancellationAndEOFDrainToolResponse`, 기존 EOF·SIGINT·SIGTERM 시험과 쓰기 취소 분류 시험을 통과했다. 새 원격 클라이언트는 이전 쓰기 키를 재사용하지 않으며 닫힌 클라이언트는 호출을 시작하지 않는다.
- `doctor`의 여덟 단계별 실패·순서·종속 건너뜀·제한 시간·취소·출력 실패와 두 출력 형식의 허용 필드를 확인했다. 로컬 TLS·브라우저 실행 대역·실제 루프백·ES256 토큰 교환에서 발견·목록만 호출하고 인가 URL·토큰·code·state·PKCE·proof를 출력과 로그에 남기지 않았다.
- 같은 구성의 진단 재실행에서 새 프로세스 수명의 키·토큰·인가 상태를 확인하고 이전 callback이 닫히는지 확인했다. 잘못된 진단 경로·query, 수신기·실행기 실패와 시간 초과·취소의 cleanup도 확인했다.
- 양쪽 모듈 build·vet, 서버 `TestToolManifestMatchesClient`, Linux amd64 교차 빌드, 저장소 전체 gofmt·diff와 변경 문서 상대 링크를 통과했다. SRS 기능·비기능 요구사항 51건의 SDD 추적을 유지했고 한국어 표현·맞춤법을 검토했다.

## 해결된 결함

### DEF-20261003-005: 응답 직후 같은 호스트 ID 재사용의 경쟁

- 영향도: 중간. 위치: `host/stdio.go`의 dispatcher·writer·호출 정리.
- 근거: 최초 stdio 회귀에서 이미 응답을 받은 뒤 같은 ID를 재사용하면 중복 ID 오류로 거부됐다. writer 완료 확인과 호출 map 정리가 호스트의 다음 요청보다 늦을 수 있었다.
- 처리: 응답 기록 직전에 완료한 ID를 해제하고 호출별 객체를 비교해 이전 호출의 늦은 정리가 새 호출을 지우지 않도록 했다. 전체 도구·오류의 연속 ID 재사용과 반복 race 시험이 통과했다.

### DEF-20261003-006: 전달됐을 수 있는 쓰기의 내부 취소 분류

- 영향도: 중간. 위치: `remote/http.go`의 `completionError`.
- 근거: 기존 코드는 deadline·전송 실패만 불확실 쓰기로 분류하고 명시적 취소를 그대로 반환했다. 5단계 직렬화에서는 이미 전달한 쓰기도 읽기와 같은 `client_timeout`이 될 수 있었다.
- 처리: 전달됐을 수 있는 쓰기의 명시적 취소도 `ErrIndeterminate`로 반환한다. 전달 뒤 읽기는 취소, 쓰기는 불확실 결과가 되는 회귀 시험을 통과했다.

### DEF-20261003-007: SDK가 목록 실패를 빈 성공 목록으로 해석함

- 영향도: 높음. 위치: `host/stdio.go`의 dispatcher·오류 직렬화.
- 근거: 공식 Go SDK `ListTools`에 원격 `ErrAuthorization`을 주입한 회귀가 수정 전 `Tools:[]`, `err=nil`로 실패했다. 목록 결과에 호출 전용 `isError`를 넣어 SDK가 실패를 관찰할 수 없었다.
- 처리: 목록의 로컬 오류를 JSON-RPC `-32603`·고정 메시지·`data.client_error`로 분리했다. 호출 오류와 검증된 원격 오류의 계약은 유지했다. SDK에서 일곱 목록 오류의 코드·문구·재시도 힌트를 확인했고, 잘못된 metadata·cursor·접수 상한·호스트 ID를 포함한 출력 상한 및 실제 HTTP 계약 누락·추가·스키마 불일치도 부분 목록 없이 실패했다.

### DEF-20261003-008: DNS·TLS 진단이 HTTP 프록시를 우회함

- 영향도: 낮음. 위치: `doctor/doctor.go`의 `checkNetwork`.
- 근거: 직접 TLS dial은 실제 요청의 HTTP transport·`HTTPS_PROXY` 경로를 사용하지 않았다. 직접 연결을 금지한 프록시 전용 시험과 독립 프로세스 환경 시험으로 필요한 경로를 확인했다.
- 처리: 같은 HTTP transport를 복제해 자격 증명 없는 `HEAD`를 보내며 TLS 검증과 프록시 설정을 유지하고 리디렉션은 막았다. `401`·`405`를 TLS 실패로 오인하지 않고 프록시 경로의 잘못된 TLS 호스트는 거부한다. 실제 진단 전체 시험에서 임의 TLS 대역 대신 운영 기본 네트워크 검사를 사용했다.

### DEF-20261003-009: 명시적 취소 뒤 추가 응답을 기록함

- 영향도: 낮음. 위치: SDD 「호스트 stdio」와 `host/stdio.go`의 취소·writer 경계.
- 근거: 공식 MCP `2026-07-28` stdio는 취소된 요청에 추가 메시지를 금지한다(`MUST NOT`). 기존 구현과 SDD는 `notifications/cancelled` 뒤에도 취소 결과를 기록했다. 규정은 [외부 근거](../reference.md)에 기록했다.
- 처리: 명시적 취소를 EOF·내부 제한 시간과 분리하고 writer의 기록 직전에 미기록 응답을 생략한다. 뒤늦은 성공·느린 출력 대기 취소·자원 해제와 EOF 응답 기록 회귀, 호스트·진단 race 5회 반복을 통과했다. 내부 오류 분류는 유지하지만 명시적으로 취소된 요청에는 성공·오류 어느 응답도 보내지 않는다. 공식 conformance 실행 완료를 뜻하지 않는다.

## 미해결 결함과 후속 경계

검증 범위에서 확정된 미해결 결함은 없다. 아래 항목은 대역·로컬 TLS·stdio 검증이 대체하지 않는 후속 범위다.

- 실제 서비스·데이터베이스·외부 계정 로그인·수신 측 proof 재생 방어·Bearer 하향 거부는 미실행이다. 3단계의 수신 측 완료 기준은 미완료로 유지한다.
- 시스템 브라우저·WSL2·최소 지원 운영체제·Codex CLI `0.156.1`·공식 conformance·성능 판정·배포 무결성은 6단계다. 로컬 시험과 교차 빌드를 이 결과로 보고하지 않는다.
- 실제 서비스·데이터베이스 통합 및 서버 전체 회귀는 이번 범위에서 실행하지 않았다. 공유 manifest 시험은 서버의 전체 통합 검증을 대체하지 않는다.

## DOX와 설계 정합성

- 추가 검토 뒤 클라이언트 전체 `go test -race ./... -count=1`, 호스트·진단 race 5회 반복, 양쪽 모듈 build·vet와 공유 manifest 시험을 통과했다. gofmt·diff·문서 링크와 요구사항 51건의 추적을 확인했다. 목록 오류·프록시·취소 계약을 SDD와 호스트·진단 소유 DOX 및 클라이언트 부모에 반영했다. 서버 계획·SRS와 상위 DOX는 요구사항·구조·소유권이 바뀌지 않아 유지했다.

- SDD에 stdio 전송 어댑터·원문 보존·MCP 외피·출력 backpressure·호출 범위, 진단 외피·제한 시간·종료 코드와 취소 오류 분류를 정리했다. 요구사항·서버 도구 계약·환경 변수·의존성은 변경하지 않았다.
- `host/AGENTS.md`와 `doctor/AGENTS.md`에 새 지속 경계의 책임·검증 계약을 두고 `client/AGENTS.md`의 Child DOX Index를 갱신했다. 인증·원격 소유 문서에 재사용 진단·공통 호출 범위와 안전한 로그 계약을 반영했다.
- 루트·명세 부모의 소유권·구조·workflow는 유지되므로 변경하지 않았다. SRS의 요구사항과 기존 추적 ID도 유지했다. 기존 검토 기록은 당시 검증 범위로 보존하고 현행 구현 상태는 개발 계획에서 갱신했다.
