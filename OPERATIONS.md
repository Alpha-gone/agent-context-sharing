# 서버 시험 운영 매뉴얼

이 문서는 Oracle Cloud 단일 호스트에서 `compose.prod.yaml`과 Cloudflare DNS 프록시를 사용하는 시험 운영 절차입니다. Tunnel은 사용하지 않습니다. 서비스 계약은 [서버 설계](spec/service/agent-context/SDD.md), 구성 항목은 [.env.prod.example](.env.prod.example)을 기준으로 합니다. 로컬 개발은 [README](README.md)를 따릅니다.

명령은 별도 표시가 없으면 서버용 체크아웃의 저장소 루트에서 실행합니다. `agent-context.example.com`, 절대 경로, 백업 이름은 실제 값으로 바꾸십시오. 배포·복구 명령은 운영자가 점검한 뒤 실행하는 절차이며, 문서 작성만으로 서버에 적용되지는 않습니다.

## 1. 운영 전제와 개방 조건

시험 운영 담당자는 호스트·Docker 관리 권한, Cloudflare 도메인·SSL 설정과 OCI 네트워크 관리 권한, Gemini API 이용 권한을 준비합니다. 서버에서 Go를 직접 설치할 필요는 없으며 애플리케이션 이미지는 고정된 Go 1.27.1로 빌드합니다. Docker Compose, `curl`, `jq`가 필요하며 원본 서버 인증서·개인 키는 사용하지 않습니다.

- DB 포트는 호스트에 공개하지 않습니다. 애플리케이션은 IPv4 `0.0.0.0:80`에 HTTP로 공개합니다. 원본으로 들어오는 TCP 80은 OCI와 호스트 방화벽에서 Cloudflare 접속 대역으로 제한합니다.
- 사용자→Cloudflare는 HTTPS, Cloudflare→원본 서버는 HTTP인 `Flexible` 모드입니다. 서버는 실제 접속자가 Cloudflare 대역이고 `X-Forwarded-Proto: https`가 전달된 업무 요청만 처리합니다. 전체 네트워크를 신뢰하지 않습니다.
- **원본 구간은 암호화되지 않습니다.** 비밀번호·토큰·세션 쿠키·본문도 그 구간에서는 평문으로 전송되며 IP 접근 제한으로 암호화를 대체할 수 없습니다. 이는 사용자가 명시한 시험 운영 예외이고 전 구간 TLS·토큰 전송 보호 기준을 충족하지 않습니다. 민감 데이터를 쓰는 일반 운영에는 원본 TLS를 갖춘 별도 배치가 필요합니다.
- 운영 구성에는 Ollama와 WAL 자동 정리 서비스가 없습니다. 기본 임베딩 구성은 Gemini입니다.
- 컨텍스트 본문과 검색 질의가 Gemini로 전송됩니다. 기밀 데이터 입력 여부, API 정책·예산·할당량을 먼저 확인합니다.
- 가입한 모든 계정이 운영자 복구 자격을 갖는 검증 단계입니다. 승인된 시험 참여자로 접근을 제한하고 실제 민감 데이터는 사용하지 않습니다. 외부 접근 제어를 추가했다면 웹 로그인과 MCP 인가·요청 경로가 모두 동작하는지 확인합니다.
- 호스트 디스크 장애에 대비하여 물리 백업과 WAL을 다른 호스트 또는 별도 저장소에 보관합니다. 같은 호스트의 Docker 볼륨만으로는 재해 복구를 보장할 수 없습니다.

개방 전에 담당자·연락 경로, 시험 기간, 참여자, 허용 데이터, 최대 허용 데이터 손실 시간과 복구 시간, 백업 보존 기간을 기록합니다. 이 값과 서버 용량은 시험 부하와 복구 리허설의 측정 결과로 정하며, 이 저장소가 운영 보장 수치를 제공하지는 않습니다.

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
- `RESOURCE_SERVER_URL=https://agent-context.example.com/mcp`
- `AUTHORIZATION_SERVER_URL=https://agent-context.example.com`
- `MCP_ALLOWED_ORIGINS=https://agent-context.example.com`
- `OAUTH_CLIENTS`: 실제 클라이언트 ID와 허용할 리디렉션 URI를 등록합니다. 예시는 `agent-context`와 IPv4·IPv6 루프백 callback을 등록하며 동적 루프백 포트를 허용합니다.
- `HTTP_PORT=80`: 기존 설정의 `8080` 또는 `443`을 그대로 두지 않습니다. Flexible의 일반 HTTPS 접속은 원본 HTTP 80으로 전달됩니다. 별도 origin rule이 있다면 그 설정과 실제 수신 포트를 대조합니다.

Compose는 `.env`를 읽으며 서버 컨테이너의 `DATABASE_URL`, HTTP 수신 주소와 TLS 배치는 운영 구성에 맞게 덮어씁니다. `TLS_TERMINATION=proxy`, `TRUSTED_PROXY_CIDRS`는 공식 Cloudflare 접속 대역으로 고정합니다. 서버의 `TLS_CERT_FILE`·`TLS_KEY_FILE`은 빈 값이며 인증서·키를 마운트하지 않습니다. 이전 설정에 이 값이 남아 있어도 서버에서는 사용하지 않습니다. 개발용 `DATABASE_URL`을 복사하여 호스트 DB에 연결하려고 하지 마십시오. 이 절차에서는 `.env`를 셸에 내보낼 필요가 없습니다.

`.env`, 백업과 개인 키는 버전 관리 및 공개 로그에 넣지 않습니다. `docker compose config`의 전체 출력은 비밀번호와 API 키를 포함할 수 있으므로 다음 검사처럼 `--quiet`를 사용합니다.

### DNS와 원본 접근 준비

1. Cloudflare DNS의 프록시 활성화된 A 레코드가 Oracle 호스트의 공인 IPv4를 가리키는지 확인합니다. IPv6 원본 수신을 준비하지 않았다면 원본을 가리키는 AAAA 레코드는 사용하지 않습니다.
2. Cloudflare SSL/TLS 모드는 `Flexible`로 지정합니다. 브라우저 측 HTTPS 인증서가 활성화되어 있는지 확인하고 공개 클라이언트 URL은 HTTPS를 유지합니다. 원본에서 HTTP→HTTPS 리디렉션을 추가하지 않습니다.
3. OCI의 유효한 NSG·보안 목록과 호스트의 컨테이너 유입 방화벽에서 TCP 80을 Cloudflare의 [현재 원본 접속 IP 대역](https://www.cloudflare.com/ips/)에만 허용하고 다른 출처의 원본 접속은 차단합니다. 기존 `0.0.0.0/0` 허용 규칙이 남아 있지 않은지도 확인합니다. Docker 공개 포트는 일반 호스트 INPUT 규칙만으로 제한되지 않을 수 있으므로 실제 컨테이너 유입 경계와 OCI 규칙을 검증합니다. SSH 관리 경로는 별도로 보존합니다.

Cloudflare 대역 허용은 원본 우회 노출을 줄이지만 특정 Cloudflare 계정을 인증하지는 않습니다. 방화벽 규칙은 자동으로 적용되지 않습니다. [Flexible 안내](https://developers.cloudflare.com/ssl/origin-configuration/ssl-modes/flexible/), [원본 접근 제한 안내](https://developers.cloudflare.com/fundamentals/concepts/cloudflare-ip-addresses/)와 [Docker 방화벽 경계](https://docs.docker.com/engine/network/packet-filtering-firewalls/)를 확인하십시오. Compose의 신뢰 대역은 2026-10-05 공식 IPv4·IPv6 목록과 대조했으며, 공식 목록이 바뀌면 구성·검사 기대값·방화벽을 함께 갱신합니다.

```sh
docker compose -f compose.prod.yaml config --quiet
sh ./test-prod-compose.sh
```

정적 검사는 실제 `.env`를 읽거나 이미지 빌드·컨테이너 기동을 하지 않습니다. 방화벽·실제 접속 출발지와 Cloudflare 연결은 별도 점검 대상입니다. 운영 예시의 고정 네트워크 `10.203.0.0/24`가 호스트의 기존 네트워크와 충돌하는지도 확인합니다. 이 내부 네트워크는 HTTPS 전달 헤더의 신뢰 대역이 아닙니다.

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

### Cloudflare DNS 프록시 연결

원본 준비와 `Flexible` 설정을 마친 뒤 외부의 실제 HTTPS 경로를 확인합니다. 기본 원본은 HTTP 80이며 별도 origin rule이 포트나 호스트를 덮어쓰고 있지 않은지도 확인합니다. 클라이언트의 HTTP 접속을 HTTPS로 전환하려면 Cloudflare의 HTTPS 리디렉션을 사용합니다. 원본에서 HTTPS로 돌려보내면 Flexible에서 반복 리디렉션이 발생할 수 있습니다.

```sh
curl --fail --show-error https://agent-context.example.com/readyz
curl --fail --show-error --output /dev/null https://agent-context.example.com/login
```

외부 HTTPS의 준비 확인과 로그인 화면은 `200`이 기대 결과입니다. 서버는 실제 접속 주소가 Cloudflare 대역이고 단일 `X-Forwarded-Proto` 값이 `https`일 때만 업무 요청을 처리합니다. [Cloudflare 전달 헤더 안내](https://developers.cloudflare.com/fundamentals/reference/http-headers/)에 따라 이 값은 방문자의 원래 접속 프로토콜이며 방문자 주소인 `CF-Connecting-IP`와 구분합니다. 로컬 원본의 `/login`에 직접 HTTP로 접속하면 `400 https_required`가 정상입니다. 로컬 요청에 전달 헤더를 임의로 붙여도 신뢰 대역 밖이면 거부되어야 합니다. 외부 검증에서는 TLS 검증을 끄거나 위조 헤더를 정상 연결 검증의 대용으로 사용하지 않습니다.

기존 Tunnel 또는 직접 TLS 구성에서 전환한 경우 서버를 재생성해야 합니다. `.env`의 `HTTP_PORT=80`, 방화벽과 Cloudflare 설정을 준비한 뒤 `docker compose -f compose.prod.yaml up -d --build --force-recreate --wait server`로 적용합니다. DB 볼륨은 삭제하지 않습니다. 실패하면 설정·포트·방화벽과 서버가 실제로 받는 접속 출발지를 확인하며 전체 CIDR 허용으로 우회하지 않습니다.

## 4. 참여자 연결과 개방 확인

브라우저와 인증서를 검증할 수 있는 참여자 컴퓨터에서 MCP 클라이언트를 빌드합니다. 서버용 `.env`나 Gemini 키는 참여자에게 배포하지 않습니다.

```sh
(cd client && go build -o ../out/agent-context-client ./cmd/client)
export AGENT_CONTEXT_CLIENT_REMOTE_URL=https://agent-context.example.com/mcp
export AGENT_CONTEXT_CLIENT_ID=agent-context
export AGENT_CONTEXT_CLIENT_AGENT_ID=0198e7c0-0000-7000-8000-000000000001
export AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT=5m
export AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT=30s
export AGENT_CONTEXT_CLIENT_TOOL_POLICY=all
export AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST=
./out/agent-context-client doctor
```

같은 절차를 `client-setup.sh`로 실행할 수 있습니다. `init`은 빌드 후 `out/client.env`를 만들며, 에이전트 식별자를 지정하지 않으면 UUIDv7을 생성하고 다시 실행해도 기존 식별자를 유지합니다. `host-config`는 MCP 호스트에 등록할 실행 정보를 출력합니다.

```sh
./client-setup.sh init https://agent-context.example.com/mcp agent-context
./client-setup.sh doctor
./client-setup.sh host-config
```

예시 에이전트 식별자를 실제 에이전트별 고유 UUIDv7로 바꿉니다. `/register`에서 시험 계정을 생성한 뒤 브라우저 인가를 완료합니다. `doctor` 종료 코드 `0`과 모든 검사 `pass`를 확인합니다. 클라이언트의 `serve` 실행 정보는 [README](README.md)의 호스트 연결 예시를 참고합니다.

개방 전에는 전용 시험 그래프에서 다음 사항을 확인하고 결과·시각·서버 commit과 이미지 ID를 기록합니다.

- MCP 도구 목록에서 정책 `all` 기준 13종을 확인하고 `graph_create`로 시험 그래프를 만듭니다.
- `node_create`로 민감 정보가 없는 원천 컨텍스트를 저장하고 `node_get`으로 직접 조회합니다. 실제 인자는 도구 스키마를 사용합니다.
- 색인 대기가 해소된 뒤 `context_flow_get`의 의미 검색과 키워드 검색을 확인합니다. 직접 조회 성공만으로 임베딩 제공자 성공을 판정하지 않습니다.
- `/graphs`에서 시험 그래프 조회·국소 그래프 보기를 확인합니다. 소유자가 다른 시험 계정에 등급을 부여하고, 열람자의 쓰기가 거부되는지 확인합니다.
- 클라이언트를 재시작하여 재인증을 확인하고, 잘못된 계정·인가 범위로 다른 그래프를 읽을 수 없는지 확인합니다.
- 「백업과 복구」의 첫 물리 백업과 격리 복구 리허설을 완료합니다.
- 호스트 재부팅 후 DB·서버와 원본 포트·방화벽이 정상으로 돌아오며 `/readyz`와 클라이언트 진단이 성공하는지 확인합니다.

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

`pg_stat_archiver`의 실패 횟수는 누적값이므로 이전 점검과 비교합니다. 마지막 성공 시각은 쓰기가 없으면 오래될 수 있습니다. 실패 증가와 WAL·디스크 사용량을 함께 판단합니다. WAL 보관 실패가 지속되면 디스크가 가득 차 DB가 멈출 수 있습니다.

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

Apache AGE 그래프는 스키마 OID를 참조하므로 `pg_dump`·`pg_restore`만으로 그래프를 정상 복구할 수 없습니다. **기본 물리 백업과 연속 WAL 보관**을 사용합니다. 계정·권한·원본·관계·서명 키를 함께 보존하며 운영 `.env`, 배포 commit, 이미지 ID와 DNS·Cloudflare SSL 모드 및 방화벽 설정도 별도로 보호합니다.

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
10. 실제 장애 복구에서는 최종 복구 승인 후에만 새 DB로 배치를 전환하고 원본 접속을 다시 허용합니다. `/readyz`, 외부 로그인과 참여자 `doctor`를 확인합니다. 복구 후 새 기본 백업을 받아 새 timeline의 보관을 시작하고 실제 손실 범위·복구 시간을 기록합니다.

리허설용 복사본의 회수·삭제는 볼륨 이름과 보존 대상이 확인된 후 별도 승인된 정리로 수행합니다. 서명 키가 유실되거나 이전 상태로 돌아오면 기존 토큰·웹 세션이 무효가 될 수 있으므로 참여자에게 재인증을 안내합니다. 그래프 감사가 실패하면 개방하지 않습니다. 시점 복구의 상세 설정과 WAL 처리 기준은 [PostgreSQL 18 복구 문서](https://www.postgresql.org/docs/18/continuous-archiving.html)를 따릅니다.

## 7. 변경 및 복귀

변경 창을 알리고 새 판의 요구사항·마이그레이션·임베딩 구성 변화와 현재 스키마의 호환성을 검토합니다. 변경 전 commit, 이미지 ID, 보호된 설정 사본과 복구가 검증된 물리 백업을 확보합니다. 운영 체크아웃에서 자동 `pull`을 전제로 하지 않으며 승인된 판을 명시적으로 선택합니다.

```sh
docker compose -f compose.prod.yaml stop server
docker compose -f compose.prod.yaml build db migrate server
docker compose -f compose.prod.yaml run --rm --no-deps migrate migrate status
docker compose -f compose.prod.yaml run --rm --no-deps migrate migrate up
docker compose -f compose.prod.yaml run --rm --no-deps server audit
docker compose -f compose.prod.yaml up -d --wait server
```

위 예시는 **DB 이미지가 바뀌지 않는 애플리케이션 변경**입니다. `build db`는 실행 중인 DB를 자동 교체하지 않습니다. PostgreSQL·AGE·pgvector 버전이나 DB 이미지를 바꾸는 작업은 별도 검증과 업그레이드 계획이 필요합니다. 각 단계가 성공한 경우에만 진행하고 마지막에 내부·외부 준비 확인과 기능 점검을 반복합니다.

벡터 타입·차원을 변경하려면 다음 번호의 마이그레이션과 재색인 계획이 필요합니다. 적용된 `001_init.sql` 수정, checksum 덮어쓰기나 `.env`만 변경하는 전환은 하지 않습니다. 같은 차원의 모델 변경도 재색인 중 검색 품질과 외부 API 사용량을 확인합니다.

스키마가 호환되면 보호해 둔 이전 이미지·설정으로 서버를 되돌릴 수 있습니다. 마이그레이션에는 `down` 명령이 없으며, 스키마가 호환되지 않으면 이미지 교체만으로 복귀하지 않습니다. 백업·WAL을 새 볼륨에 복구하고 손실 범위 승인을 받은 뒤 배치를 전환합니다. 빌드가 같은 이미지 태그를 갱신하므로 이전 판은 변경 전에 별도 태그 또는 저장한 이미지로 확보하십시오.

## 8. 장애 대응

- **컨테이너는 healthy인데 업무 요청 실패:** `/readyz`, 외부 `/login`, `doctor`와 색인 상태를 따로 확인합니다. DB·TLS·인가·임베딩 경계를 구분합니다.
- **외부 접속 실패:** DNS A 레코드의 원본 IP·프록시 상태, `Flexible` 모드, 원본 TCP 80 공개와 OCI·호스트 방화벽을 확인합니다. 원본 HTTP에 `Full`·`Full (strict)`로 접속하면 TLS 연결이 맞지 않습니다. `https_required`가 나오면 서버의 `TLS_TERMINATION=proxy`, 실제 접속 출발지의 Cloudflare 대역 포함 여부와 단일 `X-Forwarded-Proto: https` 전달을 확인합니다. Docker 또는 중간 프록시가 출발지 주소를 바꿨다면 연결 경로를 조사하고 전체 CIDR·Docker 게이트웨이 신뢰로 우회하지 않습니다. 외부 인증서 오류는 Cloudflare의 방문자 측 인증서·도메인을 대조합니다.
- **`migrate` 실패:** DB 준비 상태, 기존 적용 파일의 checksum과 치환 구성, 권한·확장 버전을 확인합니다. 적용 이력을 삭제하지 않습니다.
- **서버 시작 시 임베딩 스키마 불일치:** 실행 구성과 DB의 벡터 타입·차원을 대조합니다. 기존 DB의 변경은 정식 마이그레이션으로 수행합니다.
- **의미 검색 누락·부분 검색:** Gemini 키·할당량·네트워크와 큐를 확인합니다. 미색인 컨텍스트는 의미 유사도 채널에서 빠질 수 있습니다. 원본 조회·키워드 검색과 구분하여 안내합니다.
- **WAL 보관 실패·디스크 부족:** 외부 입력을 제한하고 보관 경로 권한·여유 공간·실패 시각을 확인합니다. 검증된 백업과 필요한 WAL을 먼저 보호하고 `pg_wal`을 직접 삭제하지 않습니다.
- **MCP 인가 실패:** 공개 URL·issuer, 클라이언트 ID·redirect URI, 브라우저 루프백 도달, 호스트 시각과 도구 정책을 확인합니다. 클라이언트 재시작 후에는 재인증이 정상입니다.
- **감사 위반·데이터 손상:** 개방 또는 변경 완료 판정을 중단하고 백업·현재 상태를 보존합니다. 자동 수정이나 광범위한 SQL 삭제 대신 위반 규칙과 대상에 한정한 수정·복구를 검토합니다.

사고 기록에는 발생 시각, 영향 범위, 마지막 정상 확인, 배포 판, 안전한 로그와 조치·검증 결과만 남깁니다. 본문·토큰·API 키·비밀번호를 포함하지 않습니다.

## 9. 시험 종료

참여자에게 종료 시각과 데이터 보존·반출 정책을 알립니다. 원본으로 들어오는 서비스 트래픽을 차단한 뒤 서버를 정상 종료하여 요청과 주기 작업의 추가 쓰기를 멈춥니다. 도메인의 프록시를 끄고 평문 원본을 직접 노출하는 방식으로 종료하지 않습니다.

```sh
docker compose -f compose.prod.yaml stop server
```

DB가 실행 중인 상태에서 「백업과 복구」의 최종 물리 백업, WAL 전환·보관과 외부 복사를 완료한 뒤 DB를 종료합니다.

```sh
docker compose -f compose.prod.yaml stop db
```

서버는 요청에 최대 30초, 작업자 종료에 10초를 기다리며 Compose 종료 유예는 50초입니다. 정상 종료를 기다리고 불필요한 강제 종료를 피합니다. 컨테이너·네트워크만 제거하려면 백업 확인 후 `docker compose -f compose.prod.yaml down`을 사용할 수 있으며 명명된 볼륨은 남습니다.

**운영 환경에서 `down -v`를 실행하지 마십시오.** 개발 초기화용 명령이며 운영 DB와 WAL 볼륨을 삭제합니다. 데이터·볼륨·백업의 영구 삭제와 API 키 폐기는 보존 기간과 대상이 확인된 별도 승인 절차로 수행합니다.
