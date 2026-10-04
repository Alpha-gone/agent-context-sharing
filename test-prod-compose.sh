#!/bin/sh
# 운영 구성의 공개 범위·신뢰 프록시·WAL 보관과 버전 고정 정적 검사. 이미지를 빌드하거나
# 컨테이너를 띄우지 않는다.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/prod-compose-test.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT

# 호출자의 변수 치환 값을 쓰지 않는다. 애플리케이션 env_file은 검사 대상 키를 compose가
# 덮어쓰므로 결과에 영향을 주지 않는다.
compose() {
    docker compose --env-file /dev/null -f "$repo_root/compose.prod.yaml" "$@"
}
unset POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB HTTP_PORT

if compose config --format json > /dev/null 2>&1; then
    echo "운영 구성 검사 실패: POSTGRES_PASSWORD 없이 구성이 만들어졌습니다." >&2
    exit 1
fi

check_config() {
    if ! jq -e --arg http_port "$2" '
        (.services | keys == ["db", "migrate", "server"]) and
        (.services.db.ports // [] | length == 0) and
        (.services.migrate.ports // [] | length == 0) and
        (.services.server.ports | length == 1) and
        (.services.server.ports[0] | .host_ip == "127.0.0.1" and .target == 8080 and .published == $http_port) and
        (.services.db.command | index("archive_mode=on") != null) and
        (.services.db.volumes | map(.target) | index("/var/lib/postgresql/wal_archive") != null) and
        (.services.server.environment.TLS_TERMINATION == "proxy") and
        (.services.server.environment.TRUSTED_PROXY_CIDRS == "10.203.0.1/32") and
        (.services.server.environment.DATABASE_URL | startswith("postgres://prod_compose_test:prod_compose_test@db:5432/")) and
        (.networks.default.ipam.config[0] | .subnet == "10.203.0.0/24" and .gateway == "10.203.0.1")
    ' "$1" > /dev/null; then
        echo "운영 구성 검사 실패: 공개 포트, 신뢰 프록시 또는 WAL 보관을 확인하십시오." >&2
        return 1
    fi
}

export POSTGRES_USER=prod_compose_test POSTGRES_PASSWORD=prod_compose_test
compose config --format json > "$test_dir/default.json"
check_config "$test_dir/default.json" 8080
export HTTP_PORT=18080
compose config --format json > "$test_dir/port.json"
check_config "$test_dir/port.json" 18080

grep -Eq '^FROM golang:1[.]27[.]1-trixie@sha256:3b77fc618ec235a1ab412de7737f120dd507c57e8d87de4cbb7994fb94275ed5 AS build$' "$repo_root/Dockerfile.server"
grep -Eq '^FROM alpine:3[.]23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0$' "$repo_root/Dockerfile.server"
echo "운영 구성 검사 통과: 공개 범위, 신뢰 프록시, WAL 보관과 버전 고정"
