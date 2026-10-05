# 에이전트 컨텍스트 공유 시스템

AI 에이전트가 작업 중 얻은 지식과 맥락을 저장하고, 관계를 연결하여 이후 작업에서 다시 활용하는 AI 에이전트 제텔카스텐입니다. 여러 작업자가 컨텍스트 그래프를 공유하며 계정별 권한 범위 안에서 조회하고 기여할 수 있습니다.

서버 시험 운영은 [운영 매뉴얼](OPERATIONS.md)을 따릅니다. 서비스 동작의 기준은 [서버 요구사항](spec/service/agent-context/SRS.md)과 [서버 설계](spec/service/agent-context/SDD.md)이며, 이 문서는 프로젝트 개요와 시작 방법을 안내합니다.

## 주요 기능

- 원천·파생·사건 컨텍스트의 저장, 조회, 갱신, 폐기와 복구
- 의미 유사도·키워드·시간 필터 검색과 컨텍스트 관계 탐색
- 그래프별 소유자·편집자·열람자 권한과 팀 관리
- 웹 관리 화면의 컨텍스트 조회, 국소 그래프 보기와 운영자 복구
- MCP 도구 13종과 브라우저 기반 인가, PKCE·DPoP를 사용하는 접근 토큰
- 비동기 임베딩 색인, 그래프 불변식 감사와 검색 품질 평가 도구

현재는 검증 단계입니다. 인증된 모든 계정에 운영자 복구 자격이 있으므로 신뢰할 수 있는 시험 참여자로 접근 범위를 제한해야 합니다. 비밀번호 재설정, 계정 잠금, 2FA와 외부 인가 서버 연동은 현재 제공하지 않습니다. 공개 서비스 전환 여부는 [서비스 현황](spec/service/README.md)과 보안 요구사항을 함께 검토하십시오.

## 구성

에이전트 호스트는 로컬 MCP 클라이언트와 `stdio`로 통신하고, 클라이언트는 원격 서버의 `/mcp`에 HTTPS Streamable HTTP 요청을 보냅니다. 서버는 PostgreSQL의 Apache AGE로 그래프를, pgvector로 임베딩을 저장합니다. 임베딩 제공자는 Gemini 또는 Ollama를 명시적으로 선택합니다.

- 서버: 루트 Go 모듈의 `cmd/server`와 `internal/`
- MCP 클라이언트: 독립 Go 모듈인 `client/`
- 개발 환경: 호스트에서 Go 프로그램을 실행하고 `compose.dev.yaml`로 DB를 기동
- 시험 운영 환경: `compose.prod.yaml`로 서버·마이그레이션·DB를 실행하고 Cloudflare DNS 프록시가 TLS를 종단한 뒤 HTTP 원본에 접속. 서버는 Cloudflare의 HTTPS 전달 헤더만 신뢰하며 원본 구간은 암호화되지 않음([운영 제약](OPERATIONS.md#1-운영-전제와-개방-조건) 확인)

## 로컬 시작

다음 명령은 저장소 루트에서 실행합니다. 기존 `.env`가 운영용이라면 별도 개발 체크아웃에서 진행하십시오.

### 준비 사항

- Go **1.27.1**: 루트와 `client/` 모두 같은 버전을 사용합니다.
- Docker와 Docker Compose: 설치된 판이 제공된 Compose 파일의 `config --quiet` 검사를 통과해야 합니다.
- Gemini API 키: 아래 기본 구성에 필요합니다. 본문과 검색 질의가 외부 API로 전송되므로 이용 정책과 비용을 확인하십시오.
- 브라우저와 클라이언트가 신뢰하는 `localhost` TLS 인증서 및 개인 키
- `curl`, 구성 검사에 사용하는 `jq`

기본 셸에 Go가 없다면 IntelliJ IDEA에 등록된 Go 1.27.1 SDK의 통합 터미널이나 실행 구성을 사용합니다.

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

나머지는 `.env.example`의 개발 기본값으로 시작합니다. `RESOURCE_SERVER_URL=https://localhost:8080/mcp`, `AUTHORIZATION_SERVER_URL=https://localhost:8080`, `MCP_ALLOWED_ORIGINS=https://localhost:8080`이 인증서 및 접속 주소와 일치해야 합니다.

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

브라우저에서 `https://localhost:8080/register`로 계정을 생성하고 `/graphs`로 이동합니다. 초기 그래프 생성과 컨텍스트 기여는 MCP 도구로 수행합니다. 웹 화면은 조회·시각화와 권한 관리에 사용합니다.

기본 개발 기동에는 Ollama가 포함되지 않습니다. 새 개발 DB에서 Ollama 구성을 시험하려면 먼저 다음 명령으로 모델을 준비하고, 환경을 `EMBEDDING_PROVIDER=ollama`, `EMBEDDING_BASE_URL=http://localhost:11434`, `EMBEDDING_MODEL=bge-m3`, `EMBEDDING_DIMENSION=1024`로 바꿉니다.

```sh
docker compose -f compose.dev.yaml --profile ollama up -d ollama
docker compose -f compose.dev.yaml --profile ollama exec ollama ollama pull bge-m3
```

기존 DB의 벡터 타입·차원은 환경 변수만 바꿔 전환할 수 없습니다. [서버 설계](spec/service/agent-context/SDD.md)의 「전환·복귀 경계」를 따르십시오.

### 3. MCP 클라이언트 연결

빌드는 별도 모듈에서 실행합니다.

```sh
(cd client && go build -o ../out/agent-context-client ./cmd/client)
./out/agent-context-client doctor
```

`doctor`는 앞서 내보낸 `.env`의 `AGENT_CONTEXT_CLIENT_*` 값을 사용합니다. 브라우저를 열 수 있는 사용자 컴퓨터에서 실행하고 로그인·인가를 완료합니다. 전체 성공의 종료 코드는 `0`이며, `1`은 실패, `2`는 사용법 오류, `3`은 실패 없이 건너뛴 검사가 있는 상태입니다.

MCP 호스트에는 다음 실행 정보와 환경 변수를 등록합니다. 호스트마다 설정 파일 형식은 다르므로 해당 호스트의 MCP 설정 위치에 맞춰 적용하십시오.

```json
{
  "command": "/absolute/path/agent_context_sharing/out/agent-context-client",
  "args": ["serve"],
  "env": {
    "AGENT_CONTEXT_CLIENT_REMOTE_URL": "https://localhost:8080/mcp",
    "AGENT_CONTEXT_CLIENT_ID": "agent-context-dev",
    "AGENT_CONTEXT_CLIENT_AGENT_ID": "0198e7c0-0000-7000-8000-000000000001",
    "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": "5m",
    "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "30s",
    "AGENT_CONTEXT_CLIENT_TOOL_POLICY": "all",
    "AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST": ""
  }
}
```

예시 에이전트 식별자는 로컬 시험용입니다. 실제 사용에서는 에이전트별 고유 UUIDv7을 지정합니다. 클라이언트 ID는 서버의 `OAUTH_CLIENTS`에 등록된 값과 일치해야 합니다. 클라이언트 프로세스는 토큰과 DPoP 키를 메모리에만 보관하므로 재시작하면 재인증이 필요합니다.

원격 서버에 연결하는 참여자는 `./client-setup.sh init <원격 /mcp 주소> [클라이언트 ID]`로 빌드와 구성 파일 생성을, `doctor`와 `host-config`로 진단과 위 실행 정보 출력을 한 번에 처리할 수 있습니다. 절차는 [운영 매뉴얼](OPERATIONS.md)의 「참여자 연결과 개방 확인」을 참고하십시오.

조회 전용 호스트에는 `AGENT_CONTEXT_CLIENT_TOOL_POLICY=read_only`를 지정합니다. 도구 공개 정책은 서버의 그래프별 인가를 대체하지 않습니다. 세부 계약은 [클라이언트 설계](spec/service/agent-context-client/SDD.md)에 있습니다.

## 검증

DB 통합 검증은 운영 데이터가 없는 전용 개발·시험 DB에서 실행합니다. `.env.example`의 `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED=1`을 내보낸 상태에서 진행하십시오. 서명 키 통합 시험은 임시 DB를 만들므로 시험 계정에 DB 생성 권한이 필요합니다.

```sh
go build ./...
go vet ./...
go test ./...
(cd client && go build ./... && go vet ./... && go test ./...)
gofmt -l .
sh ./test-dev-compose.sh
sh ./test-prod-compose.sh
```

`TEST_DATABASE_URL`이 없고 필수 설정도 없으면 DB 통합 시험은 건너뜁니다. 이를 통합 검증 성공으로 간주하지 않습니다. `gofmt -l .`의 정상 결과는 출력이 없는 상태입니다.

별도 격리 환경을 만드는 검증은 다음과 같습니다. 이미지 빌드·컨테이너 기동과 실행 시간이 필요하므로 선택하여 실행합니다. 개발 Compose 실행 검사는 Ollama 모델을 내려받지 않습니다.

```sh
sh ./test-dev-compose.sh --runtime
sh ./test-wal-archive.sh
```

그래프 불변식 감사는 `go run ./cmd/audit`, 검색 품질 평가는 `cmd/eval`이 담당합니다. 평가 표본과 실행 방법은 서버 설계의 「테스트 전략」과 「검증을 가능하게 하는 설계」를 확인하십시오.

## 문서와 저장소 구조

- [운영 매뉴얼](OPERATIONS.md): 시험 운영 배포, 점검, 백업·복구, 변경과 종료
- [서비스 목록 및 현황](spec/service/README.md): 서비스별 요구사항·설계 진입점
- [용어 정리집](spec/DICTIONARY.md): 프로젝트 용어의 기준
- [서버 요구사항](spec/service/agent-context/SRS.md) / [설계](spec/service/agent-context/SDD.md)
- [클라이언트 요구사항](spec/service/agent-context-client/SRS.md) / [설계](spec/service/agent-context-client/SDD.md)
- [개발 설정 예시](.env.example) / [운영 설정 예시](.env.prod.example)
- [마이그레이션 계약](migrations/AGENTS.md): 적용 이력과 임베딩 스키마 전환
- [작업 계약](AGENTS.md): 변경 시 따라야 할 DOX 계층과 검증 기준

`cmd/`에는 서버·마이그레이션·감사·평가 실행기가, `internal/`에는 서버 구현이, `migrations/`에는 스키마가 있습니다. `client/`는 독립 모듈이며 `spec/`은 요구사항·설계와 프로젝트 용어를 소유합니다.
