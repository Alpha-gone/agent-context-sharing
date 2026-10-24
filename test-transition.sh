#!/bin/sh
# OPS-08 전용 합성 배포. 실제 .env·운영 자원·외부 제공자를 사용하지 않는다.
set -eu
umask 077
unset POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB HTTP_PORT DIAGNOSTIC_INSTANCE_ID
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/transition-test.XXXXXX")
test_project=$(basename "$test_dir" | tr '[:upper:].' '[:lower:]-')
TRANSITION_APP_IMAGE="$test_project-old"
export TRANSITION_APP_IMAGE
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
    compose down --volumes >/dev/null || result=1
    for suffix in db old new fault; do
        if docker image inspect "$test_project-$suffix" >/dev/null 2>&1; then
            docker image rm "$test_project-$suffix" >/dev/null || result=1
        fi
    done
    for resource in container volume network; do
        remaining=$(docker "$resource" ls -q --filter "label=com.docker.compose.project=$test_project") || result=1
        [ -z "$remaining" ] || result=1
    done
    rm -rf "$test_dir"
    if [ "$result" -eq 0 ]; then printf 'PASS: OPS-08 전환·파일 롤백·백업/이미지 복귀·자원 정리\n'; fi
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
fail() { printf '%s\n' "$*" >&2; exit 1; }
sql() { compose exec -T "$1" psql -U ops08 -d ops08 -X -At -v ON_ERROR_STOP=1 -c "$2"; }
go_binary=${GO_BINARY:-go}
[ "$("$go_binary" env GOVERSION)" = go1.27.1 ] || fail 'Go 1.27.1이 필요하다.'
architecture=$(docker info --format '{{.Architecture}}')
case "$architecture" in aarch64|arm64) architecture=arm64;; x86_64|amd64) architecture=amd64;; *) fail '지원하지 않는 Docker 아키텍처다.';; esac
sed -e 's/^POSTGRES_USER=.*/POSTGRES_USER=ops08/' \
    -e 's/^POSTGRES_DB=.*/POSTGRES_DB=ops08/' \
    -e 's/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=ops08/' \
    -e 's/^GEMINI_API_KEY=.*/GEMINI_API_KEY=ops08_synthetic_never_called/' \
    -e 's/^DIAGNOSTIC_INSTANCE_ID=.*/DIAGNOSTIC_INSTANCE_ID=transition-server/' \
    -e 's/^INDEX_WORKER_PLACEMENT=.*/INDEX_WORKER_PLACEMENT=external/' \
    "$repo_root/.env.prod.example" > "$test_dir/runtime.env"
cat > "$test_dir/override.yaml" <<YAML
services:
  db:
    image: $test_project-db
    ports: !reset []
    restart: "no"
    volumes:
      - backup:/backup
  migrate:
    image: \${TRANSITION_APP_IMAGE}
    env_file: !override
      - path: "$test_dir/runtime.env"
  server:
    image: \${TRANSITION_APP_IMAGE}
    env_file: !override
      - path: "$test_dir/runtime.env"
    ports: !reset []
    restart: "no"
  recovered:
    image: $test_project-db
    environment:
      POSTGRES_USER: ops08
      POSTGRES_PASSWORD: ops08
      POSTGRES_DB: ops08
    volumes:
      - recovered_data:/var/lib/postgresql
      - backup:/backup:ro
    command: [postgres, -c, shared_preload_libraries=age, -c, archive_mode=off]
    healthcheck:
      test: [CMD-SHELL, pg_isready -U ops08 -d ops08]
      interval: 1s
      timeout: 3s
      retries: 90
networks:
  default:
    internal: true
    ipam: !reset {}
volumes:
  backup:
  recovered_data:
YAML
compose config --format json > "$test_dir/config.json"
jq -e '(.services | all(.[]; (.ports // [] | length) == 0)) and
    (.networks.default.internal == true) and
    (.volumes | all(.[]; (.external // false) == false)) and
    (.services.server.environment.INDEX_WORKER_PLACEMENT == "external")' "$test_dir/config.json" >/dev/null
cd "$repo_root"
CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" "$go_binary" test -c -o "$test_dir/transition.test" ./cmd/server
# 같은 코드에서013만 제외한 합성 이전 형상이며 과거 출시 이미지가 아니다.
mkdir "$test_dir/old" "$test_dir/fault"
for part in go.mod go.sum Dockerfile.server cmd internal migrations; do
    cp -R "$repo_root/$part" "$test_dir/old/"
    cp -R "$repo_root/$part" "$test_dir/fault/"
done
rm "$test_dir/old/migrations/013_context_revision.sql"
cat > "$test_dir/fault/migrations/014_ops08_success.sql" <<'SQL'
CREATE TABLE public.ops08_committed (id integer PRIMARY KEY);
SQL
cat > "$test_dir/fault/migrations/015_ops08_failure.sql" <<'SQL'
CREATE TABLE public.ops08_rolled_back (id integer PRIMARY KEY);
UPDATE public.context_graph SET name='이 변경은 롤백되어야 한다';
SELECT 1/0;
SQL
docker build -f "$test_dir/old/Dockerfile.server" -t "$test_project-old" "$test_dir/old"
old_image=$(docker image inspect "$test_project-old" --format '{{.Id}}')
TRANSITION_APP_IMAGE="$test_project-new"
compose build db server
new_image=$(docker image inspect "$test_project-new" --format '{{.Id}}')
[ "$old_image" != "$new_image" ] || fail '이전/목표 이미지가 같다.'
docker build -f "$test_dir/fault/Dockerfile.server" -t "$test_project-fault" "$test_dir/fault"
TRANSITION_APP_IMAGE="$test_project-old"
compose up -d --wait --wait-timeout 120 db
compose run --rm --no-deps migrate migrate up
[ "$(sql db 'SELECT max(version) FROM public.schema_migration')" = 12 ] || fail '합성 이전 형상이 012가 아니다.'
run_fixture() {
    service=$1 phase=$2
    container=$(compose ps -q "$service")
    docker cp "$test_dir/transition.test" "$container:/tmp/transition.test" >/dev/null
    compose exec -T -e TEST_DATABASE_URL=postgres://ops08:ops08@localhost/ops08 \
        -e TEST_DATABASE_REQUIRED=1 -e OPS08_PHASE="$phase" "$service" \
        /tmp/transition.test -test.run '^TestOPS08Transition$' -test.v -test.timeout=60s
}
audit() { compose run --rm --no-deps server audit; }
ready() {
    container=$(compose ps -q server)
    [ "$(docker inspect "$container" --format '{{.Image}}')" = "$1" ] || fail '예정한 이미지로 실행되지 않았다.'
    compose exec -T server wget -q -O /dev/null http://127.0.0.1:8080/healthz
    compose exec -T server wget -q -O /dev/null http://127.0.0.1:8080/readyz
}
run_fixture db seed
audit
compose up -d --no-deps --wait --wait-timeout 120 server
ready "$old_image"
compose stop server
[ "$(docker inspect "$(compose ps -aq server)" --format '{{.State.ExitCode}}')" = 0 ] || fail '이전 서버가 정상 종료되지 않았다.'
run_fixture db verify
history=$(sql db 'SELECT md5(string_agg(row_to_json(m)::text,CHR(10) ORDER BY version)) FROM public.schema_migration m')
compose exec -T --user root db install -d -m 0700 -o postgres -g postgres /backup
compose exec -T --user postgres db pg_basebackup -U ops08 -D /backup/base --format=plain --wal-method=stream --checkpoint=fast
compose exec -T --user postgres db pg_verifybackup /backup/base
TRANSITION_APP_IMAGE="$test_project-new"
compose run --rm --no-deps migrate migrate status
compose run --rm --no-deps migrate migrate up
compose run --rm --no-deps migrate migrate up > "$test_dir/repeat.log"
rg -q '적용할 마이그레이션이 없다' "$test_dir/repeat.log" || fail '반복 적용이 무변경이 아니다.'
run_fixture db verify
audit
compose up -d --no-deps --force-recreate --wait --wait-timeout 120 server
ready "$new_image"
run_fixture db write
audit
compose stop server
# 파일 단위 경계: 성공한014는 남고015의 DDL/업무 변경/이력만 모두 롤백된다.
TRANSITION_APP_IMAGE="$test_project-fault"
code=0
compose run --rm --no-deps migrate migrate up > "$test_dir/failure.log" 2>&1 || code=$?
[ "$code" = 1 ] || fail "실패 적용의 종료 코드가 다르다: $code"
[ "$(sql db "SELECT max(version)=14 AND count(*) FILTER (WHERE version=15)=0 FROM public.schema_migration")" = t ] || fail '파일별 적용 이력이 다르다.'
[ "$(sql db "SELECT to_regclass('public.ops08_committed') IS NOT NULL AND to_regclass('public.ops08_rolled_back') IS NULL AND NOT EXISTS (SELECT FROM public.context_graph WHERE name='이 변경은 롤백되어야 한다')")" = t ] || fail '실패 파일의 변경이 남았다.'
printf 'transition: old=012 new=013 used_revision=true repeat_noop=true failed_file_rolled_back=true earlier_file_preserved=true\n'
# 이미 사용한 스키마는 down 대신 별도 빈 볼륨의 물리 백업과 보관 이미지로 복귀한다.
compose run --rm --no-deps --entrypoint sh recovered -eu -c '
    test ! -e "$PGDATA/PG_VERSION"
    install -d -m 0700 -o postgres -g postgres "$PGDATA"
    cp -a /backup/base/. "$PGDATA/"
'
compose up -d --wait --wait-timeout 120 recovered
[ "$(sql recovered "SELECT max(version)=12 AND to_regclass('public.context_revision') IS NULL AND to_regclass('public.ops08_committed') IS NULL FROM public.schema_migration")" = t ] || fail '복원 스키마가 이전 형상이 아니다.'
[ "$(sql recovered 'SELECT md5(string_agg(row_to_json(m)::text,CHR(10) ORDER BY version)) FROM public.schema_migration m')" = "$history" ] || fail '과거 적용 이력이 달라졌다.'
run_fixture recovered verify
# 복원본으로 접속을 전환하고 자동 마이그레이션 없이 보관 이미지로 감사·준비를 확인한다.
cat > "$test_dir/restore.yaml" <<'YAML'
services:
  server:
    environment:
      DATABASE_URL: postgres://ops08:ops08@recovered:5432/ops08
YAML
compose_original() {
    docker compose --env-file "$test_dir/runtime.env" --project-name "$test_project" \
        -f "$repo_root/compose.prod.yaml" -f "$test_dir/override.yaml" -f "$test_dir/restore.yaml" "$@"
}
TRANSITION_APP_IMAGE="$test_project-old"
compose_original run --rm --no-deps server audit
compose_original up -d --no-deps --force-recreate --wait --wait-timeout 120 server
ready "$old_image"
compose_original stop server
run_fixture recovered verify
[ "$(sql db 'SELECT max(version) FROM public.schema_migration')" = 14 ] || fail '전환한 원본 DB가 변경됐다.'
printf 'restore: old_image=true schema=012 history_preserved=true originals_permissions_keys_queue_session=true source_retained=true\n'
verified=1
