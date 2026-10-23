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
    if [ "$runtime_started" -eq 1 ]; then
        set -- -f "$test_dir/runtime.override.yaml" "$@"
    fi
    docker compose --env-file /dev/null --project-name "$test_project" \
        -f "$repo_root/compose.dev.yaml" "$@"
}

cleanup() {
    result=$?
    trap - EXIT INT TERM
    if [ "$runtime_started" -eq 1 ]; then
        if [ "$result" -ne 0 ]; then
            compose logs --no-color db wal_cleanup >&2 || true
        fi
        compose down --volumes --rmi local || result=1
        # 성공 출력은 시험이 만든 자원이 모두 반환된 뒤에만 허용한다.
        remaining_containers=$(docker ps -aq --filter "label=com.docker.compose.project=$test_project") || result=1
        test -z "$remaining_containers" || result=1
        remaining_volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$test_project") || result=1
        test -z "$remaining_volumes" || result=1
        remaining_networks=$(docker network ls -q --filter "label=com.docker.compose.project=$test_project") || result=1
        test -z "$remaining_networks" || result=1
    fi
    rm -f "$test_dir/default.json" "$test_dir/ports.json" "$test_dir/runtime.json" "$test_dir/server.json" "$test_dir/age.txt" "$test_dir/runtime.override.yaml"
    rmdir "$test_dir" || result=1
    if [ "$result" -eq 0 ] && [ "$runtime_started" -eq 1 ]; then
        echo "격리 자원 정리 통과: 시험 컨테이너·볼륨·네트워크 반환"
    fi
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# 호출자의 .env와 DB 자격 증명을 사용하지 않는다. 공개 인터페이스는 구성에 고정한다.
export POSTGRES_USER=dev_compose_test POSTGRES_PASSWORD=dev_compose_test POSTGRES_DB=dev_compose_test
export WAL_ARCHIVE_RETENTION_DAYS=7
unset POSTGRES_PORT COMPOSE_PROFILES
unset DIAGNOSTIC_INSTANCE_ID

check_config() {
    if ! jq -e --arg db_port "$2" '
        (.services.db.ports | length == 1) and
        (.services.db.ports[0] | .host_ip == "127.0.0.1" and .target == 5432 and .published == $db_port) and
        (.services.wal_cleanup.ports // [] | length == 0) and
        (.services.db.build.dockerfile == "Dockerfile") and
        (.services.wal_cleanup.build == .services.db.build) and
        (.services.db.environment | .POSTGRES_USER == "dev_compose_test" and .POSTGRES_PASSWORD == "dev_compose_test" and .POSTGRES_DB == "dev_compose_test") and
        (.services.db.volumes | length == 1) and
        (.services.db.volumes[0] | .type == "volume" and .source == "db_data" and .target == "/var/lib/postgresql") and
        (.services.wal_cleanup.volumes == .services.db.volumes) and
        (.services.db.command == ["postgres", "-c", "shared_preload_libraries=age", "-c", "archive_mode=on", "-c", "archive_command=test ! -f /var/lib/postgresql/wal_archive/%f && cp %p /var/lib/postgresql/wal_archive/%f"]) and
        (.services.wal_cleanup.depends_on.db.condition == "service_healthy") and
        (.services.wal_cleanup.user == "postgres") and
        (.services.wal_cleanup.entrypoint == ["/bin/sh", "-c"]) and
        (.services.wal_cleanup.command[0] | contains("-mtime +7 -delete") and contains("sleep 86400")) and
        (.volumes.db_data | (.external // false) == false) and
        (.volumes.db_data.name == (.name + "_db_data")) and
        (.networks.default | (.external // false) == false) and
        (.networks.default.name == (.name + "_default"))
    ' "$1" > /dev/null; then
        echo "개발 구성 검사 실패: DB 포트 바인딩과 서비스 구성을 확인하십시오." >&2
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
check_config "$test_dir/default.json" 5432
export POSTGRES_PORT=15432
compose config --no-env-resolution --format json > "$test_dir/ports.json"
check_config "$test_dir/ports.json" 15432

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
    (.services.server.volumes | length == 1) and
    (.services.server.volumes[0] | .type == "volume" and .source == "diagnostic_logs" and .target == "/var/log/agent-context") and
    (.services.server.environment.DIAGNOSTIC_LOG_DIR == "/var/log/agent-context") and
    (.services.server.environment.DIAGNOSTIC_INSTANCE_ID == "development") and
    (.services.server.environment.DATABASE_URL == "postgres://dev_compose_test:dev_compose_test@db:5432/dev_compose_test") and
    (.services.migrate.environment.DATABASE_URL == .services.server.environment.DATABASE_URL) and
    (.services.migrate.command == ["migrate", "up"]) and
    (.services.migrate.depends_on.db.condition == "service_healthy") and
    (.services.server.build == .services.migrate.build) and
    (.services.server.build.dockerfile == "Dockerfile.server") and
    (.services.server.image == .services.migrate.image) and
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
grep -Fx '.env' "$repo_root/.dockerignore" > /dev/null
grep -Fx '.env.*' "$repo_root/.dockerignore" > /dev/null
echo "개발 구성 검사 통과: 선택적 profile, 서버 localhost:80·신뢰 경로와 loopback·버전 고정"

if [ "$runtime" -eq 0 ]; then
    exit 0
fi

# Docker가 사용 가능한 임시 포트를 배정하며 기존 개발 포트·볼륨을 사용하지 않는다.
export POSTGRES_PORT=0
compose config --no-env-resolution --format json > "$test_dir/runtime.json"
check_config "$test_dir/runtime.json" 0
# --env-file은 보간용 파일만 바꾸므로 service env_file도 별도로 제거한다.
# 비활성 서버 profile의 설정을 해석하더라도 실제 .env를 읽지 않는다.
cat > "$test_dir/runtime.override.yaml" <<'YAML'
services:
  migrate:
    env_file: !reset []
  server:
    env_file: !reset []
YAML
runtime_started=1
compose --profile server config --format json > "$test_dir/runtime.json"
check_config "$test_dir/runtime.json" 0
jq -e '
    (.services.migrate.env_file // [] | length == 0) and
    (.services.server.env_file // [] | length == 0) and
    (.services.migrate.environment | keys == ["DATABASE_URL"])
' "$test_dir/runtime.json" > /dev/null
# 둘 다 빌드하여 up이 기존 이미지나 암묵적 빌드에 의존하지 않게 한다.
compose build --no-cache db wal_cleanup
compose up -d --no-build --wait --wait-timeout 120
db_id=$(compose ps -q db)
test -n "$db_id"
test "$(compose ps --services | sort | tr '\n' ' ')" = 'db wal_cleanup '
docker inspect --format '{{json .NetworkSettings.Ports}}' "$db_id" |
    jq -e '."5432/tcp" | length == 1 and all(.[]; .HostIp == "127.0.0.1" and (.HostPort | tonumber) > 0)' > /dev/null

test "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$db_id")" = "$test_project"
volume_name=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/postgresql"}}{{.Name}}{{end}}{{end}}' "$db_id")
test -n "$volume_name"
test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$volume_name")" = "$test_project"
docker exec "$db_id" sh -eu -c '
    test "$(stat -c %U:%G:%a /var/lib/postgresql/wal_archive)" = postgres:postgres:700
    test "$(stat -c %u /var/lib/postgresql/wal_archive)" = "$(id -u postgres)"
'
test "$(docker exec "$db_id" psql -X -U dev_compose_test -d dev_compose_test -At -v ON_ERROR_STOP=1 \
    -c "SELECT current_setting('server_version_num')::int / 10000;")" = 18

test "$(docker exec "$db_id" dpkg-query -W -f='${Version}' postgresql-18-pgvector)" = '0.8.6-1.pgdg13+1'
db_versions=$(docker exec "$db_id" psql -X -U dev_compose_test -d dev_compose_test -qAt -v ON_ERROR_STOP=1 \
    -c 'CREATE EXTENSION IF NOT EXISTS vector; CREATE EXTENSION IF NOT EXISTS age;' \
    -c "SELECT extname || '=' || extversion FROM pg_extension WHERE extname IN ('age', 'vector') ORDER BY extname;")
test "$db_versions" = 'age=1.8.0
vector=0.8.6'
test "$(docker exec "$db_id" psql -X -U dev_compose_test -d dev_compose_test -At -v ON_ERROR_STOP=1 \
    -c "SELECT '[1,0]'::vector <=> '[1,0]'::vector;")" = 0

# 확장 설치뿐 아니라 AGE 적재·search_path·합성 그래프의 실제 질의도 확인한다.
docker exec -i "$db_id" psql -X -U dev_compose_test -d dev_compose_test -qAt -v ON_ERROR_STOP=1 > "$test_dir/age.txt" <<'SQL'
LOAD 'age';
SET search_path = ag_catalog, "$user", public;
SELECT create_graph('ops01_test');
SELECT * FROM cypher('ops01_test', $$ CREATE (:Probe {value: 42}) $$) AS (v agtype);
SELECT * FROM cypher('ops01_test', $$ MATCH (n:Probe) RETURN n.value $$) AS (value agtype);
SQL
test "$(sed '/^$/d' "$test_dir/age.txt")" = 42
# WAL 설정 확인도 같은 전용 DB 안에서만 수행한다.
test "$(docker exec "$db_id" psql -X -U dev_compose_test -d dev_compose_test -At -v ON_ERROR_STOP=1 \
    -c "SELECT current_setting('archive_mode') || '|' || current_setting('shared_preload_libraries');")" = 'on|age'

echo "격리 기동 검사 통과: PostgreSQL 18·AGE 1.8.0·pgvector 0.8.6, loopback·WAL 경로·벡터/그래프 SQL"
