#!/bin/sh
# 독립 Compose 프로젝트의 합성 데이터만 생성·삭제한다. 기존 개발 DB는 사용하지 않는다.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/wal-archive-test.XXXXXX")
test_project=$(basename "$test_dir" | tr '[:upper:].' '[:lower:]-')
legacy_container="${test_project}-legacy"
WAL_ARCHIVE_RETENTION_DAYS=7
export WAL_ARCHIVE_RETENTION_DAYS

compose() {
    docker compose --env-file /dev/null --project-name "$test_project" \
        -f "$repo_root/compose.dev.yaml" -f "$repo_root/compose.wal-test.yaml" "$@"
}

cleanup() {
    result=$?
    trap - EXIT
    if [ "$result" -ne 0 ]; then
        compose logs --no-color db wal_cleanup >&2 || true
        docker logs "$legacy_container" >&2 2>/dev/null || true
    fi
    # 이 실행에서 생성한 자원만 정리하며 기존 프로젝트의 down은 실행하지 않는다.
    docker rm -f "$legacy_container" >/dev/null 2>&1 || true
    compose down --volumes --rmi local >/dev/null || result=1
    rmdir "$test_dir" || result=1
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
    printf '%s\n' "$*" >&2
    exit 1
}

sql() {
    docker exec "$db_container" psql -U wal_test -d wal_test -X -At -v ON_ERROR_STOP=1 -c "$1"
}

pending() {
    sql "SELECT count(*) FROM pg_ls_dir('pg_wal/archive_status') AS name WHERE name LIKE '%.ready'"
}

wait_archived() {
    attempts=0
    while [ "$(pending)" -ne 0 ]; do
        attempts=$((attempts + 1))
        [ "$attempts" -lt 90 ] || fail 'WAL 보관 대기 시간이 초과됐다.'
        sleep 1
    done
}

check_directory() {
    actual=$(docker exec "$db_container" stat -c '%U:%G:%a' /var/lib/postgresql/wal_archive)
    [ "$actual" = 'postgres:postgres:700' ] || fail "보관 경로 소유권·권한 불일치: $actual"
}

compose up --build -d --wait --wait-timeout 120 db
db_container=$(compose ps -q db)
image_id=$(docker inspect "$db_container" --format '{{.Image}}')
image_cmd=$(docker image inspect "$image_id" --format '{{json .Config.Cmd}}')
[ "$image_cmd" = '["postgres","-c","shared_preload_libraries=age"]' ] || fail 'AGE의 기본 기동 명령이 보존되지 않았다.'
check_directory
sql 'CREATE TABLE wal_probe (id integer PRIMARY KEY); INSERT INTO wal_probe VALUES (1)' >/dev/null
sql 'SELECT pg_switch_wal()' >/dev/null
wait_archived
fresh_archived=$(sql 'SELECT archived_count FROM pg_stat_archiver')
fresh_failed=$(sql 'SELECT failed_count FROM pg_stat_archiver')
[ "$fresh_archived" -gt 0 ] || fail '새 볼륨의 WAL 보관본이 없다.'
[ "$fresh_failed" -eq 0 ] || fail '새 볼륨에서 WAL 보관에 실패했다.'
printf 'fresh: archived=%s failed=%s owner=postgres mode=700\n' "$fresh_archived" "$fresh_failed"

# 기존 볼륨을 연결하지 않은 별도 컨테이너에서 준비 실패 시 기동 차단을 확인한다.
if failure_output=$(docker run --rm --pull=never --entrypoint /bin/sh "$image_id" -eu -c \
    'touch /var/lib/postgresql/wal_archive; exec /usr/local/bin/agent-context-db-entrypoint.sh postgres' 2>&1); then
    fail '보관 경로 준비 실패에도 기동 래퍼가 성공했다.'
fi
case "$failure_output" in
    *'install:'*'/var/lib/postgresql/wal_archive'*) ;;
    *) fail "예상하지 못한 준비 실패: $failure_output" ;;
esac

# 시험 DB에서만 WAL 크기 목표를 낮춰 적은 합성 쓰기로 재활용을 검증한다.
sql "ALTER SYSTEM SET min_wal_size='32MB'" >/dev/null
sql "ALTER SYSTEM SET max_wal_size='64MB'" >/dev/null
sql 'SELECT pg_reload_conf()' >/dev/null
compose stop db

# 이 실행의 시험 볼륨에서만 보관 경로를 없애 기존 결함을 재현한다.
compose run --rm --no-deps --user root --entrypoint /bin/sh db -eu -c \
    'find /var/lib/postgresql/wal_archive -type f -delete; rmdir /var/lib/postgresql/wal_archive'
compose run -d --rm --no-deps --name "$legacy_container" --entrypoint docker-entrypoint.sh db \
    postgres -c shared_preload_libraries=age -c archive_mode=on \
    -c 'archive_command=test ! -f /var/lib/postgresql/wal_archive/%f && cp %p /var/lib/postgresql/wal_archive/%f' >/dev/null
db_container=$legacy_container
attempts=0
until docker exec "$db_container" pg_isready -U wal_test -d wal_test >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    [ "$attempts" -lt 60 ] || fail '결함 재현 DB가 준비되지 않았다.'
    sleep 1
done
id=2
while [ "$id" -le 12 ]; do
    sql "INSERT INTO wal_probe VALUES ($id)" >/dev/null
    sql 'SELECT pg_switch_wal()' >/dev/null
    id=$((id + 1))
done
attempts=0
while [ "$(sql 'SELECT failed_count FROM pg_stat_archiver')" -eq 0 ]; do
    attempts=$((attempts + 1))
    [ "$attempts" -lt 60 ] || fail '보관 경로 누락으로 인한 실패가 재현되지 않았다.'
    sleep 1
done
legacy_failed=$(sql 'SELECT failed_count FROM pg_stat_archiver')
legacy_pending=$(pending)
legacy_bytes=$(sql 'SELECT sum(size) FROM pg_ls_waldir()')
[ "$legacy_pending" -gt 0 ] || fail '결함 재현 DB에 보관 대기 WAL이 없다.'
printf 'legacy: failed=%s pending=%s wal_bytes=%s\n' "$legacy_failed" "$legacy_pending" "$legacy_bytes"
docker stop "$legacy_container" >/dev/null

# 같은 볼륨의 재기동: 초기화가 생략돼도 경로 복구·원본 보존·보관 재개가 성립해야 한다.
compose up -d --wait --wait-timeout 120 db
db_container=$(compose ps -q db)
check_directory
[ "$(sql 'SELECT count(*) FROM wal_probe')" -eq 12 ] || fail '기존 데이터가 보존되지 않았다.'
# 결함 재현 컨테이너 종료 중에도 실패가 늘 수 있으므로 수정 경로 재기동 뒤의 값을
# 기준으로 삼는다. 정상 db 서비스 로그에는 보관 실패가 한 번도 없어야 한다.
restart_failed=$(sql 'SELECT failed_count FROM pg_stat_archiver')
wait_archived
sql 'INSERT INTO wal_probe VALUES (13); SELECT pg_switch_wal()' >/dev/null
wait_archived
recovered_archived=$(sql 'SELECT archived_count FROM pg_stat_archiver')
recovered_failed=$(sql 'SELECT failed_count FROM pg_stat_archiver')
[ "$recovered_archived" -gt "$fresh_archived" ] || fail '기존 볼륨의 보관이 재개되지 않았다.'
[ "$recovered_failed" -eq "$restart_failed" ] || fail '복구 뒤 WAL 보관 실패가 증가했다.'
if compose logs --no-color db | grep -q 'archive command failed'; then
    fail '수정된 기동 경로에서 WAL 보관에 실패했다.'
fi
sql 'CHECKPOINT' >/dev/null
sql 'CHECKPOINT' >/dev/null
recovered_bytes=$(sql 'SELECT sum(size) FROM pg_ls_waldir()')
[ "$recovered_bytes" -lt "$legacy_bytes" ] || fail '보관 완료·체크포인트 뒤 WAL이 줄지 않았다.'
printf 'recovered: archived=%s failed=%s new_failures=0 pending=0 wal_bytes=%s rows=13\n' \
    "$recovered_archived" "$recovered_failed" "$recovered_bytes"

# 기존 디렉터리의 소유권도 바로잡고 보관 파일은 지우지 않는지 확인한다.
preserved_wal=$(sql 'SELECT last_archived_wal FROM pg_stat_archiver')
compose stop db
compose run --rm --no-deps --user root --entrypoint /bin/sh db -eu -c \
    'chown root:root /var/lib/postgresql/wal_archive; chmod 0755 /var/lib/postgresql/wal_archive'
compose up -d --wait --wait-timeout 120 db
db_container=$(compose ps -q db)
check_directory
docker exec "$db_container" test -f "/var/lib/postgresql/wal_archive/$preserved_wal"
compose run --rm --no-deps --user postgres db postgres --version >/dev/null
docker exec --user postgres "$db_container" /bin/sh -eu -c \
    'touch /var/lib/postgresql/wal_archive/test-recent; touch -d "10 days ago" /var/lib/postgresql/wal_archive/test-old'
compose up --build -d --wait --wait-timeout 120 wal_cleanup
attempts=0
while docker exec "$db_container" test -e /var/lib/postgresql/wal_archive/test-old; do
    attempts=$((attempts + 1))
    [ "$attempts" -lt 30 ] || fail 'wal_cleanup이 오래된 합성 보관본을 정리하지 않았다.'
    sleep 1
done
docker exec "$db_container" test -f /var/lib/postgresql/wal_archive/test-recent
[ "$(compose ps --status running -q wal_cleanup)" != '' ] || fail 'wal_cleanup이 실행 중이 아니다.'
cleanup_logs=$(compose logs --no-color wal_cleanup)
[ -z "$cleanup_logs" ] || fail "wal_cleanup의 예상하지 못한 로그: $cleanup_logs"
[ "$(sql 'SELECT count(*) FROM wal_probe')" -eq 13 ] || fail '재기동 뒤 기존 데이터가 보존되지 않았다.'
printf 'cleanup: expired_removed=true recent_preserved=true errors=0\n'
printf 'PASS: 새 볼륨·기존 볼륨·보관 재개·WAL 재활용·보관본 정리\n'
