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
        -f "$repo_root/compose.yaml" "$@"
}

cleanup() {
    result=$?
    trap - EXIT INT TERM
    if [ "$runtime_started" -eq 1 ]; then
        if [ "$result" -ne 0 ]; then
            compose logs --no-color db ollama >&2 || true
        fi
        compose down --volumes --rmi local || result=1
    fi
    rm -f "$test_dir/default.json" "$test_dir/ports.json" "$test_dir/runtime.json"
    rmdir "$test_dir" || result=1
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# 호출자의 .env와 DB 자격 증명을 사용하지 않는다. 공개 인터페이스는 구성에 고정한다.
export POSTGRES_USER=dev_compose_test POSTGRES_PASSWORD=dev_compose_test POSTGRES_DB=dev_compose_test
export WAL_ARCHIVE_RETENTION_DAYS=7
unset POSTGRES_PORT OLLAMA_PORT

check_config() {
    if ! jq -e --arg db_port "$2" --arg ollama_port "$3" '
        (.services.db.ports | length == 1) and
        (.services.db.ports[0] | .host_ip == "127.0.0.1" and .target == 5432 and .published == $db_port) and
        (.services.ollama.ports | length == 1) and
        (.services.ollama.ports[0] | .host_ip == "127.0.0.1" and .target == 11434 and .published == $ollama_port) and
        (.services.wal_cleanup.ports // [] | length == 0) and
        (.services.ollama.image == "ollama/ollama:0.33.3@sha256:32931b46719f673c05fdbaa81ccb26da18ea4a1c57590a754874ab28ba269eb2")
    ' "$1" > /dev/null; then
        echo "개발 구성 검사 실패: 포트 바인딩 또는 Ollama 버전 고정을 확인하십시오." >&2
        return 1
    fi
}

compose config --format json > "$test_dir/default.json"
check_config "$test_dir/default.json" 5432 11434
export POSTGRES_PORT=15432 OLLAMA_PORT=21434
compose config --format json > "$test_dir/ports.json"
check_config "$test_dir/ports.json" 15432 21434
grep -Eq 'apt-get install .*postgresql-18-pgvector=0[.]8[.]6-1[.]pgdg13[+]1([[:space:]]|$)' "$repo_root/Dockerfile"
echo "개발 구성 검사 통과: 기본·재정의 포트의 loopback 바인딩과 버전 고정"

if [ "$runtime" -eq 0 ]; then
    exit 0
fi

# Docker가 사용 가능한 임시 포트를 배정하며 기존 개발 포트·볼륨을 사용하지 않는다.
export POSTGRES_PORT=0 OLLAMA_PORT=0
compose config --format json > "$test_dir/runtime.json"
check_config "$test_dir/runtime.json" 0 0
runtime_started=1
compose build --no-cache db
compose up -d --wait --wait-timeout 120 db ollama
db_id=$(compose ps -q db)
ollama_id=$(compose ps -q ollama)
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
