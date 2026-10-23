#!/bin/sh
# OPS-02 모듈 인수. 예시 구성과 새 프로젝트만 사용하며 외부 제공자를 호출하지 않는다.
set -eu
umask 077
unset POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB HTTP_PORT DIAGNOSTIC_INSTANCE_ID
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/prod-runtime-test.XXXXXX")
test_project=$(basename "$test_dir" | tr '[:upper:].' '[:lower:]-')
verified=0
compose() {
    docker compose --env-file "$test_dir/runtime.env" --project-name "$test_project" \
        -f "$repo_root/compose.prod.yaml" -f "$test_dir/override.yaml" "$@"
}
cleanup() {
    result=$?
    trap - EXIT
    [ "$verified" -eq 1 ] || result=1
    if [ "$result" -ne 0 ]; then compose logs --no-color >&2 || true; fi
    compose down --volumes --rmi local >/dev/null || result=1
    for image in "$test_project-db" "$test_project-app"; do
        if docker image inspect "$image" >/dev/null 2>&1; then docker image rm "$image" >/dev/null || result=1; fi
    done
    for resource in container volume network; do
        remaining=$(docker "$resource" ls -q --filter "label=com.docker.compose.project=$test_project") || result=1
        [ -z "$remaining" ] || result=1
    done
    rm -rf "$test_dir"
    if [ "$result" -eq 0 ]; then printf 'PASS: OPS-02 격리 실행·재생성·권한/중복 거부·자원 정리\n'; fi
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
fail() { printf '%s\n' "$*" >&2; exit 1; }
# 실제 .env를 복사하거나 source하지 않는다. 예시의 공개 설정에 합성 비밀만 채운다.
sed -e 's/^POSTGRES_USER=.*/POSTGRES_USER=ops02/' \
    -e 's/^POSTGRES_DB=.*/POSTGRES_DB=ops02/' \
    -e 's/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=ops02_synthetic_password/' \
    -e 's/^GEMINI_API_KEY=.*/GEMINI_API_KEY=ops02_synthetic_never_called/' \
    -e 's/^DIAGNOSTIC_INSTANCE_ID=.*/DIAGNOSTIC_INSTANCE_ID=ops02-server-1/' \
    -e 's/^INDEX_WORKER_PLACEMENT=.*/INDEX_WORKER_PLACEMENT=external/' \
    "$repo_root/.env.prod.example" > "$test_dir/runtime.env"
cat > "$test_dir/override.yaml" <<YAML
services:
  db:
    image: $test_project-db
    ports: !reset []
  migrate:
    image: $test_project-app
    env_file: !override
      - path: "$test_dir/runtime.env"
  server:
    image: $test_project-app
    env_file: !override
      - path: "$test_dir/runtime.env"
    ports: !reset []
networks:
  default:
    ipam: !reset {}
YAML
compose config --format json > "$test_dir/config.json"
jq -e '(.services.db.ports // [] | length == 0) and (.services.server.ports // [] | length == 0) and
    (.services.server.environment.INDEX_WORKER_PLACEMENT == "external") and
    (.volumes | all(.[]; (.external // false) == false))' "$test_dir/config.json" >/dev/null
compose build db server
compose up -d --wait --wait-timeout 180
server_id=$(compose ps -q server)
[ "$(docker exec "$server_id" id -u)" = 10001 ] || fail '서버 UID가 다르다.'
[ "$(docker exec "$server_id" stat -c '%u:%a' /var/log/agent-context)" = '10001:700' ] || fail '진단 루트 소유·권한이 다르다.'
for kind in request auth search index management system; do
    [ "$(docker exec "$server_id" stat -c '%u:%a' "/var/log/agent-context/ops02-server-1/$kind.jsonl")" = '10001:600' ] || fail '진단 파일 소유·권한이 다르다.'
done
docker exec "$server_id" wget -q -O /dev/null http://127.0.0.1:8080/readyz
for binary in server migrate audit; do docker exec "$server_id" test -x "/usr/local/bin/$binary"; done
# 같은 슬롯의 두 기록자를 허용하지 않는다. 예상 외 기동은 timeout 코드로 구별한다.
code=0
compose run --rm --no-deps server timeout 15 server > "$test_dir/duplicate.log" 2>&1 || code=$?
[ "$code" = 1 ] || fail "동일 슬롯 중복 기동의 종료 코드가 다르다: $code"
compose stop server
docker cp "$server_id:/var/log/agent-context/ops02-server-1/system.jsonl" "$test_dir/system.before" >/dev/null
[ -s "$test_dir/system.before" ] || fail '시스템 진단 행이 없다.'
jq -e -s 'length > 0 and all(.[]; .instance_id == "ops02-server-1")' "$test_dir/system.before" >/dev/null
first_boot=$(jq -r -s '.[0].boot_id' "$test_dir/system.before")
# 자신이 만든 볼륨의 잘못된 권한은 자동 교정 없이 기동을 거부해야 한다.
compose run --rm --no-deps --entrypoint chmod server 0755 /var/log/agent-context
code=0
compose run --rm --no-deps server timeout 15 server > "$test_dir/permissions.log" 2>&1 || code=$?
[ "$code" = 1 ] || fail "잘못된 권한의 종료 코드가 다르다: $code"
compose run --rm --no-deps --entrypoint sh server -ec \
    'test "$(stat -c %a /var/log/agent-context)" = 755; chmod 0700 /var/log/agent-context'
compose up -d --no-deps --force-recreate --wait --wait-timeout 120 server
new_server_id=$(compose ps -q server)
[ "$new_server_id" != "$server_id" ] || fail '서버가 재생성되지 않았다.'
compose stop server
docker cp "$new_server_id:/var/log/agent-context/ops02-server-1/system.jsonl" "$test_dir/system.after" >/dev/null
bytes=$(wc -c < "$test_dir/system.before" | tr -d ' ')
dd if="$test_dir/system.after" of="$test_dir/prefix" bs=1 count="$bytes" 2>/dev/null
cmp "$test_dir/system.before" "$test_dir/prefix" || fail '재생성 뒤 이전 진단 행이 달라졌다.'
jq -e -s --arg boot "$first_boot" 'any(.[]; .boot_id != $boot and .instance_id == "ops02-server-1")' "$test_dir/system.after" >/dev/null
printf 'runtime: uid=10001 root=0700 files=0600 stable_slot=true append_preserved=true new_boot=true\n'
verified=1
