# 에이전트 컨텍스트 공유 시스템

AI 에이전트가 작업 중 얻은 지식과 맥락을 저장하고, 관계를 연결하여 이후 작업에서 다시 활용하는 AI 에이전트 제텔카스텐입니다. 여러 작업자가 컨텍스트 그래프를 공유하며 계정별 권한 범위 안에서 조회하고 기여할 수 있습니다.

서버 시험 운영은 [운영 매뉴얼](OPERATIONS.md)을 따릅니다. 서비스 동작의 기준은 [서버 요구사항](spec/service/agent-context/SRS.md)과 [서버 설계](spec/service/agent-context/SDD.md)이며, 이 문서는 프로젝트 개요와 시작 방법을 안내합니다.

## 주요 기능

- 원천·파생·사건 컨텍스트의 저장, 조회, 갱신, 폐기와 복구
- 의미 유사도·키워드·시간 필터 검색과 컨텍스트 관계 탐색
- 그래프별 소유자·편집자·열람자 권한과 팀 관리
- 웹 관리 화면의 컨텍스트 조회, 국소 그래프 보기와 운영자 복구
- 표준 OAuth·Bearer·회전형 갱신과 MCP 14종 서버 연결 (실제 호스트 호환성 인수 대기)
- 비동기 임베딩 색인, 그래프 불변식 감사와 검색 품질 평가 도구

현재는 검증 단계입니다. 인증된 모든 계정에 운영자 복구 자격이 있으므로 신뢰할 수 있는 시험 참여자로 접근 범위를 제한해야 합니다. 비밀번호 재설정, 계정 잠금, 2FA와 외부 인가 서버 연동은 현재 제공하지 않습니다. 공개 서비스 전환 여부는 [서비스 현황](spec/service/README.md)과 보안 요구사항을 함께 검토하십시오.

## 구성

현재 서버는 웹 관리·계정 세션·OAuth·보호 MCP·그래프 저장·검색·색인을 제공합니다. 감사·평가는 별도 실행기로 실행합니다. 서버는 PostgreSQL의 Apache AGE로 그래프를, pgvector로 임베딩을 저장하며 임베딩 제공자는 Gemini를 명시적으로 구성합니다. MCP의 실제 호스트 호환성은 후속 인수 대상입니다.

- 서버: 루트 Go 모듈의 `cmd/server`와 `internal/`
- 개발 환경: 호스트에서 Go 프로그램을 실행하고 `compose.dev.yaml`로 DB를 기동하거나, `server` 프로필로 애플리케이션까지 컨테이너에서 실행
- 현재 터널 연결: 호스트의 `cloudflared`가 `http://localhost:80`으로 전달. 개발용 실행은 아래 「호스트 터널에 연결하는 컨테이너 서버」 절차를 사용
- 운영 환경: 별도 `compose.prod.yaml`로 DB·마이그레이션·서버를 실행하고 같은 호스트 터널에 연결. 설정·적용·백업 절차는 [운영 매뉴얼](OPERATIONS.md)을 사용

## 호스트 터널에 연결하는 컨테이너 서버

`cloudflared`와 Docker는 같은 호스트에서 실행합니다. `compose.dev.yaml`의 `server` 프로필은 DB 준비와 마이그레이션 완료 뒤 서버를 기동하고 `127.0.0.1:80`을 컨테이너의 HTTP 8080에 연결합니다. 터널의 원본 주소는 `http://localhost:80`으로 지정합니다. Docker Compose 2.33.1 이상을 사용하고 호스트의 80번 포트와 `10.204.0.0/24` 네트워크가 사용 가능한지 확인합니다.

개발용 `.env`에 Gemini API 키 등 기존 구성을 채우고 웹 세션 issuer를 실제 터널 도메인으로 설정합니다.

```dotenv
AUTHORIZATION_SERVER_URL=https://agent-context.example.com
```

```sh
docker compose -f compose.dev.yaml --profile server up -d --build --wait
curl --fail --show-error http://localhost:80/healthz
curl --fail --show-error http://localhost:80/readyz
curl --fail --show-error --output /dev/null https://agent-context.example.com/login
```

로컬 상태 확인은 `200`, 터널을 통과한 HTTPS 로그인 화면도 `200`이 기대 결과입니다. 로컬 HTTP `/login`은 HTTPS 전달 헤더가 없으므로 `400 https_required`가 정상입니다. 서버는 호스트 접속 경로인 `10.204.0.1/32`에서 온 단일 HTTPS 전달 헤더만 신뢰합니다. 터널을 통해 전달되는 원본 HTTP는 호스트 내부에 한정되며 호스트 접근 권한도 신뢰 경계에 포함됩니다. `cloudflared`는 이 Compose 파일이 관리하지 않습니다.

이 구성은 개발용 DB 자격 증명과 WAL 자동 정리를 사용합니다. 운영 데이터에는 별도 `compose.prod.yaml`과 [운영 매뉴얼](OPERATIONS.md)의 백업·적용 절차를 사용합니다.

## 로컬 시작

다음 명령은 저장소 루트에서 실행합니다. 기존 `.env`가 운영용이라면 별도 개발 체크아웃에서 진행하십시오.

### 준비 사항

- Go **1.27.1**
- Docker와 Docker Compose: 설치된 판이 제공된 Compose 파일의 `config --quiet` 검사를 통과해야 합니다.
- Gemini API 키: 아래 기본 구성에 필요합니다. 본문과 검색 질의가 외부 API로 전송되므로 이용 정책과 비용을 확인하십시오.
- 브라우저가 신뢰하는 `localhost` TLS 인증서 및 개인 키
- `curl`, 구성 검사에 사용하는 `jq`

기본 셸에 Go가 없다면 IntelliJ IDEA에 등록된 Go 1.27.1 SDK의 통합 터미널이나 실행 구성을 사용합니다.

### Go 도구 체인과 빌드 검사

로컬과 CI는 `go.mod`의 Go 판을 선택하고 같은 검사 스크립트를 실행합니다. 기본 PATH에서 SDK를 찾을 수 없으면 `GO_BINARY`에 등록된 SDK의 `bin/go` 절대 경로를 지정할 수 있습니다.

```sh
sh ./check-go.sh
# 현재 macOS 개발 환경의 SDK를 직접 지정하는 예시
GO_BINARY="$HOME/sdk/go1.27.1/bin/go" sh ./check-go.sh
```

스크립트는 선택된 SDK의 정확한 판과 모듈 체크섬을 확인하고, pgx/pgxpool·go-jose·MCP SDK를 포함한 최소 실행 파일을 임시 디렉터리에 빌드한 뒤 `go build ./...`·`go vet ./...`·`gofmt -l .`을 검사합니다. 임시 산출물은 종료 시 정리합니다. `.github/workflows/go.yml`이 PR과 `dev`·`main` push에서 같은 검사를 실행하며 모듈·통합 시험은 [개발 계획의 시험 시점](spec/service/agent-context/DEVELOPMENT_PLAN.md#테스트-작성과-실행-시점)에 수행합니다.

MCP SDK `v1.8.0`은 신규 구현용 의존성으로 먼저 고정했습니다. SDK의 실제 전송 적합성과 호스트 지원은 [MCP-01](spec/service/agent-context/development-plan/mcp.md#mcp-01)과 실제 호스트 인수에서 판정합니다.

### 1. 환경 설정

```sh
go version
docker compose version
test -e .env || cp .env.example .env
chmod 600 .env
```

`.env`를 편집하여 다음 값을 지정합니다. 인증서 경로는 실제 절대 경로로 바꾸고 개인 키를 저장소에 추가하지 않습니다.

```dotenv
GEMINI_API_KEY=실제_API_키
HTTP_ADDR=127.0.0.1:8080
TLS_TERMINATION=direct
TRUSTED_PROXY_CIDRS=
TLS_CERT_FILE=/absolute/path/localhost.pem
TLS_KEY_FILE=/absolute/path/localhost-key.pem
```

나머지는 `.env.example`의 개발 기본값으로 시작합니다. `AUTHORIZATION_SERVER_URL=https://localhost:8080`을 웹 세션의 고정 issuer로 사용합니다.

서버는 DB 연결 전에 MCP·OAuth·진단 구성까지 검사합니다. 기존 `.env`에는 두 환경 예시의 `MCP_RESOURCE_URL`, `MCP_ALLOWED_ORIGINS`, `OAUTH_CLIENT_REGISTRATIONS`, `DIAGNOSTIC_INSTANCE_ID`를 추가하고 실제 공개 주소·호스트의 client ID로 맞추십시오. issuer와 resource는 서로 다른 HTTPS origin을 사용할 수 있습니다. 등록 형식과 loopback 포트 정책은 [구성 계약](spec/service/agent-context/sdd/operations.md#목표-추가-구성)을 따릅니다. `DIAGNOSTIC_LOG_DIR`·`DIAGNOSTIC_LOG_LEVEL`은 비우면 각각 `/var/log/agent-context`·`INFO`입니다. OAuth 최초 인가·코드 교환·Bearer 보호 인증·갱신 회전은 구현했으며 MCP 전송 조립과 영속 진단 파일·볼륨은 후속 구현·인수 대상입니다.

Go 프로그램은 `.env`를 자동으로 읽지 않습니다. 직접 작성한 신뢰할 수 있는 `.env`만 현재 셸에 내보내십시오. 이 방식은 파일을 셸 코드로 실행합니다.

```sh
set -a
. ./.env
set +a
```

### 2. DB와 서버 기동

```sh
docker compose -f compose.dev.yaml config --quiet
docker compose -f compose.dev.yaml up -d --wait
go run ./cmd/migrate status
go run ./cmd/migrate up
go run ./cmd/server
```

다른 터미널에서 확인합니다. 개발 기본 포트인 `8080`을 바꿨다면 URL도 바꾸십시오.

```sh
curl --fail --show-error https://localhost:8080/healthz
curl --fail --show-error https://localhost:8080/readyz
```

브라우저에서 `https://localhost:8080/register`로 계정을 생성하고 `/graphs`로 이동합니다. 직접 연결 구현 전에는 웹에서 기존 그래프를 조회·관리할 수 있지만 MCP를 통한 신규 그래프 생성과 컨텍스트 기여는 사용할 수 없습니다. 웹 화면은 조회·시각화와 권한 관리에 사용합니다.

기존 DB의 벡터 타입·차원은 환경 변수만 바꿔 전환할 수 없습니다. [임베딩 전환·복귀 설계](spec/service/agent-context/sdd/indexing.md#전환복귀-경계)를 따르십시오.

### 색인 작업자 분리 실행

기본 `INDEX_WORKER_PLACEMENT=embedded`에서는 서버 안의 작업자가 큐를 소비합니다. 호스트에서 분리하려면 같은 DB·제공자·대상 계층 구성과 환경 변수를 준비한 두 터미널에서 각각 실행합니다.

```sh
# 서버 터미널
INDEX_WORKER_PLACEMENT=external go run ./cmd/server

# 색인 작업자 터미널
INDEX_WORKER_PLACEMENT=external go run ./cmd/indexer
```

`cmd/indexer`는 서버와 같은 배포 구성을 검증하고 HTTP 수신기와 주기 작업 없이 색인만 실행합니다. 별도 바이너리는 `go build -o indexer ./cmd/indexer`로 만듭니다. 서버 컨테이너 이미지의 기본 실행은 `server`이며 위 분리 실행 절차는 호스트 실행 기준입니다.

처리 후에는 1초를 기다리고 빈 회차는 2·4·8·16·30초로 늘려 조회합니다. SIGINT·SIGTERM은 제공자 호출과 소비 루프를 취소하며 최대 10초의 작업자 정리 뒤 연결 풀을 닫습니다. 중단한 작업은 DB에 리스로 남아 만료 뒤 다시 확보됩니다.

### 3. 현재 제공 범위

OAuth 발견 메타데이터 두 경로와 `/authorize`의 로그인·동의·PKCE S256, `/token`의 최초 교환·갱신 회전, Bearer 보호 `/mcp`와 등록·읽기·쓰기 14종을 서버 기동 경로에 연결했습니다. [ARCH-06](spec/service/agent-context/reviews/2026-10-23-arch-06.md)·[ARCH-07](spec/service/agent-context/reviews/2026-10-23-arch-07.md)의 조립·요청 경계 인수와 실제 호스트 지원은 구분합니다. 전체 도구 결과·호스트 호환성 인수는 MCP-09·TEST-07/08에서 수행합니다.

로그인 후 `/connections`에서 본인 OAuth 계열·활성 등록 목록과 폐기를 처리합니다. 계열 폐기는 접근·갱신을 차단하고 웹 세션을 유지하며 등록 폐기는 기존 기여·감사를 보존합니다. [현재 도구 스냅샷](internal/mcp/testdata/tools.json)은 protocol revision·instructions·14종 설명/입력 스키마/annotations를 담으며 `go test ./internal/mcp -run '^TestOPS08ToolSnapshot$'`로 대조합니다. 계약 변경을 검토한 경우에만 `UPDATE_TOOL_SNAPSHOT=1`을 함께 지정하여 갱신하고 diff를 검토합니다.

전환과 복귀는 [운영 절차](OPERATIONS.md#7-변경-및-복귀)를 따릅니다. `GO_BINARY=/path/to/go1.27.1/bin/go sh test-transition.sh`는 합성 이전 스키마의 정지·백업·새 번호 적용·전체 감사·준비 확인과 실패 파일 롤백·별도 볼륨/보관 이미지 복귀를 검증합니다. 실제 운영 배포나 외부 공개를 실행하지 않습니다.

[WEB-01 검토](spec/service/agent-context/reviews/2026-10-21-web-01.md)에서 공통 레이아웃·내장 CSS·Pretendard·라이선스, 입력 이스케이프와 자산 ETag/304를 인수했습니다. 당시 69/124개 완료였으며 다음은 WEB-02 요청 경계였습니다. 웹 회귀 411건과 race·전체 정적 검사가 통과했으며 DB 권한 통합 2건은 건너뛰었습니다. 실제 브라우저·보조 기술 인수는 후속 범위입니다.

[WEB-02 검토](spec/service/agent-context/reviews/2026-10-21-web-02.md)에서 출처·세션·계정 요청 경계와 응답 범위를 인수했습니다. 저장 장애를 404로 숨기지 않고 다른 그래프·잘못된 간선을 렌더링 전에 차단합니다. 실제 격리 DB·TLS를 포함한 717건·race·전체 정적 검사가 실패·건너뜀 없이 통과했습니다. 당시 70/124개 완료였으며 서버 조립을 위한 다음 선행은 OBS-01→OBS-02였습니다.

[OBS-01 검토](spec/service/agent-context/reviews/2026-10-21-obs-01.md)에서 여섯 진단 종류·안전 필드·고정 코드와 요청/인가/작업/시험 식별자 계약을 인수했습니다. 미확인 ID·임의 코드·원문 URI를 거부하며 요청에서 비동기 작업으로의 연결과 기동 회차를 구분합니다. 진단 57건·패키지 의존 1건, 총 58건·race·전체 정적 검사가 통과했습니다. 당시 71/124개 완료였으며 다음은 OBS-02의 영속 파일·권한·잠금이었습니다. 실제 파일 기록과 업무 계측은 후속 범위입니다.

[OBS-02 검토](spec/service/agent-context/reviews/2026-10-21-obs-02.md)에서 여섯 append 파일·0700/0600·소유권과 실제 프로세스 잠금을 인수했습니다. 기존 파일을 보존하며 기동 실패·강제 종료 뒤 잠금을 회복하고, 심볼릭/하드 링크·경로 교체·tmpfs·읽기 전용 저장소를 거부합니다. macOS 98건(race 포함)·Linux 100건·서버 의존 1건과 전체 정적 검사가 통과했습니다. 당시 72/124개 완료였으며 다음은 OBS-03의 대기열·JSON Lines·이벤트 크기였습니다. 회전·보존·총상한과 배포 영속성 인수는 후속 범위입니다.

[OBS-03 검토](spec/service/agent-context/reviews/2026-10-21-obs-03.md)에서 공용 대기열의 1024건·16 MiB 제한, 개행 포함 16 KiB JSON Lines·선택 필드 생략과 누락 집계를 인수했습니다. 실제 파일 쓰기를 단일 작업자로 직렬화하며 대기 포화·OS 잠금 장애가 enqueue를 기다리게 하지 않습니다. macOS 진단 122건·서버 의존 1건(race 포함), Linux OBS-03 24건과 전체 정적 검사가 통과했습니다. 당시 73/124개 완료였으며 다음은 OBS-04의 회전·보존·총상한이었습니다. stderr 통지·부분 행 격리·5초 종료와 전체 서버 조립은 후속 범위입니다.

[OBS-04 검토](spec/service/agent-context/reviews/2026-10-21-obs-04.md)에서 UTC 날짜·크기 회전과 7일 보존, 종류별·인스턴스별·저장소 전체 상한을 인수했습니다. 공유 잠금 아래 이전 슬롯까지 합산하고, 혼합 만료 파일은 임시 공간까지 예약한 뒤 유효 행을 보존해 교체합니다. macOS 진단 145건·서버 의존 1건(race 포함), Linux OBS-03/04 47건과 전체 정적 검사가 통과했습니다. 당시 74/124개 완료였으며 다음은 OBS-05의 처리 흐름 계측이었습니다. 실제 배포·전체 파일 통합과 stderr·부분 행 격리·5초 종료는 후속 범위입니다.

## 개발 환경 재생성

개발 환경을 처음부터 만들 때는 기존 개발 볼륨을 제거한 뒤 기동합니다. 이 절차는 개발 DB와 WAL 데이터를 삭제하므로 보존할 데이터가 없는 개발 환경에 적용합니다. 마이그레이션을 승인된 개발 통합 예외로 `001_init.sql`에 합쳐 checksum이 달라진 경우에도 이전 개발·시험 DB를 재생성합니다. 적용 조건은 [마이그레이션 계약](migrations/AGENTS.md)을 따릅니다.

```sh
docker compose -f compose.dev.yaml down -v
docker compose -f compose.dev.yaml up -d --wait
```

재생성 뒤에는 위 기동 절차의 마이그레이션을 적용합니다.

## 검증

DB 통합 검증은 운영 데이터가 없는 전용 개발·시험 DB에서 실행합니다. `.env.example`의 `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED=1`을 내보낸 상태에서 진행하십시오. 서명 키 통합 시험은 임시 DB를 만들므로 시험 계정에 DB 생성 권한이 필요합니다.

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .
sh ./test-dev-compose.sh
sh ./test-prod-compose.sh
```

`TEST_DATABASE_URL`이 없고 필수 설정도 없으면 DB 통합 시험은 건너뜁니다. 이를 통합 검증 성공으로 간주하지 않습니다. `gofmt -l .`의 정상 결과는 출력이 없는 상태입니다.

별도 격리 환경을 만드는 검증은 다음과 같습니다. 이미지 빌드·컨테이너 기동과 실행 시간이 필요하므로 선택하여 실행합니다. 개발 Compose 실행 검사는 DB·WAL 정리만 기동하며 외부 API를 호출하지 않습니다. 기존 `.env`·DB·볼륨을 사용하지 않고 별도 프로젝트의 합성 자격·임시 loopback 포트·새 볼륨을 사용합니다. 두 DB 서비스 이미지를 캐시 없이 빌드한 뒤 PostgreSQL 주 판·AGE/pgvector 설치 판·벡터/그래프 SQL·WAL 경로 권한을 확인하고, 시험 컨테이너·볼륨·네트워크를 정리합니다. 서버 이미지·실제 Tunnel 연결과 WAL 보관/시점 복구는 각각 별도 인수 범위입니다.

```sh
sh ./test-dev-compose.sh --runtime
sh ./test-wal-archive.sh
```

기록기 분야 인수는 macOS에서 `go test -race -count=1 -skip '^TestOBS08ContainerPersistence$' ./internal/diagnostic`, Linux 격리 환경에서는 `sh internal/diagnostic/test-linux.sh`로 실행합니다. Linux 스크립트는 기존 로컬 이미지 ID를 고정하며 `ACS_OBS08_IMAGE`로 이미 설치된 Linux arm64/amd64 이미지를 지정할 수 있습니다. Go 1.27.1과 Docker가 필요하고 이미지 다운로드·DB·네트워크는 사용하지 않습니다. 전용 볼륨에서 전체 파일·커널 쓰기 오류·컨테이너 재생성·슬롯 교체를 검증하고 자신이 만든 자원만 정리합니다. 결과 JSON·stderr는 출력한 임시 결과 경로에 남습니다. 상세 범위와 디스크 부족 주입의 제한은 [OBS-08 검토](spec/service/agent-context/reviews/2026-10-23-obs-08.md)를 따릅니다.

그래프 불변식 감사는 `go run ./cmd/audit`, 검색 품질 평가는 `cmd/eval`이 담당합니다. 평가 표본과 실행 방법은 [테스트 설계](spec/service/agent-context/sdd/testing.md)와 [평가 검증 설계](spec/service/agent-context/sdd/evaluation.md#검증을-가능하게-하는-설계)를 확인하십시오.

색인 시험은 기본 회귀에서 로컬 TLS 합성 제공자를 사용합니다. 실제 Gemini 전송은 `embedding_live` 태그의 별도 시험이며, 외부 전송·비용 허용과 키 주입이 필요합니다. 두 경로의 호출 상한·격리 DB·실행 조건은 [임베딩 실연결 시험 경계](spec/service/agent-context/sdd/testing.md#임베딩-제공자-실연결-테스트)를 따릅니다.

## 문서와 저장소 구조

- [운영 매뉴얼](OPERATIONS.md): 시험 운영 배포, 점검, 백업·복구, 변경과 종료
- [서비스 목록 및 현황](spec/service/README.md): 서비스별 요구사항·설계 진입점
- [용어 정리집](spec/DICTIONARY.md): 프로젝트 용어의 기준
- [서버 요구사항](spec/service/agent-context/SRS.md) / [설계](spec/service/agent-context/SDD.md)
- [개발 설정 예시](.env.example) / [운영 설정 예시](.env.prod.example)
- [마이그레이션 계약](migrations/AGENTS.md): 적용 이력과 임베딩 스키마 전환
- [작업 계약](AGENTS.md): 변경 시 따라야 할 DOX 계층과 검증 기준

`cmd/`에는 서버·마이그레이션·감사·평가 실행기가, `internal/`에는 서버 구현이, `migrations/`에는 스키마가 있습니다. `spec/`은 요구사항·설계와 프로젝트 용어를 소유합니다.

[OBS-05 검토](spec/service/agent-context/reviews/2026-10-21-obs-05.md)에서 요청·인증·검색·색인·관리·시스템 처리 지점에 안전한 계측을 연결했습니다. 실제 상관 ID·색인 세대·계열 흐름과 커밋/롤백/미확인 결과를 구분합니다. 관련 모듈·회귀 2753건(하위 사례 포함), race와 전체 정적 검사가 통과했습니다. 기존 색인 시험 3개 그룹의 실패는 [후속 수정](spec/service/agent-context/reviews/2026-10-21-obs-05.md#기존-시험-실패-수정)으로 해결했으며, 제외 없이 색인·서버 두 패키지 479건이 통과했습니다. 당시 75/124개 완료였으며 다음은 OBS-06의 감사 통합 조회였습니다. 전체 서버·호스트·배포 및 OBS-07/08의 파일 장애·종료·통합 인수는 후속 범위입니다.

[OBS-06 검토](spec/service/agent-context/reviews/2026-10-22-obs-06.md)에서 소유자의 감사 통합 페이지와 기간·대상 필터, 직접/상속 소유자 및 복구 상태 근거의 보존을 인수했습니다. 관련 733건(하위 사례 포함), race와 전체 정적 검사가 통과했습니다. 당시 76/124개 완료였으며 다음은 OBS-07의 파일 장애·stderr·부분 쓰기·종료였습니다. 감사 필터·더 보기 UI는 WEB-08 범위입니다.

[OBS-07 검토](spec/service/agent-context/reviews/2026-10-23-obs-07.md)에서 안전한 stderr 알림·부분 행 복구·최대 5초 종료를 인수했습니다. 관련 172건(하위 사례 포함), race·전체 정적 검사·Linux 교차 빌드가 통과했고 실제 DB에서 진단 파일 장애 뒤 관계와 업무 감사의 보존을 확인했습니다. 당시 77/124개 완료였으며 다음은 OBS-08의 실제 파일·재시작·다중 instance 통합 인수였습니다. 배포 stderr 수집·보관과 Linux 실행 인수는 후속 범위입니다.

[OBS-08 검토](spec/service/agent-context/reviews/2026-10-23-obs-08.md)에서 여섯 파일·흐름·권한·비밀 제외와 상한/보존·장애/누락, 컨테이너 재생성·슬롯 교체를 인수했습니다. macOS 진단 164건·HTTP/의존 2건(race 포함), Linux 174건과 전체 정적 검사가 통과했습니다. 당시 78/124개 완료로 관측성 8개 항목을 마쳤으며 다음은 ARCH-06의 애플리케이션 전체 조립이었습니다. 운영 배포·다중 호스트·외부 stderr 영속 수집은 후속 범위입니다.

[ARCH-06 검토](spec/service/agent-context/reviews/2026-10-23-arch-06.md)에서 기록기·저장소·인가/웹·스냅숏 검색·색인 제공자와 실제 14종 MCP 업무의 기동 조립을 인수했습니다. 조립 성공 후에만 작업자를 시작하며 부분 실패와 정상 종료의 정리를 공유합니다. 실제 DB·TLS 제공자·Bearer 요청과 기존 배치 프로세스를 포함한 서버 369건·MCP 433건, 총 802건(하위 사례 포함)과 race·전체 정적 검사가 통과했습니다. 당시 79/124개 완료였으며 다음은 ARCH-07의 HTTP 진입점·공통 요청 경계였습니다. 전체 운영·호스트·배포 인수는 후속 범위입니다.

[ARCH-07 검토](spec/service/agent-context/reviews/2026-10-23-arch-07.md)에서 24개 HTTP 경로·메서드·TLS 선행 판정과 상관 ID·공통 시간/본문 제한·취소를 인수했습니다. 미완성 본문 정리의 읽기 마감 수명을 보완하고 실제 느린 수신·단절·keep-alive 재사용을 확인했습니다. 신규 204건을 포함한 서버 전체 573건(하위 사례 포함), race·전체 정적 검사가 통과했습니다. 당시 80/124개 완료였으며 다음 권장은 OPS-04의 주기 작업·실행권·보존 조건이었습니다. 전체 운영 실패·MCP/호스트·배포 인수는 후속 범위입니다.

[OPS-04 검토](spec/service/agent-context/reviews/2026-10-23-ops-04.md)에서 여덟 DB 작업과 기록기 소유의 시간별 파일 정리를 연결했습니다. OAuth 상태의 보존 재검사·원자적 정리와 저장소/instance별 진단 실행권, 정리 중단 뒤 기록/flush 유지를 확인했습니다. 서버 580건·진단 168건·저장 6건·인증 48건, 총 802건(하위 사례 포함), race·전체 정적 검사·Linux 대상 빌드가 통과했습니다. 당시 81/124개 완료였으며 다음 권장은 OPS-05였습니다. 전체 운영 실패·Linux runtime·배포 인수는 후속 범위입니다.

[OPS-05 검토](spec/service/agent-context/reviews/2026-10-23-ops-05.md)에서 기동·수신 실패·정상 종료의 공통 정리와 원인 보존을 인수했습니다. 실제 HTTP·DB 연결과 파일 잠금으로 요청 30초·작업자 10초·진단 flush 5초를 확인하고 Compose의 50초 구성과 대조했습니다. 서버 595건·진단 168건, 총 763건(하위 사례 포함), race·전체 정적 검사·Linux 대상 빌드가 통과했습니다. 당시 82/124개 완료였으며 다음 권장은 OPS-06이었습니다. 컨테이너 강제 종료·전체 운영 장애·Linux runtime·배포 인수는 후속 범위입니다.

[OPS-06 검토](spec/service/agent-context/reviews/2026-10-23-ops-06.md)에서 자동 삭제 그래프의 복구 요청·운영자 처리와 취소 시 롤백을 인수했습니다. 요청 자격·선택 순서·저장 실패의 원자성·두 인스턴스의 단일 적용·감사 정리와 실제 세션 주체를 확인했습니다. 관련 시험 53건(하위 사례 포함), race·전체 정적 검사·Linux 대상 빌드가 통과했습니다. 당시 83/124개 완료였으며 다음 권장은 OPS-07이었습니다. 실제 브라우저·Linux runtime·전체 운영 장애·배포 인수는 후속 범위입니다.

[OPS-07 검토](spec/service/agent-context/reviews/2026-10-23-ops-07.md)에서 WAL 보관 회귀와 cold tablespace를 포함한 물리 백업·시점 복구를 인수했습니다. 목표 일시 정지·승격, AGE·양쪽 HNSW·기존 세션 서명·큐 소비·전체 감사와 생성 자원 정리를 확인했습니다. 격리 시험과 전체 정적 검사가 통과했습니다. 당시 84/124개 완료였으며 다음 권장은 OPS-02였습니다. 실제 운영 백업·외부 보관·보관본 삭제·배포는 후속 범위입니다.

[OPS-02 검토](spec/service/agent-context/reviews/2026-10-24-ops-02.md)에서 운영 템플릿과 영속 진단 볼륨·안정 슬롯을 인수했습니다. DB 비공개·loopback·고정 신뢰와 서버 이미지의 실제 권한·중복/권한 거부·재생성 보존을 확인했습니다. 구성 303건·race·정적 검사와 격리 컨테이너 검증이 통과했습니다. 당시 85/124개 완료였으며 다음 권장은 OPS-08 실행 안내·전환·복귀 절차였습니다. 실제 운영 배포·Tunnel·외부 제공자 인수는 후속 범위입니다.

[OPS-08 모듈 인수](spec/service/agent-context/reviews/2026-10-24-ops-08.md)에서 실행 안내와 도구 14종 스냅샷, 격리 배포의 정지·물리 백업·013 적용·감사·준비와 실패 파일 롤백·보관 이미지/새 복원본 복귀를 확인했습니다. 격리 실행·자원 정리, 도구 목록 17건·race·전체 Go 정적 검사가 통과했습니다. 당시 전체 **86/124개 완료**였으며 다음 권장은 [WEB-03](spec/service/agent-context/development-plan/web.md#web-03)입니다. 실제 운영 배포·외부 공개·호스트·Tunnel 인수는 후속 범위입니다.

[WEB-03 모듈 인수](spec/service/agent-context/reviews/2026-10-24-web-03.md)에서 그래프 목록의 등급 정규화·커서 입력 오류·취소와 실제 DB/TLS의 정렬·페이지·현재 권한 및 본인 복구 요청 진입을 확인했습니다. 웹 642건·race·전체 Go 정적 검사가 실패·건너뜀 없이 통과했습니다. 당시 전체 **87/124개 완료**이며 다음 권장은 [WEB-04](spec/service/agent-context/development-plan/web.md#web-04)입니다. 실제 브라우저·보조 기술·전체 웹 인수는 후속 범위입니다.

[WEB-04 모듈 인수](spec/service/agent-context/reviews/2026-10-24-web-04.md)에서 권한 회수의 본문 설명·팀별 그래프 등급·관리자 범위와 실제 DB/TLS의 회수/부여·현재 권한 재검사를 확인했습니다. 웹 662건·race·전체 Go 정적 검사가 실패·건너뜀 없이 통과했습니다. 당시 전체 **88/124개 완료**이며 다음 권장은 [WEB-05](spec/service/agent-context/development-plan/web.md#web-05)입니다. 실제 브라우저·보조 기술·전체 웹 인수는 후속 범위입니다.

[WEB-05 모듈 인수](spec/service/agent-context/reviews/2026-10-24-web-05.md)에서 삭제/복구 대상 보기·계층별 영향·운영자 처리 확인과 실제 DB/TLS의 저장량 한도·등급 상실 재판정·요청자 직접 소유자 복원을 확인했습니다. 웹 743건·race·전체 Go 정적 검사가 실패·건너뜀 없이 통과했습니다. 당시 전체 **89/124개 완료**이며 다음 권장은 [WEB-06](spec/service/agent-context/development-plan/web.md#web-06)입니다. 실제 브라우저·보조 기술·전체 웹 인수는 후속 범위입니다.

[WEB-06 모듈 인수](spec/service/agent-context/reviews/2026-10-24-web-06.md)에서 그래프 속성·전역 요약 영역·연결 종류 구분과 실제 DB/TLS의 시작점·슬라이더·절단·삭제 노드 제외를 확인했습니다. 웹 751건·race·Node.js 상태 전이·전체 Go 정적 검사가 실패·건너뜀 없이 통과했습니다. 현재 전체 **90/124개 완료**이며 다음 권장은 [WEB-07](spec/service/agent-context/development-plan/web.md#web-07)입니다. 실제 브라우저·보조 기술·전체 웹 인수는 후속 범위입니다.
