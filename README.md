# 에이전트 컨텍스트 공유 시스템

AI 에이전트가 작업 중 얻은 지식과 맥락을 저장하고, 관계를 연결하여 이후 작업에서 다시 활용하는 AI 에이전트 제텔카스텐입니다. 여러 작업자가 컨텍스트 그래프를 공유하며 계정별 권한 범위 안에서 조회하고 기여할 수 있습니다.

이 문서는 프로젝트 개요, 개발 시작과 클라이언트 연결 방법을 안내합니다. 서비스 동작의 기준은 [서버 요구사항](spec/service/agent-context/SRS.md)과 [서버 설계](spec/service/agent-context/SDD.md)이며, 서버 시험 운영은 [운영 매뉴얼](OPERATIONS.md)을 따릅니다.

> [!WARNING]
> 현재는 검증 단계입니다. 인증된 모든 계정에 운영자 복구 자격이 있으므로 신뢰할 수 있는 시험 참여자로 접근 범위를 제한하십시오. 비밀번호 재설정, 계정 잠금, 2FA와 외부 인가 서버 연동은 제공하지 않습니다. 공개 서비스 전환 여부는 [서비스 현황](spec/service/README.md)과 보안 요구사항을 함께 검토하십시오.

## 목차

- [주요 기능](#주요-기능)
- [구성](#구성)
- [로컬 시작](#로컬-시작)
- [호스트 터널에 연결하는 컨테이너 서버](#호스트-터널에-연결하는-컨테이너-서버)
- [클라이언트 연결](#클라이언트-연결)
- [개발 환경 재생성](#개발-환경-재생성)
- [검증](#검증)
- [문서와 저장소 구조](#문서와-저장소-구조)
- [검증 현황](#검증-현황)

## 주요 기능

- 원천·파생·사건 컨텍스트의 저장, 조회, 갱신, 폐기와 복구
- 의미 유사도·키워드·시간 필터 검색과 컨텍스트 관계 탐색
- 그래프별 소유자·편집자·열람자 권한과 팀 관리
- 웹 관리 화면의 컨텍스트 조회, 국소 그래프 보기, 연결 관리와 운영자 복구
- 공개 클라이언트 자동 등록(DCR)과 표준 OAuth(PKCE S256·회전형 갱신)로 보호하는 MCP 서버, 14종 도구
- 비동기 임베딩 색인, 그래프 불변식 감사와 검색 품질 평가 도구

## 구성

서버는 웹 관리·계정 세션·OAuth·보호 MCP·그래프 저장·검색·색인을 제공하며, 감사·평가는 별도 실행기로 실행합니다. PostgreSQL의 Apache AGE에 그래프를, pgvector에 임베딩을 저장하고 임베딩 제공자는 Gemini를 명시적으로 구성합니다.

| 실행기 | 역할 | 운영 이미지 포함 |
|---|---|---|
| `cmd/server` | 웹·OAuth·MCP 서버, 기본 내장 색인 작업자 | 예 |
| `cmd/migrate` | 스키마 조회(`status`)·적용(`up`) | 예 |
| `cmd/audit` | 그래프 불변식 전체 감사 | 예 |
| `cmd/indexer` | 분리 실행용 색인 작업자 | 아니요 |
| `cmd/eval` | 검색 품질 평가 | 아니요 |

| 환경 | 구성 |
|---|---|
| 로컬 개발 | 호스트에서 Go 프로그램을 실행하고 `compose.dev.yaml`로 DB를 기동합니다. |
| 개발 컨테이너 | `compose.dev.yaml`의 `server` 프로필로 애플리케이션까지 컨테이너에서 실행하고 호스트의 `cloudflared`에 연결합니다. |
| 시험 운영 | `compose.prod.yaml`로 DB·마이그레이션·서버를 실행하고 같은 방식의 호스트 터널에 연결합니다. 절차는 [운영 매뉴얼](OPERATIONS.md)을 따릅니다. |

## 로컬 시작

명령은 저장소 루트에서 실행합니다. 기존 `.env`가 운영용이라면 별도 개발 체크아웃에서 진행하십시오.

### 준비 사항

- Go **1.27.4**. 기본 셸에 없다면 IntelliJ IDEA에 등록된 Go 1.27.4 SDK의 통합 터미널이나 실행 구성을 사용합니다.
- Docker와 Docker Compose: 설치된 판이 제공된 Compose 파일의 `config --quiet` 검사를 통과해야 합니다.
- Gemini API 키: 본문과 검색 질의가 외부 API로 전송되므로 이용 정책과 비용을 확인하십시오.
- 브라우저가 신뢰하는 `localhost` TLS 인증서와 개인 키
- `curl`, 구성 검사에 사용하는 `jq`

### Go 도구 체인과 빌드 검사

로컬과 CI는 `go.mod`의 Go 판을 선택하고 같은 검사 스크립트를 실행합니다. 기본 PATH에서 SDK를 찾을 수 없으면 `GO_BINARY`에 SDK의 `bin/go` 절대 경로를 지정합니다.

```sh
sh ./check-go.sh
# SDK를 직접 지정하는 예시
GO_BINARY="$HOME/sdk/go1.27.4/bin/go" sh ./check-go.sh
```

스크립트는 다음을 차례로 검사하고 임시 산출물을 정리합니다.

1. 선택된 SDK의 정확한 판과 모듈 체크섬
2. pgx/pgxpool·go-jose·MCP SDK를 포함한 최소 실행 파일 빌드
3. `go build ./...`·`go vet ./...`·`gofmt -l .`
4. `govulncheck` `v1.9.0`으로 `./...` 호출 경로의 알려진 취약점. 취약점 정보는 실행 시 Go 공식 취약점 DB에서 조회하므로 네트워크가 필요합니다.

`.github/workflows/go.yml`이 PR과 `dev`·`main` push에서 같은 검사를 실행합니다. 모듈·통합 시험은 [개발 계획의 시험 시점](spec/service/agent-context/DEVELOPMENT_PLAN.md#테스트-작성과-실행-시점)에 수행합니다.

### 1. 환경 설정

```sh
go version
docker compose version
test -e .env || cp .env.example .env
chmod 600 .env
```

`.env`에서 다음 값을 지정합니다. 인증서 경로는 실제 절대 경로로 바꾸고 개인 키를 저장소에 추가하지 않습니다.

```dotenv
GEMINI_API_KEY=실제_API_키
HTTP_ADDR=127.0.0.1:8080
TLS_TERMINATION=direct
TRUSTED_PROXY_CIDRS=
TLS_CERT_FILE=/absolute/path/localhost.pem
TLS_KEY_FILE=/absolute/path/localhost-key.pem
```

나머지는 `.env.example`의 개발 기본값으로 시작합니다. 서버는 DB 연결 전에 MCP·OAuth·진단 구성까지 검사하므로 다음 필수 값이 있어야 합니다. 기존 `.env`에 없다면 예시에서 옮겨 옵니다.

| 항목 | 개발 기본값과 설명 |
|---|---|
| `AUTHORIZATION_SERVER_URL` | `https://localhost:8080`. 웹 세션과 OAuth의 고정 issuer입니다. |
| `MCP_RESOURCE_URL`·`MCP_ALLOWED_ORIGINS` | `https://localhost:8080/mcp`·`https://localhost:8080`. issuer와 다른 HTTPS origin을 사용할 수 있습니다. |
| `DIAGNOSTIC_INSTANCE_ID` | 재시작에도 유지할 진단 슬롯 ID입니다. `DIAGNOSTIC_LOG_DIR`·`DIAGNOSTIC_LOG_LEVEL`은 비우면 각각 `/var/log/agent-context`·`INFO`입니다. |

전체 항목과 검증 규칙은 [배포 구성](spec/service/agent-context/sdd/operations.md#배포-구성)과 [목표 추가 구성](spec/service/agent-context/sdd/operations.md#목표-추가-구성)을 따릅니다.

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

다른 터미널에서 상태를 확인합니다. 개발 기본 포트 `8080`을 바꿨다면 URL도 바꾸십시오.

```sh
curl --fail --show-error https://localhost:8080/healthz
curl --fail --show-error https://localhost:8080/readyz
```

브라우저에서 `https://localhost:8080/register`로 계정을 만들고 `/graphs`로 이동합니다. 웹 화면은 그래프·권한/팀·삭제/복구·감사·연결 관리와 계층/상태·근거 시각화를 제공합니다. 그래프 생성과 컨텍스트 기여는 MCP 클라이언트로 수행합니다.

기존 DB의 벡터 타입·차원은 환경 변수만 바꿔 전환할 수 없습니다. [임베딩 전환·복귀 설계](spec/service/agent-context/sdd/indexing.md#전환복귀-경계)를 따르십시오.

### 색인 작업자 분리 실행

기본값 `INDEX_WORKER_PLACEMENT=embedded`에서는 서버 안의 작업자가 큐를 소비합니다. 분리하려면 같은 DB·제공자·대상 계층 구성의 환경 변수를 준비한 두 터미널에서 각각 실행합니다.

```sh
# 서버 터미널
INDEX_WORKER_PLACEMENT=external go run ./cmd/server

# 색인 작업자 터미널
INDEX_WORKER_PLACEMENT=external go run ./cmd/indexer
```

- `cmd/indexer`는 서버와 같은 배포 구성을 검증하고 HTTP 수신기와 주기 작업 없이 색인만 실행합니다. 별도 바이너리는 `go build -o indexer ./cmd/indexer`로 만듭니다.
- 처리 후 1초를 기다리고, 빈 회차가 이어지면 2·4·8·16·30초로 간격을 늘립니다.
- SIGINT·SIGTERM은 제공자 호출과 소비 루프를 취소하고 최대 10초의 작업자 정리 뒤 연결 풀을 닫습니다. 중단한 작업은 DB에 리스로 남아 만료 뒤 다시 확보됩니다.
- 서버 컨테이너 이미지의 기본 실행은 `server`이며 이 절차는 호스트 실행 기준입니다.

## 호스트 터널에 연결하는 컨테이너 서버

`cloudflared`와 Docker는 같은 호스트에서 실행합니다. `compose.dev.yaml`의 `server` 프로필은 DB 준비와 마이그레이션 완료 뒤 서버를 기동하고 `127.0.0.1:80`을 컨테이너의 HTTP 8080에 연결합니다.

- 터널의 원본 주소는 `http://localhost:80`으로 지정합니다.
- Docker Compose 2.33.1 이상을 사용하고 호스트의 80번 포트와 `10.204.0.0/24` 네트워크가 비어 있는지 확인합니다.
- 개발용 `.env`에 Gemini API 키 등 기존 구성을 채우고, issuer·MCP 주소를 실제 터널 도메인의 HTTPS 주소로 바꿉니다.

```dotenv
AUTHORIZATION_SERVER_URL=https://agent-context.example.com
MCP_RESOURCE_URL=https://agent-context.example.com/mcp
MCP_ALLOWED_ORIGINS=https://agent-context.example.com
```

```sh
docker compose -f compose.dev.yaml --profile server up -d --build --wait
curl --fail --show-error http://localhost:80/healthz
curl --fail --show-error http://localhost:80/readyz
curl --fail --show-error --output /dev/null https://agent-context.example.com/login
```

세 요청 모두 `200`이 기대 결과입니다. 로컬 HTTP로 `/login`에 직접 접속하면 HTTPS 전달 헤더가 없으므로 `400 https_required`가 정상입니다.

서버는 호스트 접속 경로 `10.204.0.1/32`에서 온 단일 HTTPS 전달 헤더만 신뢰합니다. 터널이 전달하는 원본 HTTP는 호스트 내부에 한정되며, 호스트 접근 권한도 신뢰 경계에 포함됩니다. `cloudflared`는 이 Compose 파일이 관리하지 않습니다.

이 구성은 개발용 DB 자격 증명과 WAL 자동 정리를 사용합니다. 운영 데이터에는 `compose.prod.yaml`과 [운영 매뉴얼](OPERATIONS.md)의 백업·적용 절차를 사용합니다.

## 클라이언트 연결

MCP 클라이언트는 `<공개 주소>/mcp`에 Streamable HTTP로 연결하고 OAuth로 인가받습니다. 서버는 **공개 클라이언트 동적 등록(DCR)만** 지원합니다. 클라이언트가 callback을 등록하면 서버가 client ID를 발급하여 DB에 저장합니다. 사전 등록과 Client ID Metadata Documents는 제공하지 않습니다.

여러 참여자는 각자의 환경에서 같은 서버 주소를 설정하고, 본인 계정으로 가입·로그인한 뒤 연결을 허용합니다. 운영자가 참여자별 client ID를 등록하거나 서버를 재기동할 필요는 없습니다. client ID는 사용자 계정 ID와 별개이며 서버가 자동으로 발급하므로 사용자가 직접 정하거나 설정에 복사하지 않습니다.

**한 계정에서 여러 client ID로 연결할 수 있습니다.** 같은 계정으로 Codex·Claude Code·다른 PC의 클라이언트를 각각 연결하면 그래프 접근 권한은 해당 계정을 기준으로 적용하고, 토큰과 인가 연결은 연결별로 관리합니다. 다른 참여자는 각자의 계정을 사용하며 비밀번호와 저장된 인증 자격은 공유하지 않습니다. 상세 계약과 등록 한도는 [인증 설계](spec/service/agent-context/sdd/authentication.md#oauth-discovery와-최초-인가)를 따릅니다.

이전 사전 등록 배포에서 전환할 때는 새 마이그레이션을 적용하고 서버를 갱신한 뒤 각 클라이언트에서 다시 인가받습니다. 기존 `OAUTH_CLIENT_REGISTRATIONS`는 제거하며 서버는 이 값을 읽지 않습니다. Codex 설정의 고정 `oauth.client_id`도 제거합니다. 이전 고정 ID와 토큰은 동적 등록으로 자동 이관하지 않습니다. 서버의 계정·그래프·감사와 기존 계열 데이터는 삭제하지 않습니다.

### 1. Codex CLI

`~/.codex/config.toml`에 다음을 추가합니다. 최상위 키는 다른 테이블보다 앞에 둡니다. 서버별 `oauth.client_id`와 client secret은 설정하지 않습니다.

```toml
features.mcp_2026_07_28 = true
mcp_oauth_credentials_store = "file"

[mcp_servers.agent-context]
url = "https://agent-context.example.com/mcp"
scopes = ["agent-context"]

[mcp_servers.agent-context.oauth]
callback_url = "http://127.0.0.1:49152/callback"
callback_port = 49152
```

```sh
codex mcp login agent-context
```

Codex가 발견 메타데이터의 `registration_endpoint`로 등록한 뒤 인증 URL을 엽니다. 브라우저에서 서버에 로그인하거나 가입한 뒤 동의하면 자격이 저장됩니다. 브라우저를 열 수 없는 환경에서는 `--no-browser`를 붙여 출력된 주소로 진행합니다. 사설 CA를 사용하는 로컬 인수 구성에서는 `CODEX_CA_CERTIFICATE`로 CA를 지정합니다.

Codex CLI 0.165.0의 발견→자동 등록→인증 URL 생성까지 격리 환경에서 확인했습니다. 실제 브라우저 로그인 이후의 CLI 전체 인수는 대기이며, 현재 검증 범위는 [검증 현황](#검증-현황)을 확인합니다.

### 2. Claude Code

서버는 DCR로 `http://localhost:PORT/callback`을 등록할 수 있습니다. localhost callback은 포트·경로·query까지 등록 원문과 완전 일치해야 하므로 사용 가능한 고정 callback 포트를 지정합니다. client ID와 client secret은 지정하지 않습니다.

```sh
claude mcp add --transport http --callback-port 49153 agent-context https://agent-context.example.com/mcp
claude mcp login agent-context
```

브라우저에서 본인 계정으로 로그인하고 연결을 허용합니다. 터미널 세션의 `/mcp`에서도 인가를 시작할 수 있습니다. 다른 포트를 선택하면 `--callback-port`에 그 값을 지정하여 새로 등록합니다. 기존 IP literal 등록을 localhost로 치환하지 않습니다.

Claude Code 2.1.316의 발견→자동 등록→인증 URL 생성까지 격리 환경에서 확인했습니다. 실제 CLI의 브라우저 로그인·callback 수신·토큰 저장·도구 호출 전체 인수는 대기입니다. 서버의 동의·코드 교환 시험과 실제 CLI 시험의 범위는 [#300 검토](spec/service/agent-context/reviews/2026-11-02-issue-300.md)를 확인합니다.

도구 정의의 컨텍스트 사용량은 `/context`에서 확인합니다. 지원되는 모델·환경에서는 Tool Search가 정의를 지연 적재하며, `MCP tools (loaded on-demand)`와 도구 수를 함께 확인합니다. `ENABLE_TOOL_SEARCH=false` 또는 서버의 `alwaysLoad: true`는 전체 정의를 미리 적재할 수 있습니다. CLI에서 지연 적재를 명시하려면 `ENABLE_TOOL_SEARCH=true claude`로 시작하며 `ToolSearch`를 차단하거나 기본 도구 목록에서 제외하지 않습니다. `/context all`의 도구별 예상 비용은 실제로 모든 정의가 적재됐다는 뜻이 아닙니다. 분리 CLI의 확인 범위·추정치와 실제 모델 사용량의 차이는 [#305 검토](spec/service/agent-context/reviews/2026-11-04-issue-305.md#커밋-이후-호스트-확인과-호환성-보완)를 확인합니다.

### 3. 다른 MCP 클라이언트

클라이언트가 DCR을 지원해야 하며 서버 주소를 설정하고 자동 OAuth 연결을 시작합니다. HTTP callback은 유효한 포트가 있는 literal loopback IP 또는 정확한 소문자 localhost만 허용합니다. HTTPS·localhost callback은 등록 원문과 완전 일치시키고, 같은 HTTP loopback IP·경로·query에서는 포트 변경을 허용합니다.

Antigravity CLI(`agy`)도 공개 HTTPS 주소와 OAuth 연결을 사용합니다. 인증 후 프롬프트를 보내 도구 목록과 읽기·쓰기 호출이 유지되는지 확인합니다. `notifications/roots/list_changed`는 전환기 호환을 위해 상태 변경 없이 접수합니다. 실제 Antigravity CLI의 전체 연결과 도구 호출은 아직 인수되지 않았습니다.

Claude 앱 등 외부 서비스에서 접근하는 클라이언트는 공개 HTTPS 주소가 필요합니다. DCR 제공만으로 해당 클라이언트의 실제 연결 지원을 선언하지 않습니다. 첫 연결 때 공개 discovery·`/oauth/register`·`/authorize`·`/token`의 요청과 서버 로그를 확인합니다.

### 4. 연결 뒤 사용과 관리

- 에이전트는 노드·관계를 쓰기 전에 `agent_register`로 `agent_id`를 받아 후속 쓰기에 전달합니다. 서버의 instructions가 이 순서를 안내하며, 14종 도구의 설명과 입력 스키마는 [도구 스냅숏](internal/mcp/testdata/tools.json)에 있습니다.
- 웹 `/connections`에서 본인의 OAuth 연결과 활성 에이전트 등록을 확인하고 폐기합니다. 특정 OAuth 연결을 폐기하면 해당 연결의 접근·갱신 토큰이 차단되며 다른 연결과 웹 세션은 유지됩니다. 에이전트 등록을 폐기해도 기존 기여와 감사 기록은 보존됩니다.
- 클라이언트에서 로컬 연결만 삭제하면 서버 자격은 폐기되지 않습니다.

인증 URL에서 「연결 요청을 처리할 수 없습니다. 클라이언트에서 다시 시작해 주세요.」가 표시되면 고정 client ID 설정이 남아 있는지 확인하고 클라이언트에서 인증을 새로 시작합니다. 계속 실패하면 요청의 `client_id`·`redirect_uri`·`resource`와 서버의 DCR 등록을 대조합니다. 특히 localhost callback은 포트까지 일치해야 하며, 오류 문구만으로 실패 원인을 확정할 수는 없습니다.

### 대화 흐름 기록과 안내 진입점

대화 원천을 만들 때 `source_ref.channel=conversation`과 함께 `conversation_id`·`turn_index`를 지정하면 같은 대화의 턴 순서를 보존할 수 있습니다. 기록 시각이나 사건의 `member_refs` 정렬을 대화 순서로 사용하지 않습니다. 기존 원천에는 순서를 추정해 채우지 않습니다.

같은 대화 구간을 사건으로 묶고, 다음 사건의 `node_create`에 같은 `conversation_id`와 `previous_event_id`를 지정하면 사건 저장과 앞 사건의 `precedes` 확정을 한 번에 처리합니다. 연결이 잘못되면 사건 생성도 함께 거부됩니다. 원천끼리 사건 관계를 연결하지 않습니다.

주제 안내는 기존 요약 파생으로 작성하고, `navigation_refs`로 관련 사건·요약·원천을 연결합니다. `derived_from`은 요약의 근거이며 탐색 링크와 구분합니다. `graph_update`의 `entry_context_id`로 대표 요약을 지정하면 `graph_get`과 웹 상세에서 진입할 수 있습니다. 대표 지정 해제는 명시적 `null`, 기존 지정 유지는 필드 생략입니다. 안내에서 `node_get`의 `navigation_refs` 필터로 사건 흐름과 원천을 탐색할 수 있습니다.

기존 DB에는 서버를 정지한 뒤 [운영 마이그레이션 절차](OPERATIONS.md)에 따라 새 `017_conversation_navigation.sql`을 적용해야 합니다. 이번 변경이 기존 운영 DB에 자동 적용되지는 않습니다. 상세 필드와 갱신 책임은 [대화 흐름과 안내 계약](spec/service/agent-context/SRS.md#대화-흐름과-안내-진입점)을 따릅니다.

## 개발 환경 재생성

개발 환경을 처음부터 만들 때는 기존 개발 볼륨을 제거한 뒤 기동합니다.

> [!CAUTION]
> 이 절차는 개발 DB와 WAL 데이터를 삭제합니다. 보존할 데이터가 없는 개발 환경에만 적용하고, 운영 환경에서는 `down -v`를 실행하지 마십시오.

```sh
docker compose -f compose.dev.yaml down -v
docker compose -f compose.dev.yaml up -d --wait
```

재생성 뒤에는 [DB와 서버 기동](#2-db와-서버-기동)의 마이그레이션을 적용합니다. 마이그레이션을 승인된 개발 통합 예외로 `001_init.sql`에 합쳐 checksum이 달라진 경우에도 이전 개발·시험 DB를 재생성합니다. 적용 조건은 [마이그레이션 계약](migrations/AGENTS.md)을 따릅니다.

## 검증

### 기본 검증

DB 통합 검증은 운영 데이터가 없는 전용 개발·시험 DB에서 실행합니다. `.env.example`의 `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED=1`을 내보낸 상태에서 진행하십시오. 서명 키 통합 시험은 임시 DB를 만들므로 시험 계정에 DB 생성 권한이 필요합니다.

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .
sh ./test-dev-compose.sh
sh ./test-prod-compose.sh
```

- `gofmt -l .`은 출력이 없어야 정상입니다.
- `TEST_DATABASE_URL`도 필수 설정도 없으면 DB 통합 시험은 건너뜁니다. 이를 통합 검증 성공으로 간주하지 않습니다.

### 격리 환경 검증

이미지 빌드·컨테이너 기동과 실행 시간이 필요하므로 필요할 때 선택해 실행합니다. 모두 기존 `.env`·DB·볼륨을 사용하지 않고, 자신이 만든 시험 자원만 정리합니다.

| 명령 | 검증 내용 |
|---|---|
| `sh ./test-dev-compose.sh --runtime` | 개발 DB 이미지를 캐시 없이 빌드하고 PostgreSQL 주 판·AGE/pgvector 판·벡터/그래프 SQL·WAL 경로 권한을 확인합니다. 외부 API는 호출하지 않습니다. |
| `sh ./test-wal-archive.sh` | 개발 WAL 보관 회귀 |
| `sh ./test-prod-runtime.sh` | 운영 예시 구성의 격리 기동·진단 볼륨 보존 |
| `GO_BINARY=… sh ./test-pitr.sh` | 물리 백업·tablespace·시점 복구 |
| `GO_BINARY=… sh ./test-transition.sh` | 합성 이전 스키마의 정지·백업·적용·감사와 실패 롤백·별도 볼륨/보관 이미지 복귀. 절차는 [운영 매뉴얼](OPERATIONS.md#7-변경-및-복귀)을 따릅니다. |
| `sh internal/diagnostic/test-linux.sh` | Linux 격리 환경의 진단 기록기 파일·볼륨 인수. `ACS_OBS08_IMAGE`로 설치된 이미지를 지정할 수 있습니다. |

macOS의 진단 기록기 인수는 `go test -race -count=1 -skip '^TestOBS08ContainerPersistence$' ./internal/diagnostic`로 실행합니다. 상세 범위와 제한은 [OBS-08 검토](spec/service/agent-context/reviews/2026-10-23-obs-08.md)를 따릅니다.

### 별도 조건 시험

| 시험 | 조건과 기준 문서 |
|---|---|
| 현재 Codex DCR 초기 연결 | `TEST_DCR_CODEX_BINARY`에 실제 CLI 바이너리를 지정하고 `go test -count=1 -v -run '^TestDCRCodexCLIRegistrationIntegration$' ./cmd/server` 실행. 발견·자동 등록·인가 URL 생성을 검사합니다. [DCR 전환 검토](spec/service/agent-context/reviews/2026-11-02-oauth-dcr.md) |
| Claude Code localhost 초기 연결 | macOS에서 `TEST_CLAUDE_BINARY`에 실제 CLI 바이너리를 지정하고 `go test -count=1 -v -run '^TestLocalhostClaudeCodeRegistrationIntegration$' ./cmd/server` 실행. 임시 PTY·설정으로 발견·자동 등록·인가 URL 생성을 검사합니다. [#300 검토](spec/service/agent-context/reviews/2026-11-02-issue-300.md) |
| 실제 Codex CLI 1시간 접근 만료 | `codex_expiry_live` 태그, `-timeout=75m`, `TEST_CODEX_BINARY`(정확한 CLI 0.160.1)·`TEST_CODEX_REQUIRED=1`. [TEST-08 인수](spec/service/agent-context/reviews/2026-10-27-test-08-completion.md) |
| 조합 장애 회복(`operations_runtime`) | 약 3분의 자연 백오프·리스 만료를 포함하므로 `-timeout=420s`. [OPS-09 실행 범위](spec/service/agent-context/reviews/2026-10-27-ops-09.md) |
| 실제 Gemini 전송 | `embedding_live` 태그. 외부 전송·비용 허용과 키 주입이 필요합니다. 기본 회귀는 로컬 TLS 합성 제공자를 사용합니다. [임베딩 실연결 시험 경계](spec/service/agent-context/sdd/testing.md#임베딩-제공자-실연결-테스트) |
| 수동 브라우저 재검증 | `TEST_DATABASE_REQUIRED=1 WEB_MANUAL_WAIT=15m go test -tags browser_manual -race -count=1 -v -run '^TestTEST06ManualBrowserFixture$' -timeout=20m ./internal/web`. [수동 인수 절차](spec/service/agent-context/reviews/2026-10-28-test-06-manual.md) |

위 시험은 모두 `TEST_DATABASE_URL`·`TEST_DATABASE_REQUIRED=1`을 함께 지정합니다. 수동 브라우저 서버는 로그에 출력된 URL과 비공개 `connection.json` 경로로 접속하며, 최대 15분이 지나거나 같은 디렉터리에 STOP 파일을 만들면 종료합니다. 이 시험의 통과는 TLS 서버·표본 준비만 의미합니다.

### 도구 스냅숏, 감사와 평가

- MCP 도구 계약은 `go test ./internal/mcp -run '^TestOPS08ToolSnapshot$'`로 [도구 스냅숏](internal/mcp/testdata/tools.json)과 대조합니다. 계약 변경을 검토한 경우에만 `UPDATE_TOOL_SNAPSHOT=1`로 갱신하고 diff를 검토합니다.
- 그래프 불변식 감사는 `go run ./cmd/audit`로 실행합니다.
- 검색 품질 평가는 `cmd/eval`이 담당합니다. 표본과 실행 방법은 [테스트 설계](spec/service/agent-context/sdd/testing.md)와 [평가 검증 설계](spec/service/agent-context/sdd/evaluation.md#검증을-가능하게-하는-설계)를 확인하십시오.

## 문서와 저장소 구조

| 경로 | 내용 |
|---|---|
| `cmd/` | 서버·마이그레이션·감사·색인·평가 실행기 |
| `internal/` | 서버 구현 |
| `migrations/` | 스키마 SQL과 [마이그레이션 계약](migrations/AGENTS.md) |
| `spec/` | 요구사항·설계와 프로젝트 용어 |

- [운영 매뉴얼](OPERATIONS.md): 시험 운영 배포, 점검, 백업·복구, 변경과 종료
- [서비스 목록 및 현황](spec/service/README.md): 서비스별 요구사항·설계 진입점
- [서버 요구사항](spec/service/agent-context/SRS.md) / [설계](spec/service/agent-context/SDD.md)
- [용어 정리집](spec/DICTIONARY.md): 프로젝트 용어의 기준
- [개발 설정 예시](.env.example) / [운영 설정 예시](.env.prod.example)
- [작업 계약](AGENTS.md): 변경 시 따라야 할 DOX 계층과 검증 기준

## 검증 현황

초기 개발 계획의 124개 작업은 2026-10-29의 [TRACE-05 최종 문서 점검](spec/service/agent-context/reviews/2026-10-29-trace-05.md)까지 마쳤습니다. 이후 DCR 전용 전환과 Claude Code localhost callback 지원을 반영했습니다. 과거 사전 등록 방식의 호스트 인수 결과를 현재 DCR 전체 지원으로 확대하지 않으며, 현재 작업 상태는 [개발 계획](spec/service/agent-context/DEVELOPMENT_PLAN.md)과 아래 후속 검증 기록을 함께 확인합니다.

| 범위 | 결과 |
|---|---|
| 종합 회귀 | [TEST-09](spec/service/agent-context/reviews/2026-10-28-test-09.md): 필수 DB·race 전체 두 회차를 각각 6,422건(최상위 1,146건) 통과했습니다. 격리 WAL·물리 복구·이행/복귀·Linux 파일·조합 장애 회복도 범위를 나눠 인수했습니다. |
| MCP 도구 | [MCP-09](spec/service/agent-context/reviews/2026-10-24-mcp-09.md): 14종 도구·오류·부분 응답 |
| 현재 DCR 서버 | [DCR 전환 검토](spec/service/agent-context/reviews/2026-11-02-oauth-dcr.md): 등록 저장·동시 총량 한도·계정별 토큰 분리·새 서비스의 검증·갱신과 관련 모듈/DB 시험 |
| 현재 CLI 초기 연결 | Codex CLI 0.165.0과 Claude Code 2.1.316의 발견·DCR·인가 URL 생성 확인. localhost 동의·코드 교환은 별도 TLS/DB 시험으로 확인했습니다. [#300 검토](spec/service/agent-context/reviews/2026-11-02-issue-300.md) |
| 과거 호스트 인수 | [TEST-08](spec/service/agent-context/reviews/2026-10-27-test-08-completion.md): 사전 등록 당시의 macOS Codex CLI 0.160.1 exec·`features.mcp_2026_07_28=true`·파일 자격 저장 구성 |
| 웹 화면 | [TEST-06](spec/service/agent-context/reviews/2026-10-28-test-06-completion.md): 실제 Chrome·자동 키보드·접근성 트리와 사용자 화면 확인 |
| 진단 기록 | [OBS-08](spec/service/agent-context/reviews/2026-10-23-obs-08.md): 여섯 진단 파일과 macOS/Linux 파일·볼륨 보존 |
| 추적 | [TRACE-04](spec/service/agent-context/reviews/2026-10-29-trace-04.md): 요구사항 184건·작업 124개의 근거 연결 |

다음은 아직 인수하지 않았으며 완료 수로 대체하지 않습니다.

- 현재 DCR 구성의 실제 CLI 브라우저 로그인 이후 토큰 저장·도구 호출 전체 인수
- Codex keyring·TUI·앱/IDE, Claude 앱 등 다른 호스트와 판
- 실제 Gemini 품질·청구, 공개 데이터 전체 효과와 외부 과업 효과
- 운영 공개와 실제 Tunnel 인수
- 사용자가 제외한 추가 수동 키보드·실제 VoiceOver 관찰

단계별 명령·표본·실패/수정 내역은 [검토 기록](spec/service/agent-context/reviews/)과 [요청 기록](spec/service/agent-context/logs/prompt.md)에 보존합니다.
