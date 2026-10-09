#!/bin/sh
# 개발 구성의 공개 주소와 버전 고정 회귀 검사. --runtime은 격리 환경에서 새로 빌드한다.
set -eu

runtime=0
case "$#:$*" in
    0:) ;;
    1:--runtime) runtime=1 ;;
    *) echo "사용법: sh test-dev-compose.sh [--runtime]" >&2; exit 2 ;;
esac

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/dev-compose-test.XXXXXX")
test_project=$(basename "$test_dir" | tr '[:upper:].' '[:lower:]-')
runtime_started=0

compose() {
    docker compose --env-file /dev/null --project-name "$test_project" \
        -f "$repo_root/compose.dev.yaml" "$@"
}

cleanup() {
    result=$?
    trap - EXIT INT TERM
    if [ "$runtime_started" -eq 1 ]; then
        if [ "$result" -ne 0 ]; then
            compose --profile ollama logs --no-color db ollama >&2 || true
        fi
        compose --profile ollama down --volumes --rmi local || result=1
    fi
    rm -f "$test_dir/default.json" "$test_dir/ollama.json" "$test_dir/ports.json" "$test_dir/runtime.json" "$test_dir/server.json"
    rmdir "$test_dir" || result=1
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# 호출자의 .env와 DB 자격 증명을 사용하지 않는다. 공개 인터페이스는 구성에 고정한다.
export POSTGRES_USER=dev_compose_test POSTGRES_PASSWORD=dev_compose_test POSTGRES_DB=dev_compose_test
export WAL_ARCHIVE_RETENTION_DAYS=7
unset POSTGRES_PORT OLLAMA_PORT COMPOSE_PROFILES

check_config() {
    if ! jq -e --arg db_port "$2" --arg ollama_port "$3" '
        (.services.db.ports | length == 1) and
        (.services.db.ports[0] | .host_ip == "127.0.0.1" and .target == 5432 and .published == $db_port) and
        (.services.ollama.ports | length == 1) and
        (.services.ollama.ports[0] | .host_ip == "127.0.0.1" and .target == 11434 and .published == $ollama_port) and
        (.services.ollama.profiles == ["ollama"]) and
        (.services.wal_cleanup.ports // [] | length == 0) and
        (.services.ollama.image == "ollama/ollama:0.33.3@sha256:32931b46719f673c05fdbaa81ccb26da18ea4a1c57590a754874ab28ba269eb2")
    ' "$1" > /dev/null; then
        echo "개발 구성 검사 실패: 포트 바인딩 또는 Ollama 버전 고정을 확인하십시오." >&2
        return 1
    fi
}

compose config --no-env-resolution --format json > "$test_dir/default.json"
jq -e '
    (.services | keys == ["db", "wal_cleanup"]) and
    (.networks | keys == ["default"]) and
    (.volumes | keys == ["db_data"]) and
    (.services.db.ports[0] | .host_ip == "127.0.0.1" and .target == 5432 and .published == "5432")
' "$test_dir/default.json" > /dev/null
compose --profile ollama config --no-env-resolution --format json > "$test_dir/ollama.json"
check_config "$test_dir/ollama.json" 5432 11434
export POSTGRES_PORT=15432 OLLAMA_PORT=21434
compose --profile ollama config --no-env-resolution --format json > "$test_dir/ports.json"
check_config "$test_dir/ports.json" 15432 21434

# 실제 .env를 읽지 않고 호출자의 넓은 신뢰 설정도 고정 경로를 바꾸지 않는지 확인한다.
export HTTP_ADDR=0.0.0.0:9999 TLS_TERMINATION=direct TRUSTED_PROXY_CIDRS='0.0.0.0/0,::/0'
export TLS_CERT_FILE=/unused/cert.pem TLS_KEY_FILE=/unused/key.pem
compose --profile server config --no-env-resolution --format json > "$test_dir/server.json"
jq -e '
    (.services | keys == ["db", "migrate", "server", "wal_cleanup"]) and
    (.services.server.profiles == ["server"]) and
    (.services.migrate.profiles == ["server"]) and
    (.services.server.ports | length == 1) and
    (.services.server.ports[0] | .host_ip == "127.0.0.1" and .target == 8080 and .published == "80") and
    (.services.migrate.ports // [] | length == 0) and
    (.services.server.environment.HTTP_ADDR == ":8080") and
    (.services.server.environment.TLS_TERMINATION == "proxy") and
    (.services.server.environment.TRUSTED_PROXY_CIDRS == "10.204.0.1/32") and
    (.services.server.environment.TLS_CERT_FILE == "") and
    (.services.server.environment.TLS_KEY_FILE == "") and
    (.services.server.volumes // [] | length == 0) and
    (.services.server.environment.DATABASE_URL == "postgres://dev_compose_test:dev_compose_test@db:5432/dev_compose_test") and
    (.services.migrate.environment.DATABASE_URL == .services.server.environment.DATABASE_URL) and
    (.services.migrate.command == ["migrate", "up"]) and
    (.services.migrate.depends_on.db.condition == "service_healthy") and
    (.services.server.depends_on.migrate.condition == "service_completed_successfully") and
    (.services.server.networks | keys == ["default", "tunnel"]) and
    (.services.server.networks.tunnel.priority == 1) and
    (.services.server.networks.tunnel.gw_priority == 1) and
    (.networks.tunnel.ipam.config[0] | .subnet == "10.204.0.0/24" and .gateway == "10.204.0.1") and
    (.services.server.healthcheck.test == ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/healthz"])
' "$test_dir/server.json" > /dev/null
unset HTTP_ADDR TLS_TERMINATION TRUSTED_PROXY_CIDRS TLS_CERT_FILE TLS_KEY_FILE
if ! grep -Eq '^FROM apache/age:release_PG18_1[.]8[.]0@sha256:47a0b054c3663a3e64a25fc0c5a982f1b58348019274b326a691cdf77f78eb58$' "$repo_root/Dockerfile"; then
    echo "개발 구성 검사 실패: DB 베이스 이미지 태그와 digest 고정을 확인하십시오." >&2
    exit 1
fi
grep -Eq 'apt-get install .*postgresql-18-pgvector=0[.]8[.]6-1[.]pgdg13[+]1([[:space:]]|$)' "$repo_root/Dockerfile"
echo "개발 구성 검사 통과: 선택적 profile, 서버 localhost:80·신뢰 경로와 loopback·버전 고정"

if [ "$runtime" -eq 0 ]; then
    exit 0
fi

# Docker가 사용 가능한 임시 포트를 배정하며 기존 개발 포트·볼륨을 사용하지 않는다.
export POSTGRES_PORT=0 OLLAMA_PORT=0
compose --profile ollama config --no-env-resolution --format json > "$test_dir/runtime.json"
check_config "$test_dir/runtime.json" 0 0
runtime_started=1
compose build --no-cache db
compose up -d --wait --wait-timeout 120
db_id=$(compose ps -q db)
test -n "$db_id"
test -z "$(compose --profile ollama ps -q ollama)"
test -z "$(docker volume ls -q --filter "label=com.docker.compose.project=$test_project" --filter 'label=com.docker.compose.volume=ollama_data')"
compose --profile ollama up -d --wait --wait-timeout 120 ollama
ollama_id=$(compose --profile ollama ps -q ollama)
test -n "$db_id" && test -n "$ollama_id"

for binding in "$db_id:5432/tcp" "$ollama_id:11434/tcp"; do
    container_id=${binding%%:*}
    container_port=${binding#*:}
    docker inspect --format '{{json .NetworkSettings.Ports}}' "$container_id" |
        jq -e --arg port "$container_port" '
            .[$port] | length == 1 and all(.[]; .HostIp == "127.0.0.1" and (.HostPort | tonumber) > 0)
        ' > /dev/null
done

test "$(docker exec "$db_id" dpkg-query -W -f='${Version}' postgresql-18-pgvector)" = '0.8.6-1.pgdg13+1'
db_versions=$(docker exec "$db_id" psql -X -U dev_compose_test -d dev_compose_test -qAt -v ON_ERROR_STOP=1 \
    -c 'CREATE EXTENSION IF NOT EXISTS vector; CREATE EXTENSION IF NOT EXISTS age;' \
    -c "SELECT extname || '=' || extversion FROM pg_extension WHERE extname IN ('age', 'vector') ORDER BY extname;")
test "$db_versions" = 'age=1.8.0
vector=0.8.6'
test "$(docker exec "$db_id" psql -X -U dev_compose_test -d dev_compose_test -At -v ON_ERROR_STOP=1 \
    -c "SELECT '[1,0]'::vector <=> '[1,0]'::vector;")" = 0

ollama_address=$(docker port "$ollama_id" 11434/tcp)
curl --noproxy '*' --fail --silent --show-error --max-time 10 "http://$ollama_address/api/version" |
    jq -e '.version == "0.33.3"' > /dev/null
echo "격리 기동 검사 통과: 실제 loopback 바인딩, pgvector·AGE와 Ollama 버전·응답"
