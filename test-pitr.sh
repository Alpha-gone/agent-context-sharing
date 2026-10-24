#!/bin/sh
# OPS-07 모듈 인수. 이 실행의 합성 클러스터·백업만 생성하고 제거한다.
set -eu
umask 077
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/pitr-test.XXXXXX")
test_project=$(basename "$test_dir" | tr '[:upper:].' '[:lower:]-')
PITR_TEST_IMAGE="${test_project}-db"
export PITR_TEST_IMAGE
compose() {
    docker compose --env-file /dev/null --project-name "$test_project" \
        -f "$repo_root/compose.pitr-test.yaml" "$@"
}
cleanup() {
    result=$?
    trap - EXIT
    if [ "$result" -ne 0 ]; then compose logs --no-color >&2 || true; fi
    compose down --volumes --rmi local >/dev/null || result=1
    if docker image inspect "$PITR_TEST_IMAGE" >/dev/null 2>&1; then
        docker image rm "$PITR_TEST_IMAGE" >/dev/null || result=1
    fi
    for resource in container volume network; do
        remaining=$(docker "$resource" ls -q --filter "label=com.docker.compose.project=$test_project") || result=1
        [ -z "$remaining" ] || result=1
    done
    rm -rf "$test_dir"
    if [ "$result" -eq 0 ]; then printf 'PASS: 시험 컨테이너·볼륨·네트워크·이미지 정리\n'; fi
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
fail() { printf '%s\n' "$*" >&2; exit 1; }
sql() { compose exec -T "$1" psql -U pitr_test -d pitr_test -X -At -v ON_ERROR_STOP=1 -c "$2"; }
go_binary=${GO_BINARY:-go}
[ "$("$go_binary" env GOVERSION)" = go1.27.1 ] || fail 'Go 1.27.1이 필요하다. GO_BINARY로 SDK를 지정한다.'
architecture=$(docker info --format '{{.Architecture}}')
case "$architecture" in aarch64|arm64) architecture=arm64;; x86_64|amd64) architecture=amd64;; *) fail '지원하지 않는 Docker 아키텍처다.';; esac
cd "$repo_root"
CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" "$go_binary" test -c -o "$test_dir/pitr.test" ./cmd/server
CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" "$go_binary" build -o "$test_dir/audit" ./cmd/audit
compose build source
compose run --rm --no-deps --entrypoint /bin/sh source -eu -c \
    'install -d -m 0700 -o postgres -g postgres /var/lib/postgresql/cold /backup'
compose up -d --wait --wait-timeout 120 source
source_id=$(compose ps -q source)
sql source "CREATE TABLESPACE ops07_cold LOCATION '/var/lib/postgresql/cold'" >/dev/null
run_fixture() {
    service=$1 phase=$2
    container=$(compose ps -q "$service")
    docker cp "$test_dir/pitr.test" "$container:/tmp/pitr.test" >/dev/null
    compose exec -T -e TEST_DATABASE_URL=postgres://pitr_test:pitr_test@localhost/pitr_test \
        -e TEST_DATABASE_REQUIRED=1 -e OPS07_PHASE="$phase" "$service" \
        /tmp/pitr.test -test.run '^TestOPS07PhysicalRecovery$' -test.v -test.timeout=120s
}
run_fixture source seed
cold_oid=$(sql source "SELECT oid FROM pg_tablespace WHERE spcname='ops07_cold'")
case "$cold_oid" in ''|*[!0-9]*) fail 'cold tablespace OID가 올바르지 않다.';; esac
compose exec -T --user postgres source pg_basebackup -U pitr_test -D /backup/base \
    --format=plain --wal-method=stream --checkpoint=fast \
    --tablespace-mapping=/var/lib/postgresql/cold=/backup/cold
compose exec -T --user postgres source pg_verifybackup /backup/base
# 불완전한 백업을 통과로 판정하지 않는지 실제 manifest 검사를 대조한다.
compose exec -T --user postgres source sh -eu -c \
    'cp /backup/base/PG_VERSION /backup/version.saved; printf bad > /backup/base/PG_VERSION'
if compose exec -T --user postgres source pg_verifybackup /backup/base >"$test_dir/corrupt.log" 2>&1; then
    fail '손상시킨 백업을 허용했다.'
fi
compose exec -T --user postgres source sh -eu -c 'cp /backup/version.saved /backup/base/PG_VERSION'
compose exec -T --user postgres source pg_verifybackup /backup/base
printf 'backup: manifest_and_wal_verified=true corruption_rejected=true\n'
# 기본 백업 이후에만 있는 변경을 목표에 포함한다. 이후 변경은 복구에서 제외해야 한다.
sql source "UPDATE public.context_graph SET name='목표 시점'" >/dev/null
sql source "SELECT pg_create_restore_point('ops07_target')" >/dev/null
sql source "UPDATE public.context_graph SET name='목표 이후'; DELETE FROM public.index_task" >/dev/null
sql source 'SELECT pg_switch_wal()' >/dev/null
attempts=0
while [ "$(sql source "SELECT count(*) FROM pg_ls_dir('pg_wal/archive_status') n WHERE n LIKE '%.ready'")" -ne 0 ]; do
    attempts=$((attempts + 1)); [ "$attempts" -lt 90 ] || fail 'WAL 보관 시간 초과'; sleep 1
done
[ "$(sql source 'SELECT failed_count FROM pg_stat_archiver')" -eq 0 ] || fail 'WAL 보관 실패가 발생했다.'
# 백업 사본만 새 빈 데이터·cold 볼륨에 복사한다. 원본 볼륨은 연결하지 않는다.
compose run --rm --no-deps -e COLD_OID="$cold_oid" --entrypoint /bin/sh recovered -eu -c '
    test ! -e "$PGDATA/PG_VERSION"
    test -z "$(ls -A /var/lib/postgresql/cold)"
    install -d -m 0700 -o postgres -g postgres "$PGDATA"
    cp -a /backup/base/. "$PGDATA/"
    cp -a /backup/cold/. /var/lib/postgresql/cold/
    rm "$PGDATA/pg_tblspc/$COLD_OID"
    ln -s /var/lib/postgresql/cold "$PGDATA/pg_tblspc/$COLD_OID"
    touch "$PGDATA/recovery.signal"
    chown postgres:postgres "$PGDATA/recovery.signal" /var/lib/postgresql/cold
    chmod 0700 /var/lib/postgresql/cold
'
compose up -d --wait --wait-timeout 120 recovered
attempts=0
while [ "$(sql recovered 'SELECT pg_is_in_recovery() AND pg_is_wal_replay_paused()')" != t ]; do
    attempts=$((attempts + 1)); [ "$attempts" -lt 90 ] || fail '복구 목표에서 일시 정지하지 않았다.'; sleep 1
done
[ "$(sql recovered 'SELECT name FROM public.context_graph')" = '목표 시점' ] || fail '복구 목표 상태가 다르다.'
[ "$(sql source 'SELECT name FROM public.context_graph')" = '목표 이후' ] || fail '원본 상태가 변경됐다.'
run_fixture recovered verify
sql recovered 'SELECT pg_wal_replay_resume()' >/dev/null
attempts=0
while [ "$(sql recovered 'SELECT pg_is_in_recovery()')" != f ]; do
    attempts=$((attempts + 1)); [ "$attempts" -lt 30 ] || fail '복구 승격 시간 초과'; sleep 1
done
recovered_id=$(compose ps -q recovered)
docker cp "$test_dir/audit" "$recovered_id:/tmp/audit" >/dev/null
run_fixture recovered search
compose exec -T -e DATABASE_URL=postgres://pitr_test:pitr_test@localhost/pitr_test \
    -e AGE_GRAPH_NAME=agent_context -e EMBEDDING_PROVIDER=gemini \
    -e EMBEDDING_BASE_URL=https://generativelanguage.googleapis.com \
    -e EMBEDDING_MODEL=gemini-embedding-2 -e EMBEDDING_VECTOR_TYPE=vector \
    -e EMBEDDING_DIMENSION=128 recovered /tmp/audit
[ "$(docker inspect "$source_id" --format '{{.State.Running}}')" = true ] || fail '원본 DB가 실행 중이 아니다.'
printf 'PASS: 물리 백업·cold tablespace·목표 일시 정지/승격·AGE·HNSW·서명·큐·전체 감사\n'
