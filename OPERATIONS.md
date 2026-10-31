# 서버 시험 운영 매뉴얼

이 문서는 `compose.prod.yaml`과 호스트에서 실행하는 Cloudflare Tunnel의 `http://localhost:80` 연결을 사용하는 서버 시험 운영 절차입니다. 서비스 계약은 [서버 설계](spec/service/agent-context/SDD.md), 운영 구성 항목은 [.env.prod.example](.env.prod.example)을 기준으로 합니다. 개발용 서버 profile은 운영 데이터에 사용하지 않습니다.

명령은 별도 표시가 없으면 서버용 체크아웃의 저장소 루트에서 실행합니다. `agent-context.example.com`, 절대 경로, 백업 이름은 실제 값으로 바꾸십시오. 배포·복구 명령은 운영자가 점검한 뒤 실행하는 절차이며, 문서 작성만으로 서버에 적용되지는 않습니다.

## 검증 상태와 이관

[TEST-09](spec/service/agent-context/reviews/2026-10-28-test-09.md)에서 운영 예시 구성의 격리 기동·진단 볼륨 보존, WAL·물리 백업/복구·합성 스키마 전환/복귀와 실제 DB 중단·재시도/리스 만료 후 회복을 인수했습니다. [TRACE-04](spec/service/agent-context/reviews/2026-10-29-trace-04.md)는 이 결과를 코드/서버 계약·선정 호스트·파일·효과·운영별로 연결합니다.

실제 운영 대상의 `.env`·데이터·백업 보존 위치·공개 HTTPS/Tunnel·실제 호스트 등록/자격 전환은 대상별 인수가 남아 있습니다. 운영 담당자는 아래 절차의 대상·담당자·허용 데이터·보존·검증·복귀 조건을 확정하고 실행 결과를 별도 기록합니다. 시험용 합성 자격과 예시 주소를 운영 값으로 사용하지 않습니다.

선정 CLI의 지원 범위는 [TEST-08](spec/service/agent-context/reviews/2026-10-27-test-08-completion.md)의 macOS Codex CLI 0.160.1 exec·기능 활성·파일 자격 저장 구성입니다. 실제 Gemini 품질/청구·공개 데이터 전체 효과·외부 업무 효과와 다른 호스트/저장 모드는 별도 실증 범위로 유지합니다. 이 문서의 절차와 구현 완료 수를 운영 공개의 실행 증거로 사용하지 않습니다.

## 1. 운영 전제와 개방 조건

시험 운영 담당자는 호스트·Docker 관리 권한, Cloudflare 도메인·Tunnel 설정과 OCI 네트워크 관리 권한, Gemini API 이용 권한을 준비합니다. `cloudflared`는 Docker와 같은 호스트에서 별도로 실행합니다. 서버에서 Go를 직접 설치할 필요는 없으며 애플리케이션 이미지는 고정된 Go 1.27.1로 빌드합니다. Docker Compose, `curl`, `jq`가 필요하며 원본 서버 인증서·개인 키는 사용하지 않습니다.

- DB 포트는 호스트에 공개하지 않습니다. 애플리케이션은 IPv4 `127.0.0.1:80`에만 HTTP로 연결합니다. 원본 TCP 80·443을 외부에 공개할 필요가 없습니다.
- 사용자→Cloudflare는 HTTPS, Cloudflare→호스트 커넥터는 암호화된 터널, 커넥터→컨테이너 서버는 로컬 HTTP입니다. 서버는 호스트의 공개 포트 접속 경로인 `10.203.0.1/32`와 단일 `X-Forwarded-Proto: https`를 함께 검증합니다. Cloudflare 공인 대역이나 전체 Docker 대역을 신뢰하지 않습니다.
- **로컬 HTTP 원본 구간은 암호화되지 않습니다.** 비밀번호·토큰·세션 쿠키·본문도 그 구간에서는 평문으로 전송됩니다. 이는 시험 운영 예외이고 전 구간 TLS·토큰 전송 보호 기준을 충족하지 않습니다. 같은 호스트의 다른 프로세스도 전달 헤더를 보낼 수 있으므로 호스트 접근 권한이 신뢰 경계에 포함됩니다. 민감 데이터를 쓰는 일반 운영에는 로컬 원본 TLS를 갖춘 별도 배치가 필요합니다.
- 운영 구성에는 WAL 자동 정리 서비스가 없습니다. 임베딩 제공자는 Gemini입니다.
- 컨텍스트 본문과 검색 질의가 Gemini로 전송됩니다. 기밀 데이터 입력 여부, API 정책·예산·할당량을 먼저 확인합니다.
- 가입한 모든 계정이 운영자 복구 자격을 갖는 검증 단계입니다. 승인된 시험 참여자로 접근을 제한하고 실제 민감 데이터는 사용하지 않습니다. 외부 접근 제어를 추가했다면 현재 제공하는 웹 로그인·관리 화면이 동작하는지 확인합니다.
- 호스트 디스크 장애에 대비하여 물리 백업과 WAL을 다른 호스트 또는 별도 저장소에 보관합니다. 같은 호스트의 Docker 볼륨만으로는 재해 복구를 보장할 수 없습니다.

개방 전에 담당자·연락 경로, 시험 기간, 참여자, 허용 데이터, 최대 허용 데이터 손실 시간과 복구 시간, 백업 보존 기간을 기록합니다. 이 값과 서버 용량은 시험 부하와 복구 리허설의 측정 결과로 정하며, 이 저장소가 운영 보장 수치를 제공하지는 않습니다.

### 공개 로그인·가입 보호 점검

애플리케이션은 [SDD의 공개 로그인·가입 자원 보호](spec/service/agent-context/sdd/authentication.md#공개-로그인가입-자원-보호)에 따라 로그인·가입·MCP 동의 POST를 합쳐 출처별 10회/분으로 제한하며 전체 출처 합계의 분당 한도는 두지 않습니다. 로그인·가입의 비밀번호 작업 동시 4개와 인증 양식 본문 8 KiB 제한은 유지합니다. 초과는 `429`·`Retry-After`·`no-store`이며 계정을 잠그지 않습니다. 출처 상태는 재시작 시 초기화되고 인스턴스별로 적용됩니다. NAT·IPv6 /64 공유 사용자는 출처 예산도 공유하며, 작업 자리 점유·출처 상태 포화는 정상 요청도 일시적으로 거부할 수 있어 분산 공격 방어를 보장하지 않습니다.

현재 신뢰된 호스트 접속 경로와 단일 HTTPS 전달 판정을 통과한 요청에서만 단일 `CF-Connecting-IP`를 사용합니다. 다른 전달 주소 헤더는 사용하지 않고 누락·잘못된 값은 프록시 주소로 합산합니다. 공개 연결에서 정상 주소가 전달되는지, 자체 Worker·Pseudo IPv4·IP 헤더 제거 설정이 어떤지 확인하십시오. 같은 호스트 프로세스가 헤더를 위조할 수 있다는 기존 원본 신뢰 제약은 유지합니다.

저장소 구성만으로는 실제 Cloudflare WAF·Rate Limiting·봇 방어 정책의 적용 여부를 확인할 수 없습니다. 운영 담당자는 공개 경로의 외부 정책과 원본 우회 방지, 정상 참여자의 재시도·오탐률 및 서버 용량을 별도로 점검하고 기록하십시오. 정책 적용을 확인하지 않은 상태에서 애플리케이션 방어의 전제로 가정하지 않습니다. 운영 환경에 공격·CPU 고갈 시험을 실행하지 마십시오.

## 2. 환경 설정

개발용 `.env`와 운영용 `.env`를 같은 체크아웃에서 교체하며 사용하지 않습니다. 기존 설정이 있으면 덮어쓰지 말고 운영용인지 먼저 확인하십시오.

```sh
test -e .env || cp .env.prod.example .env
chmod 600 .env
```

`.env`에서 다음 항목을 확인합니다.

- `POSTGRES_PASSWORD`: 개발 기본값을 사용하지 않습니다. 현재 Compose가 비밀번호를 URL에 그대로 넣으므로 충분히 긴 영문·숫자 조합을 사용합니다.
- `POSTGRES_USER`, `POSTGRES_DB`, `AGE_GRAPH_NAME`: 최초 생성 후 임의로 바꾸지 않습니다. 초기화된 DB의 사용자 비밀번호는 `.env`만 바꿔도 변경되지 않습니다.
- `GEMINI_API_KEY`: 실제 키를 지정합니다. `EMBEDDING_PROVIDER=gemini`, 모델·타입·차원은 우선 운영 예시를 유지합니다.
- `AUTHORIZATION_SERVER_URL=https://agent-context.example.com`
- `MCP_RESOURCE_URL=https://agent-context.example.com/mcp`, `MCP_ALLOWED_ORIGINS=https://agent-context.example.com`: 실제 공개 주소를 사용합니다. 리소스와 issuer는 독립된 HTTPS 식별자입니다.
- `OAUTH_CLIENT_REGISTRATIONS`: 예시의 합성 client ID를 실제 호스트에서 확인한 공개 ID로 바꾸고 callback·resource를 [등록 계약](spec/service/agent-context/sdd/operations.md#목표-추가-구성)에 맞춥니다. 첫 Codex 시험은 고정 `49152` callback과 `loopback_port_policy=exact`를 사용합니다.
- `DIAGNOSTIC_INSTANCE_ID`: 운영 Compose에 필수인 안정적인 슬롯별 고유 ID입니다. `server-1`처럼 재시작에도 같은 값을 유지하고 동시 서버가 같은 슬롯을 공유하지 않게 합니다. Compose는 `DIAGNOSTIC_LOG_DIR=/var/log/agent-context`를 고정하고 `diagnostic_logs` 볼륨을 연결합니다. 수준 기본값은 `INFO`입니다.
- `HTTP_PORT=80`: 커넥터의 원본 URL `http://localhost:80`과 맞춥니다. 기존 설정의 `8080` 또는 `443`을 그대로 두지 않습니다. 포트를 재정의하면 커넥터의 원본 URL도 함께 바꿉니다.

Compose는 `.env`를 읽으며 서버 컨테이너의 `DATABASE_URL`, HTTP 수신 주소와 TLS 배치는 운영 구성에 맞게 덮어씁니다. `TLS_TERMINATION=proxy`, `TRUSTED_PROXY_CIDRS=10.203.0.1/32`를 고정합니다. 서버의 `TLS_CERT_FILE`·`TLS_KEY_FILE`은 빈 값이며 인증서·키를 마운트하지 않습니다. 이전 설정에 이 값이 남아 있어도 서버에서는 사용하지 않습니다. 개발용 `DATABASE_URL`을 복사하여 호스트 DB에 연결하려고 하지 마십시오. 이 절차에서는 `.env`를 셸에 내보낼 필요가 없습니다.

`.env`, 백업과 개인 키는 버전 관리 및 공개 로그에 넣지 않습니다. `docker compose config`의 전체 출력은 비밀번호와 API 키를 포함할 수 있으므로 다음 검사처럼 `--quiet`를 사용합니다.

빈 진단 볼륨은 이미지가 준비한 UID 10001 소유·0700 디렉터리로 초기화합니다. 기록기는 슬롯 디렉터리를 0700, 여섯 파일을 0600으로 준비하고 재생성 후 append합니다. 기존 볼륨은 새 이미지가 덮어쓰거나 권한을 자동 교정하지 않습니다. 권한이 잘못됐거나 같은 슬롯이 이미 잠겨 있으면 DB 연결·요청 수신 전에 기동을 거부하므로 원래 데이터·권한·실행 주체를 확인하십시오. 정상 자료를 삭제하거나 전체 볼륨을 재귀적으로 chown하지 않습니다.

저장소의 `sh test-prod-compose.sh`는 실제 `.env`를 읽거나 컨테이너를 실행하지 않는 정적 검사입니다. `sh test-prod-runtime.sh`는 운영 예시와 합성 자격으로 공개 포트 없는 별도 프로젝트를 만들어 서버 이미지·기동 순서·진단 권한·중복/권한 거부·재생성 보존을 검사하고 시험 자원만 정리합니다. 실제 운영 DB·Cloudflare·외부 제공자를 호출하는 인수와 구분합니다.

### 터널과 원본 접근 준비

1. 호스트의 `cloudflared`와 Tunnel 상태를 확인하고 공개 호스트 이름을 해당 터널로 연결합니다. 원본 서비스 URL은 `http://localhost:80`으로 지정합니다. `HTTP_PORT`를 바꿨다면 동일한 포트를 사용합니다.
2. 브라우저 측 HTTPS 인증서가 활성화되어 있는지 확인하고 공개 클라이언트 URL을 실제 터널 도메인의 HTTPS 주소로 맞춥니다. 이전 DNS 프록시 배치의 `Flexible` 설정을 이 절차의 필수 조건으로 사용하지 않습니다. 커넥터의 HTTP 원본 URL이 로컬 연결 방식을 정합니다.
3. 원본 TCP 80·443의 외부 인바운드 허용은 필요하지 않습니다. 기존 원본 공개 규칙의 제거 여부를 검토하고 터널의 아웃바운드 연결이 가능한지 확인합니다. SSH 관리 경로와 다른 서비스에 필요한 규칙은 보존합니다.

Cloudflare·방화벽·호스트의 커넥터 설정은 자동으로 적용되지 않습니다. [Tunnel 원본 프로토콜 안내](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/protocols/)와 [Docker 포트 공개 안내](https://docs.docker.com/engine/network/port-publishing/)를 확인하십시오. loopback 공개 포트가 같은 L2 네트워크에 노출되는 구버전 제약을 피하려면 Docker Engine 28.0.0 이상을 사용합니다. 호스트의 접근 권한을 제한하고 로컬 신뢰 경로를 다른 용도의 비신뢰 프로세스에 제공하지 않습니다.

```sh
docker compose -f compose.prod.yaml config --quiet
sh ./test-prod-compose.sh
```

정적 검사는 실제 `.env`를 읽거나 이미지 빌드·컨테이너 기동을 하지 않습니다. 방화벽·실제 접속 상대와 Cloudflare 연결은 별도 점검 대상입니다. 운영 예시의 고정 네트워크 `10.203.0.0/24`가 호스트의 기존 네트워크와 충돌하는지도 확인합니다. 이 네트워크 전체가 아니라 호스트 접속 경로의 게이트웨이 `10.203.0.1/32`만 HTTPS 전달 헤더를 신뢰합니다.

## 3. 최초 배포

### 이미지, DB와 스키마

최초 배포는 빈 운영 볼륨을 전제로 합니다. 기존 데이터가 있다면 먼저 「변경 및 복귀」 절차를 따릅니다.

```sh
docker compose -f compose.prod.yaml build db migrate server
docker compose -f compose.prod.yaml up -d --wait db
docker compose -f compose.prod.yaml run --rm --no-deps migrate migrate status
docker compose -f compose.prod.yaml run --rm --no-deps migrate migrate up
docker compose -f compose.prod.yaml run --rm --no-deps server audit
```

각 명령이 성공한 뒤 다음 명령으로 진행합니다. `status`도 마이그레이션 이력 준비와 잠금을 사용하므로 DB가 기동된 상태에서 실행합니다. 마이그레이션 파일과 당시 치환 구성의 이력을 보존하며 적용된 파일의 checksum을 강제로 변경하지 않습니다.

`audit`는 데이터를 자동 수정하지 않는 그래프 불변식 감사입니다. 종료 코드 `0`은 위반 없음, `1`은 위반 발견, `2`는 감사 완료 불가입니다. `1` 또는 `2`이면 개방을 중단하고 규칙 식별자·위치 또는 접속 실패 원인을 확인합니다.

### 서버 기동과 내부 점검

```sh
docker compose -f compose.prod.yaml up -d --wait server
docker compose -f compose.prod.yaml ps -a
docker compose -f compose.prod.yaml logs --tail=100 server migrate db
```

호스트에서 자격 증명 없는 원본 상태 확인 경로를 HTTP로 점검합니다. `HTTP_PORT`를 재정의했다면 아래 포트도 맞춥니다.

```sh
curl --fail --show-error http://127.0.0.1:80/healthz
curl --fail --show-error http://127.0.0.1:80/readyz
```

`server`의 의존 서비스인 `migrate`는 미적용 파일을 확인하고 종료합니다. 성공한 마이그레이션 컨테이너의 `Exited (0)`은 정상입니다. 이미 완료된 적용을 다시 수행하지 않습니다.

`/healthz`는 프로세스 생존, `/readyz`는 종료 중이 아니며 DB 연결이 가능한 상태를 확인합니다. 둘 다 정상 시 HTTP `200`을 반환합니다. Compose의 서버 healthcheck는 `/healthz`만 검사하므로 `healthy`만으로 업무 준비 완료를 판정하지 않습니다. 두 경로는 임베딩 API의 정상 동작이나 색인 완료를 보장하지 않습니다.

### Cloudflare Tunnel 연결

내부 상태 확인을 마친 뒤 호스트의 커넥터와 공개 호스트 이름의 원본 URL `http://localhost:80`을 확인하고 외부의 실제 HTTPS 경로를 점검합니다. 커넥터를 컨테이너 안에서 실행한다면 `localhost`의 의미가 달라지므로 이 배치의 절차를 그대로 사용하지 않습니다.

```sh
curl --fail --show-error https://agent-context.example.com/readyz
curl --fail --show-error --output /dev/null https://agent-context.example.com/login
```

외부 HTTPS의 준비 확인과 로그인 화면은 `200`이 기대 결과입니다. 서버는 실제 접속 주소가 `10.203.0.1`이고 단일 `X-Forwarded-Proto` 값이 `https`일 때만 업무 요청을 처리합니다. [Cloudflare 전달 헤더 안내](https://developers.cloudflare.com/fundamentals/reference/http-headers/)에 따라 이 값은 방문자의 원래 접속 프로토콜이며 방문자 주소인 `CF-Connecting-IP`와 구분합니다. 전달 헤더 없이 로컬 원본의 `/login`에 직접 HTTP로 접속하면 `400 https_required`가 정상입니다. 같은 호스트의 다른 프로세스는 신뢰 경로에서 헤더를 보낼 수 있으므로 호스트 권한을 제한합니다. 외부 검증에서는 TLS 검증을 끄거나 위조 헤더를 정상 연결 검증의 대용으로 사용하지 않습니다.

기존 DNS 프록시 또는 직접 TLS 구성에서 전환한 경우 서버를 재생성해야 합니다. `.env`의 `HTTP_PORT=80`, 호스트 커넥터의 원본 URL과 공개 호스트 이름을 준비한 뒤 `docker compose -f compose.prod.yaml up -d --build --force-recreate --wait server`로 적용합니다. DB 볼륨은 삭제하지 않습니다. 실패하면 설정·포트와 서버가 실제로 받는 접속 상대를 확인하며 전체 CIDR 허용으로 우회하지 않습니다.

## 4. 참여자 연결과 개방 확인

웹 로그인·관리 화면과 본인 연결 목록·폐기, OAuth 발견·인가·코드 교환·갱신, Bearer로 보호하는 `/mcp`와 14종 처리가 구현되어 있습니다. HTTPS `/login`과 시험 계정의 권한 범위를 점검합니다. 실제 호스트 호환성·브라우저 전체·운영 장애 인수는 아직 완료되지 않았으므로 서버 구현만으로 특정 호스트의 연결 지원을 선언하지 않습니다.

[도구 목록 스냅샷](internal/mcp/testdata/tools.json)은 protocol revision·공통 instructions·14종의 설명·입력 스키마·annotations를 담습니다. 등록은 `agent_register`, 읽기는 `graph_list`·`graph_get`·`node_get`·`context_flow_get`·`relation_list`, 쓰기는 `graph_create`·`graph_update`·`node_create`·`node_update`·`node_discard`·`node_restore`·`relation_confirm`·`relation_discard`입니다. 본인 연결 폐기는 웹 `/connections`에서 처리하며 전용 클라이언트·독자 연결 엔드포인트를 추가하지 않습니다. 도구 성공·도메인 실패·부분 결과와 실제 호스트 전체 계약은 MCP-09·TEST-07/08의 인수 범위입니다.

실행기는 `cmd/server`, 별도 색인 `cmd/indexer`, 스키마 `cmd/migrate status|up`, 전체 감사 `cmd/audit`, 평가 `cmd/eval`입니다. 운영 이미지에는 `server`·`migrate`·`audit`만 들어 있으며 `indexer`·`eval`은 호스트에서 각각 `go build -o indexer ./cmd/indexer`, `go build -o eval ./cmd/eval`로 빌드합니다. external 배치의 색인 실행은 [README](README.md#색인-작업자-분리-실행)를 따릅니다. `/metrics`·재색인 공개 CLI·마이그레이션 `down`은 제공하지 않습니다.

## 5. 일상 점검

최소한 시험 시작·종료 시, 배포 직후와 장애 발생 시 점검합니다. 백업 간격과 점검 주기는 합의한 손실 허용 시간에 맞춰 더 짧게 정합니다.

### 서비스와 용량

```sh
docker compose -f compose.prod.yaml ps -a
curl --fail --show-error http://127.0.0.1:80/readyz
curl --fail --show-error https://agent-context.example.com/readyz
docker compose -f compose.prod.yaml logs --since=30m --tail=200 server db
docker stats --no-stream
docker system df
df -h
```

재시작 증가, HTTP 오류, 부분 검색 결과, 색인 실패, 메모리·디스크 증가를 확인합니다. 별도 `/metrics` HTTP 경로는 현재 없습니다. 구조화 로그의 사건·상관 식별자와 처리 시간을 사용하며 로그 수집·회전·보존과 디스크 경보는 호스트 운영에서 설정합니다. 로그를 외부에 전달할 때 자격 증명·본문·요청 헤더가 섞이지 않았는지 확인합니다.

### DB, WAL과 색인 대기

```sh
docker compose -f compose.prod.yaml exec -T db sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
SELECT pg_size_pretty(pg_database_size(current_database())) AS database_size;
SELECT archived_count, failed_count, last_archived_wal, last_archived_time,
       last_failed_wal, last_failed_time FROM pg_stat_archiver;
SELECT state, count(*) AS tasks, min(enqueued_at) AS oldest_enqueued_at,
       min(next_attempt_at) AS next_attempt_at
FROM public.index_task GROUP BY state ORDER BY state;
SQL
docker compose -f compose.prod.yaml exec -T db sh -c 'du -sh "$PGDATA/pg_wal" /var/lib/postgresql/wal_archive; df -h /var/lib/postgresql'
```

`pg_stat_archiver`의 실패 횟수는 누적값이므로 이전 점검과 비교합니다. 마지막 성공 시각은 쓰기가 없으면 오래될 수 있습니다. 실패 증가와 WAL·디스크 사용량을 함께 판단합니다. WAL 보관 실패가 지속되면 디스크가 가득 차 DB가 멈출 수 있습니다. DB 로그에 `같은 이름의 보관본 내용이 다르다`가 남으면 같은 이름의 다른 보관본이 이미 있다는 뜻입니다. 보관본을 임의로 덮어쓰거나 지우지 말고, 이전 보관 명령이 남긴 부분 파일인지 원본 WAL과 대조해 원인을 확인한 뒤 조치합니다.

색인 작업 상태는 `pending`·`failed` 두 가지이며 성공한 작업은 제거됩니다. `pending`에는 지연 재시도와 작업자가 확보한 항목도 포함됩니다. 일시적 실패는 최대 5회, 재시도 간격은 1·2·4·8분이며 차원 불일치 등 재시도 불가 오류는 즉시 실패할 수 있습니다. `failed`는 자동으로 계속 재시도하지 않습니다.

실패 시 제공자 설정·키·할당량·네트워크와 서버 로그를 먼저 확인합니다. 현재 공개된 재색인 CLI나 MCP 도구는 없습니다. 재처리가 필요하면 대상 그래프·컨텍스트·실패 작업을 특정한 유지보수 변경으로 검토합니다. 임의의 전체 큐 삭제, 성공 데이터 삭제나 무제한 반복 호출로 복구하지 않습니다. 모델 변경 시의 기동 재색인과 타입·차원 전환은 서버 설계의 「재색인」·「전환·복귀 경계」를 따릅니다.

### 그래프 감사와 운영자 복구

```sh
docker compose -f compose.prod.yaml run --rm --no-deps server audit
```

마이그레이션·복구 후에는 전체 감사가 필수입니다. 일반 점검도 저부하 시간에 실행하고 감사 결과를 보존합니다. 실패 시 자동 수정하지 않으며, 변경 중인 모델의 재색인이 끝났는지도 함께 확인합니다.

자동 소프트 삭제된 그래프의 복구 요청은 로그인 후 `/operator/restores`에서 확인합니다. 이전 소유자의 요청과 대상 그래프를 대조하고 웹 절차로 처리합니다. 이는 DB의 시점 복구와 다른 기능입니다. 데이터 유실이나 물리 손상은 아래 DB 복구 절차로 처리합니다.

## 6. 백업과 복구

### 백업 원칙

Apache AGE 그래프는 스키마 OID를 참조하므로 `pg_dump`·`pg_restore`만으로 그래프를 정상 복구할 수 없습니다. **기본 물리 백업과 연속 WAL 보관**을 사용합니다. 계정·권한·원본·관계·서명 키를 함께 보존하며 운영 `.env`, 배포 commit, 이미지 ID, Tunnel 공개 호스트 이름·원본 URL, 커넥터 설정·자격 증명과 방화벽 설정도 별도로 보호합니다.

아래 예시는 추가 tablespace가 없는 기본 운영 구성입니다. `EMBEDDING_COLD_TABLESPACE`를 지정했다면 tablespace별 백업·마운트 복구 계획을 먼저 마련하십시오. 백업에는 비밀번호 해시·서명 키·컨텍스트 본문이 들어 있으므로 접근 제한과 외부 저장소 암호화가 필요합니다.

### 기본 물리 백업 예시

백업 디렉터리는 저장소 밖의 실제 보호 경로로 바꿉니다. 부모 디렉터리를 미리 준비하고 예시의 시각 기반 이름을 재사용하거나 기존 백업을 덮어쓰지 않습니다. 아래 `mkdir`가 실패하면 다음 명령을 실행하지 않습니다.

```sh
umask 077
backup_dir=/secure/backups/agent-context/2026-10-05T120000Z
mkdir "$backup_dir"
docker compose -f compose.prod.yaml exec -T --user postgres db sh -c 'pg_basebackup -U "$POSTGRES_USER" -D - -F tar -X fetch --checkpoint=fast' > "$backup_dir/base.tar.partial"
```

명령이 종료 코드 `0`으로 끝난 경우에만 완료 이름으로 바꿉니다. 실패하거나 디스크가 부족했다면 부분 파일을 백업으로 사용하지 않습니다. `-X fetch` 방식은 백업 중 필요한 WAL이 재활용되어 사라지면 실패할 수 있으므로 로그와 종료 코드를 확인합니다.

```sh
mv "$backup_dir/base.tar.partial" "$backup_dir/base.tar"
docker compose -f compose.prod.yaml exec -T db sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT pg_switch_wal();"'
```

WAL 전환 후 「DB, WAL과 색인 대기」의 SQL로 새 보관 성공을 확인한 뒤 보관본을 복사합니다. 전환 직전의 `pg_walfile_name(pg_current_wal_insert_lsn())`을 기록하면 해당 파일의 보관 여부를 대조할 수 있습니다.

```sh
docker compose -f compose.prod.yaml exec -T --user postgres db tar -C /var/lib/postgresql/wal_archive -cf - . > "$backup_dir/wal.tar.partial"
```

WAL 복사도 성공한 경우에만 완료 처리합니다. DB가 새 WAL을 보관하는 중에 복사할 수 있으므로 파일 목록만으로 완결성을 판정하지 않습니다. 외부 보관 자동화에서는 보관 완료된 파일만 전송하고 사본의 크기·무결성을 확인해야 합니다.

```sh
mv "$backup_dir/wal.tar.partial" "$backup_dir/wal.tar"
tar -tf "$backup_dir/base.tar" > /dev/null
tar -tf "$backup_dir/wal.tar" > /dev/null
docker compose -f compose.prod.yaml images
git rev-parse HEAD
```

파일 목록 검사와 종료 코드 확인은 복구 성공을 보장하지 않습니다. 백업 시각·크기·배포 판·WAL 보관 범위를 기록하고 암호화된 외부 저장소로 옮긴 뒤 실제 복구로 검증합니다. 이 수동 WAL 사본은 복사 시점 이후 변경을 보호하지 않습니다. 허용 손실 시간에 맞춰 이후 WAL을 지속적으로 외부에 복사하고, 조용한 DB의 미완료 WAL 구간도 고려합니다.

운영 WAL은 자동 삭제되지 않습니다. 검증된 기본 백업부터 모든 보존 대상 복구 시점까지 필요한 WAL과 timeline history를 확인한 뒤 보존 정책에 따라 정리합니다. 개발의 날짜 기준 `wal_cleanup`을 운영에 복사하거나 운영 `pg_wal` 내부 파일을 수동 삭제하지 않습니다. 자세한 도구 옵션은 [PostgreSQL 18 기본 백업 문서](https://www.postgresql.org/docs/18/app-pgbasebackup.html)를 참조하십시오.

### 추가 tablespace가 있는 백업

`EMBEDDING_COLD_TABLESPACE`를 사용하는 DB에서는 위의 표준 출력 `base.tar` 한 개 방식으로 백업하지 않습니다. `pg_tablespace`의 OID·이름·`pg_tablespace_location(oid)`와 실제 마운트를 먼저 기록하고, 모든 tablespace를 포함할 별도 백업 저장소를 준비합니다. plain 형식에서는 다음과 같이 **각 tablespace마다** 원본 절대 경로를 빈 백업 경로로 매핑합니다. `/secure`는 운영자가 준비한 보호된 마운트의 예시이며 실행 전에 실제 경로로 바꿉니다.

```sh
pg_basebackup -D /secure/backup/base --format=plain --wal-method=stream \
  --checkpoint=fast \
  --tablespace-mapping=/var/lib/postgresql/cold=/secure/backup/cold
pg_verifybackup /secure/backup/base
```

명령은 DB와 같은 PostgreSQL 18 도구로 인증이 준비된 환경에서 실행합니다. `-X stream`은 기본 백업과 WAL 스트림에 두 개의 복제 연결을 사용합니다. 기본 디렉터리만 복사하면 cold 데이터가 빠지므로 매핑된 모든 경로·manifest·연속 WAL·timeline history를 함께 외부에 보관합니다. 복원 대상의 `pg_tblspc/<OID>` 링크는 원래의 절대 경로를 그대로 믿지 않고, 별도 복원 볼륨의 실제 tablespace 마운트를 가리키는지 확인합니다. `pg_verifybackup` 성공 후에도 아래 실제 복구를 수행해야 합니다.

### 격리 복구 시험 실행

저장소 루트에서 다음 모듈 시험을 실행할 수 있습니다. Docker 엔진·Compose와 Go 1.27.1이 필요하며 SDK가 PATH에 없으면 `GO_BINARY`에 지정합니다.

```sh
sh test-wal-archive.sh
GO_BINARY=/path/to/go1.27.1/bin/go sh test-pitr.sh
```

두 실행기는 개발 `.env`와 기존 DB를 사용하지 않고 매번 새 프로젝트를 만듭니다. `test-pitr.sh`는 기본 데이터·별도 cold tablespace·WAL·백업과 복구본을 전용 볼륨에 만들고, 고정된 DB 이미지에서 물리 백업·manifest/손상 거부·이름 있는 복구 지점·목표 일시 정지·승격을 검사합니다. 원본과 복구본의 목표 전후 데이터, AGE·양쪽 HNSW·현재 모델·서명·큐·전체 감사를 대조한 뒤 이 실행의 합성 자원만 정리합니다. 외부 제공자 호출과 호스트 포트 공개는 없습니다. 실제 운영 백업이나 보관본 삭제를 수행하는 명령은 아닙니다.

### 격리 복구 리허설과 장애 복구

첫 개방 전, 이후 중요한 스키마 변경 후와 백업 정책에 정한 주기로 리허설을 수행합니다. 원래 운영 볼륨을 덮어쓰지 않습니다. 다음은 운영자가 DB 복구 환경을 준비하여 실행할 점검 순서입니다.

1. 사용할 기본 백업, 목표 복구 시각, 연속 WAL·timeline history, 배포 commit과 동일 DB 이미지를 확보합니다. 장애 시 원래 DB 데이터와 보관되지 않은 WAL도 별도로 보존합니다.
2. 리허설은 별도 호스트 또는 별도 Compose 프로젝트·볼륨·네트워크에서 수행합니다. `compose.prod.yaml`의 고정 subnet은 기존 프로젝트와 충돌할 수 있으므로 격리 환경에서 조정합니다. 서버 공개 포트를 연결하지 않고 `db`만 준비합니다.
3. **DB가 정지된 새 빈 볼륨**에 기본 백업을 풀어 실제 `PGDATA` 경로를 재현합니다. 운영 마운트 루트는 `/var/lib/postgresql`이며 그 자체가 항상 `PGDATA`인 것은 아닙니다. 정상 DB에서 `SHOW data_directory` 또는 컨테이너의 `PGDATA`를 확인하여 기록합니다.
4. 보관 WAL은 새 `wal_archive` 볼륨의 `/var/lib/postgresql/wal_archive`에 풀고 파일 소유자·권한을 DB 이미지의 `postgres` 계정에 맞춥니다. 추가 tablespace가 있으면 원래 위치·링크·마운트를 모두 복원합니다.
5. 시점 복구가 필요하면 새 데이터 디렉터리의 `postgresql.auto.conf`에 아래 설정을 추가하고 같은 디렉터리에 빈 `recovery.signal` 파일을 만듭니다. 예시 시각은 실제 목표로 바꿉니다. 일반 애플리케이션은 아직 연결하지 않습니다.

```conf
restore_command = 'cp /var/lib/postgresql/wal_archive/%f %p'
recovery_target_time = '2026-10-05 12:30:00+00'
recovery_target_action = 'pause'
```

6. 동일 DB 이미지와 AGE 기동 설정으로 DB를 시작하고 로그에서 복구 목표 도달을 확인합니다. `pg_isready` 성공만으로 목표 도달을 판단하지 않습니다. 필요한 WAL이 없거나 목표에 도달하지 못하면 중단하고 다른 백업·WAL을 확보합니다.
7. `SELECT pg_is_wal_replay_paused();`와 목표 데이터의 존재·삭제 상태를 확인합니다. 목표를 확인한 운영자만 `SELECT pg_wal_replay_resume();`로 복구를 완료하고 `SELECT pg_is_in_recovery();`가 `false`인지 확인합니다. 기본 백업 완료 시점보다 앞선 목표는 그 백업으로 복구할 수 없습니다. 목표 설정과 일시 정지 동작은 [PostgreSQL 복구 목표 설정](https://www.postgresql.org/docs/18/runtime-config-wal.html#RUNTIME-CONFIG-WAL-RECOVERY-TARGET)을 참조하십시오.
8. 복원한 스키마와 일치하는 애플리케이션 판으로 전체 `audit`를 실행합니다. 격리 Compose에서 `run --rm --no-deps server audit`가 **복원 DB만** 가리키는지 확인합니다. 아직 새 판의 `migrate up`을 자동 실행하지 않습니다.
9. 감사 종료 코드 `0`, 그래프 조회, 권한 검사와 의미·키워드 검색을 확인합니다. 리허설에서 서버를 켜면 주기 작업과 외부 임베딩 요청이 실행될 수 있으므로 시험 계정·데이터로 제한하고 외부 전송을 통제합니다.
10. 실제 장애 복구에서는 최종 복구 승인 후에만 새 DB로 배치를 전환하고 원본 접속을 다시 허용합니다. `/readyz`, 외부 로그인과 웹 관리 화면을 확인합니다. 복구 후 새 기본 백업을 받아 새 timeline의 보관을 시작하고 실제 손실 범위·복구 시간을 기록합니다.

리허설용 복사본의 회수·삭제는 볼륨 이름과 보존 대상이 확인된 후 별도 승인된 정리로 수행합니다. 서명 키가 유실되거나 이전 상태로 돌아오면 기존 토큰·웹 세션이 무효가 될 수 있으므로 참여자에게 재인증을 안내합니다. 그래프 감사가 실패하면 개방하지 않습니다. 시점 복구의 상세 설정과 WAL 처리 기준은 [PostgreSQL 18 복구 문서](https://www.postgresql.org/docs/18/continuous-archiving.html)를 따릅니다.

## 7. 변경 및 복귀

변경 창을 알리고 새 판의 요구사항·마이그레이션·임베딩 구성 변화와 현재 스키마의 호환성을 검토합니다. 먼저 아래 격리 리허설을 통과하고 실제 실행 대상·백업·복귀 승인 조건을 검토합니다. 운영 체크아웃에서 자동 `pull`을 전제로 하지 않으며 승인된 판을 명시적으로 선택합니다.

### 실행 전 검토 자료

- 대상 호스트·Compose 프로젝트·DB/진단/WAL/tablespace 볼륨과 현재/목표 commit·DB 확장 판을 기록합니다. 이전 애플리케이션의 이미지 ID와 보관 태그 또는 `docker image save` 사본을 확보한 뒤 새 이미지를 빌드합니다. 같은 태그를 덮어쓴 뒤 이전 이미지가 남아 있으리라 가정하지 않습니다.
- 보호된 환경 설정 사본과 실제 키의 존재를 확인하고 기록에는 구성 항목·비밀을 제외한 변경점만 남깁니다. 공개 resource·issuer·Origin·호스트 callback, 안정 진단 슬롯·볼륨 권한과 embedded/external 배치를 대조합니다.
- 적용 파일 번호·checksum·render_config와 현재/목표 벡터 형상을 보존합니다. 현재 배포 SQL은 001→009→010→011→012→013→014입니다. `migrate status`는 자문 잠금 없이 읽기 전용 트랜잭션에서 조회하며 이력 테이블·구성을 바꾸지 않습니다. 구형 이력의 구성 보충은 `migrate up`만 수행합니다.
- 모든 서버·외부 색인 작업자의 정지 방법, 30/10/5초 정리·50초 컨테이너 유예와 검증·복귀 담당자를 지정합니다. 정지 후 물리 백업·필요 WAL·tablespace·manifest, 복구 표본과 손실 승인 조건을 확보합니다. 진단 파일은 DB 백업과 별도로 보존합니다.
- 전체 감사 0·반복 적용 무변경·원본/권한/키/등록/완료 결과 대조·`/readyz`·대상 기능 인수의 기대 결과와 중단 조건을 적습니다. `/healthz`·Compose healthy·스냅샷 일치만으로 서비스 개방을 승인하지 않습니다.

### 정지부터 인수까지

외부 입력을 중단하고 모든 서버·별도 색인 작업자를 정상 종료합니다. 아래 명령은 현재 Compose의 서버 한 서비스만 멈추므로 다른 인스턴스·호스트 실행기에도 정지 절차를 적용합니다.

```sh
docker compose -f compose.prod.yaml stop server
```

쓰기 중단을 확인한 뒤 [물리 백업](#6-백업과-복구)과 이미지·설정·적용 이력 보존을 완료합니다. DB는 백업을 위해 실행 상태를 유지합니다. 그 뒤 준비한 목표 판에서 다음을 한 단계씩 실행합니다. 감사가 성공하기 전에는 서버·작업자를 재기동하지 않습니다.

```sh
docker compose -f compose.prod.yaml build db migrate server
docker compose -f compose.prod.yaml run --rm --no-deps migrate migrate status
docker compose -f compose.prod.yaml run --rm --no-deps migrate migrate up
docker compose -f compose.prod.yaml run --rm --no-deps server audit
docker compose -f compose.prod.yaml up -d --no-deps --force-recreate --wait server
curl --fail --show-error http://127.0.0.1:80/readyz
```

위 예시는 **DB 이미지가 바뀌지 않는 애플리케이션 변경**입니다. `build db`는 실행 중인 DB를 자동 교체하지 않습니다. PostgreSQL·AGE·pgvector 버전이나 DB 이미지를 바꾸는 작업은 별도 검증과 업그레이드 계획이 필요합니다. 각 단계가 성공한 경우에만 진행하고 새 서버의 실제 이미지 ID를 검토 자료와 대조합니다. 외부 입력을 열기 전에 내부·외부 준비 확인, 로그인·권한과 대상 기능 인수를 반복합니다. 별도 색인 작업자는 설정·비용·대상 인수가 허용된 시점에 다시 시작합니다.

### 실패 경계와 복귀

`migrate up`은 파일마다 하나의 트랜잭션을 사용합니다. 실패 파일의 DDL·데이터·적용 이력은 롤백되지만 앞서 성공한 파일은 남습니다. 실패 파일이 롤백됐다는 이유만으로 배포 전체가 이전 상태라고 판단하지 않습니다. 적용 이력·업무 상태를 확인하고 원인을 해결하거나 검증된 복귀를 완료하기 전까지 서버·작업자를 정지합니다.

이미 적용·사용한 스키마와 데이터를 이전 상태로 돌릴 때는 이미지 교체나 이력 삭제로 되돌리지 않습니다. 이전 백업을 별도 빈 DB 볼륨에 복원하고 보관한 동일 DB·애플리케이션 이미지와 이전 설정으로 전체 감사를 실행합니다. 감사·원본/권한/키/기존 세션·적용 이력·준비 확인 후 손실 범위를 승인받아 새 DB로 전환합니다. 제거한 전용 클라이언트 경로를 복귀 경로로 다시 만들지 않습니다.

복귀 실행은 복원 DB를 가리키는 별도 Compose override에 `server.image`를 보관 이미지 ID/태그로, `server.environment.DATABASE_URL`을 복원 DB로 지정합니다. 감사는 `run --rm --no-deps server audit`, 기동은 `up -d --no-deps --force-recreate --wait server`로 수행하여 `migrate` 의존 서비스가 새 스키마를 자동 적용하지 않게 합니다. 전체 구성 출력에 비밀을 남기지 말고 대상 DB·이미지 ID만 대조합니다. 원본 볼륨은 조사·재시도용으로 보존합니다.

### 격리 전환·복귀 리허설

```sh
GO_BINARY=/path/to/go1.27.1/bin/go sh test-transition.sh
```

실행기는 실제 `.env`를 읽지 않고 예시 구성·합성 자격·별도 프로젝트·빈 볼륨을 사용합니다. 호스트 포트 공개와 외부 네트워크를 차단하고 색인은 external로 둡니다. 현재 소스에서 전환 대상 013 이후의 번호를 모두 제외해 만든 합성 이전 이미지는 과거 출시 이미지의 호환성을 보장하는 자료가 아닙니다. 012 기동→정상 정지→물리 백업/manifest 검증→013부터 현재 최신 번호까지 적용/반복 무변경→전체 감사→새 이미지 준비 확인→013을 사용하는 파생 생성/감사와 실패 파일 롤백을 확인합니다. 실패 주입 파일은 현재 최신 번호 뒤의 두 번호를 사용합니다. 이후 별도 볼륨에 백업을 복원하고 자동 적용 없이 보관한 합성 이전 이미지로 감사·이전 세션/권한/원본·준비를 확인합니다. 시험이 만든 자원만 정리하며 실제 운영 배포·Tunnel·외부 제공자·실제 호스트 인수와 구분합니다.

벡터 타입·차원을 변경하려면 다음 번호의 마이그레이션과 재색인 계획이 필요합니다. 적용된 `001_init.sql` 수정, checksum 덮어쓰기나 `.env`만 변경하는 전환은 하지 않습니다. 같은 차원의 모델 변경도 재색인 중 검색 품질과 외부 API 사용량을 확인합니다.

### 벡터 형상 변경과 데이터 보존

`012_embedding_configuration.sql`은 처음 적용할 때 현재 구성과 DB 열을 비교합니다. 타입·차원이 같으면 기존 벡터·작업·내용 판을 보존하고, 다르면 활성 대상 작업 등록·호환되지 않는 벡터 제거·열 변경·양쪽 HNSW 재생성을 한 트랜잭션에서 수행합니다. 원본 컨텍스트·사건·관계·감사와 기존 OAuth·등록·멱등성 결과는 보존합니다. 실제 API 호출은 마이그레이션에 포함되지 않습니다.

`013_context_revision.sql`은 파생의 본문·참조·판 연결을 보존하는 이력 테이블을 추가합니다. 서버·색인 작업자를 정지하고 백업한 뒤 같은 `migrate up` 절차로 적용합니다. 기존 데이터의 알 수 없는 과거 판은 복원하지 않으며 첫 변경·대체 시 관찰한 판부터 보존합니다. [파생 판 이력](spec/service/agent-context/sdd/data.md#파생-판-이력)의 보존·복귀 경계를 따릅니다.

1. 모든 서버 인스턴스와 별도 `cmd/indexer` 작업자를 정지합니다. 배포 판·적용 이력·이전/목표 모델·벡터 타입·차원·대상 계층·콜드 tablespace를 기록하고 복구가 검증된 백업을 확보합니다. API 키·접속 비밀번호는 작업 기록에 넣지 않습니다.
2. 같은 이미지·확장 판의 격리 복원본에서 먼저 `migrate status` → `migrate up` → `migrate up`을 실행합니다. 두 번째 적용은 무변경이어야 합니다. 구형 이력은 DB 형상으로 복원한 치환 SQL의 checksum이 일치할 때만 구성을 보충합니다. 불일치 시 중단하고 원래 SQL·구성을 확인하며 checksum을 덮어쓰지 않습니다.
3. 승인된 운영 대상에서 위 변경 명령을 순서대로 실행합니다. 각 파일은 별도 트랜잭션이므로 실패한 파일만 롤백되며 앞서 성공한 파일은 남습니다. 실패 후 적용 이력·열 타입·큐·임베딩·HNSW를 확인하고 원인이 해결되기 전에는 서버·작업자를 재기동하지 않습니다.
4. 적용 후 전체 불변식 감사와 원본·감사·계정·OAuth·등록·멱등성 결과 및 최초 만료 보존을 대조합니다. 두 파티션의 영속성·HNSW·tablespace를 확인합니다. tablespace 설정만 바꿔도 기존 파티션이 자동 이동하지 않으므로 물리 이동은 별도 새 번호 SQL과 복구 검증으로 수행합니다.
5. 외부 전송·비용이 허용된 조건에서 목표 구성으로 재색인을 시작합니다. 옛 모델 행 수와 함께 현재 모델 누락·pending·failed를 확인합니다. 옛 모델 행이 0이어도 준비 완료를 뜻하지 않습니다. 실제 호스트 연결·쓰기·갱신 인수는 별도로 수행합니다.
6. 이미 적용한 012 이후의 추가 형상 변경이나 원래 타입·차원으로 복귀할 때는 같은 전환 절차를 다음 번호 파일로 추가합니다. 과거 번호·checksum·이력을 재사용하지 않습니다. 원본을 보존한 형상 복귀에도 벡터 재생성이 필요합니다. 적용 후 사용한 업무 상태까지 되돌려야 하면 아래 물리 백업·WAL 복구 절차를 따릅니다.

스키마가 호환되면 보호해 둔 이전 이미지·설정으로 서버를 되돌릴 수 있습니다. 마이그레이션에는 `down` 명령이 없으며, 스키마가 호환되지 않으면 이미지 교체만으로 복귀하지 않습니다. 백업·WAL을 새 볼륨에 복구하고 손실 범위 승인을 받은 뒤 배치를 전환합니다. 빌드가 같은 이미지 태그를 갱신하므로 이전 판은 변경 전에 별도 태그 또는 저장한 이미지로 확보하십시오.

## 8. 장애 대응

- **컨테이너는 healthy인데 업무 요청 실패:** `/readyz`, 외부 `/login`과 색인 상태를 따로 확인합니다. DB·TLS·웹 인증·임베딩 경계를 구분합니다.
- **외부 접속 실패:** Tunnel·호스트 커넥터 상태, 공개 호스트 이름의 원본 URL, 로컬 `/readyz`와 네트워크를 확인합니다. `https_required`가 나오면 서버의 `TLS_TERMINATION=proxy`, 실제 접속 상대 `10.203.0.1`과 단일 `X-Forwarded-Proto: https` 전달을 확인합니다. Docker Desktop 또는 중간 프록시가 접속 주소를 바꿨다면 배치·신뢰 경로를 함께 조사하고 전체 CIDR 허용으로 우회하지 않습니다. 외부 인증서 오류는 Cloudflare의 방문자 측 인증서·도메인을 대조합니다.
- **`migrate` 실패:** DB 준비 상태, 기존 적용 파일의 checksum과 치환 구성, 권한·확장 버전을 확인합니다. 적용 이력을 삭제하지 않습니다.
- **서버 시작 시 임베딩 스키마 불일치:** 실행 구성과 DB의 벡터 타입·차원을 대조합니다. 기존 DB의 변경은 정식 마이그레이션으로 수행합니다.
- **의미 검색 누락·부분 검색:** Gemini 키·할당량·네트워크와 큐를 확인합니다. 미색인 컨텍스트는 의미 유사도 채널에서 빠질 수 있습니다. 원본 조회·키워드 검색과 구분하여 안내합니다.
- **WAL 보관 실패·디스크 부족:** 외부 입력을 제한하고 보관 경로 권한·여유 공간·실패 시각을 확인합니다. 검증된 백업과 필요한 WAL을 먼저 보호하고 `pg_wal`을 직접 삭제하지 않습니다.
- **감사 위반·데이터 손상:** 개방 또는 변경 완료 판정을 중단하고 백업·현재 상태를 보존합니다. 자동 수정이나 광범위한 SQL 삭제 대신 위반 규칙과 대상에 한정한 수정·복구를 검토합니다.

사고 기록에는 발생 시각, 영향 범위, 마지막 정상 확인, 배포 판, 안전한 로그와 조치·검증 결과만 남깁니다. 본문·토큰·API 키·비밀번호를 포함하지 않습니다.

## 9. 시험 종료

참여자에게 종료 시각과 데이터 보존·반출 정책을 알립니다. 해당 공개 호스트 이름의 터널 경로를 비활성화한 뒤 서버를 정상 종료하여 요청과 주기 작업의 추가 쓰기를 멈춥니다. 다른 서비스도 사용하는 커넥터를 함께 중지하거나 평문 원본을 직접 노출하는 방식으로 종료하지 않습니다.

```sh
docker compose -f compose.prod.yaml stop server
```

DB가 실행 중인 상태에서 「백업과 복구」의 최종 물리 백업, WAL 전환·보관과 외부 복사를 완료한 뒤 DB를 종료합니다.

```sh
docker compose -f compose.prod.yaml stop db
```

서버는 요청에 최대 30초, 작업자 종료에 독립된 10초, 진단 flush에 최대 5초를 기다리며 Compose 종료 유예는 50초입니다. 정상 종료를 기다리고 불필요한 강제 종료를 피합니다. 컨테이너·네트워크만 제거하려면 백업 확인 후 `docker compose -f compose.prod.yaml down`을 사용할 수 있으며 명명된 볼륨은 남습니다.

**운영 환경에서 `down -v`를 실행하지 마십시오.** 개발 초기화용 명령이며 운영 DB와 WAL 볼륨을 삭제합니다. 데이터·볼륨·백업의 영구 삭제와 API 키 폐기는 보존 기간과 대상이 확인된 별도 승인 절차로 수행합니다.
