# 에이전트 컨텍스트 MCP 클라이언트 모듈

## 목적

- `client/`는 에이전트 호스트와 원격 MCP 서버 사이에서 실행할 클라이언트의 독립 Go 모듈을 보관한다.

## 소유권

- 루트 `AGENTS.md`가 구현 공통 계약을 소유한다.
- 클라이언트의 요구사항과 패키지별 책임은 `spec/service/agent-context-client/SRS.md`와 `SDD.md`가 소유한다.
- 이 문서는 클라이언트 구현 영역의 로컬 계약을 소유한다.

## 로컬 계약

- `client/go.mod`의 모듈 경로는 `agent_context_sharing/client`이며 서버 루트 모듈의 패키지를 import하거나 `replace`로 참조하지 않는다.
- `internal/client/contract/tool_manifest.json`은 서버 `internal/mcp`의 공개 도구 13종과 입력 스키마를 고정한 공유 검증 자료다. 클라이언트 계약 검사는 이를 사용하고 서버의 스냅샷 테스트가 실제 도구 정의와 대조한다.
- 공식 MCP Go SDK 판은 `client/go.mod`에 고정한다. `contract`의 SDK 확인 테스트는 호스트 `2026-07-28` lifecycle·`stdio` 타입의 존재를 검증한다.
- `contract`는 전체 13종 계약을 검증한 뒤 공개 정책과 호스트 입력 스키마를 적용한다. `host.Tools`는 정책 밖 호출과 `created_by_agent` 우회 입력을 거부하며, 여섯 변경 도구에는 구성의 UUIDv7만 주입한다.
- 도구 이름·읽기/쓰기·행위 에이전트 주입 여부는 `contract`의 단일 분류표가 소유한다. `config.Load`는 `contract.NewPolicy`를 재사용하고 검증한 공통 정책을 제공하며 별도 도구 목록을 두지 않는다. 정책 오류는 `ErrConfiguration`, 호스트 입력·원격 계약 위반은 `ErrProtocol`로 구분하고 호스트 오류 분류는 SDD를 따른다.
- `remote`의 발견·목록 캐시는 프로세스 메모리에만 두고 `ttlMs`·`cacheScope`와 최초 도구 지문을 지킨다. 인증·HTTP 구현은 `Source` 경계가 담당하며, 캐시 주체 식별자에는 토큰 원문을 사용하지 않는다.
- 발견·목록 조회 중 주체 변경은 `ErrIdentityChanged`로 중단하고 응답을 캐시·공개·지문 변경 판정에 사용하지 않는다. 취소·제한 시간이 우선하며 이 오류 자체로 자동 재인가·재전송을 시작하지 않는다. 첫 출시의 목록 `nextCursor`는 생략·빈 문자열만 허용한다.
- `authorize`가 프로세스 키·검증 토큰·공유 브라우저 인가를 소유한다. `serve`는 기동 때 조정자를 만들고 종료 때 닫는다. 보호 요청·주체 연결은 원격 전송이 담당하며 호스트 오류 직렬화와 구분한다.
- `remote.NewHTTP`가 인증·HTTP 전송·캐시를 조립하고 `serve`가 `host.Tools`와 연결한다. 인가와 비대화형 요청 적용을 분리하고 동일 호출의 호스트 context·원격 처리 잔여 시간·재시도·멱등성 키·자원 상한을 지킨다. 인가 대기는 인증 제한 시간으로, 그 밖의 원격 처리 누적 시간은 요청 제한 시간으로 제한하며 호스트 deadline·취소가 항상 우선한다.
- `serve`의 stdout은 완전한 MCP 메시지 전용이며, 구성 오류·로그·진단은 이를 오염시키지 않는다. 호스트 `stdio`는 SDD의 입력·출력 상한을 지키고 초과 입력 뒤 프레이밍을 복구하며, 입력 EOF 때 원격 호출을 취소하고 접수한 요청의 응답을 기록한 뒤 종료한다. 명시적 취소 통지의 미기록 응답은 생략한다. 결과·오류는 원문 표현을 보존하고 로컬 오류만 일곱 코드로 분류하되 목록 오류는 JSON-RPC 오류로 노출한다.
- `doctor`는 도구 호출 없이 구성·연결·발견·브라우저·인가·원격 계약을 순차 검사하고 안전한 사람 읽기·JSON 외피를 출력한다. 실패에 종속된 검사는 건너뛰며 생성한 런타임 상태를 종료 때 정리한다.
- 실제 서버 시험은 `client_live` 빌드 태그의 `TestClientLive*`로 분리하며, 접속 정보나 시험 함수가 없거나 시험을 건너뛰면 통과로 판정하지 않는다.

## 작업 지침

- 패키지 경계와 의존 방향은 클라이언트 SDD의 「패키지 경계」를 따른다.
- 서버의 공개 도구 계약을 의도적으로 바꿀 때만 루트 모듈 `internal/mcp`의 스냅샷 시험 갱신 옵션으로 manifest를 재생성한다.

## 검증

- `client/`에서 클라이언트 빌드·vet·계약 시험을, 저장소 루트에서 서버 `TestToolManifestMatchesClient`를 각각 실행한다. `stdio` 경계를 바꾸면 구성 사전 검증, 동시 ID, 입력·출력 상한, EOF·취소·종료 신호 시험도 실행한다.
- 도구 계약·공개 정책·캐시를 바꾸면 도구 누락·추가·스키마 변경, 여섯 주입 도구의 우회 거부, 구성·계약 정책의 공통 검증과 오류 분류, 정책의 목록·호출 일치, TTL·인증 주체·지문 변경, 조회 중 주체 변경의 응답 폐기·재조회와 동시 조회·대기 취소 시험을 실행한다.
- 실제 서버 경로는 저장소 루트에서 `sh client/internal/client/test-live.sh`로 실행하고 기본 모의 시험 결과와 구분한다.

## Child DOX Index

- `internal/client/authorize/AGENTS.md`: 브라우저 인가, 메모리 자격 증명과 DPoP 키의 로컬 작업 계약을 정의한다.
- `internal/client/host/AGENTS.md`: 호스트 stdio 중계·정책·오류·응답 보존·취소의 로컬 작업 계약을 정의한다.
- `internal/client/doctor/AGENTS.md`: 독립 순차 진단·출력·수명과 검증의 로컬 작업 계약을 정의한다.
- `internal/client/remote/AGENTS.md`: 원격 HTTP 시도, 멱등성·재인가·자원 상한과 계약 캐시의 로컬 작업 계약을 정의한다.
