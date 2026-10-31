#!/bin/sh
# 기존 로컬 이미지와 시험이 소유한 경로·볼륨만 사용한다. 네트워크나 DB를 시작하지 않는다.
set -eu
umask 077
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd -P)
cd "$repo_dir"
go_command=${GO_BINARY:-go}
image=${ACS_OBS08_IMAGE:-sha256:2b6d51048c03dae147064683f5b078690f1525365d561fd80b78f74de8c3c697}
architecture=$(docker image inspect "$image" --format '{{.Architecture}}')
image_os=$(docker image inspect "$image" --format '{{.Os}}')
test "$image_os" = linux
case "$architecture" in arm64|amd64) ;; *) exit 1 ;; esac
required_version=$(awk '$1 == "go" { print $2; exit }' go.mod)
test "$("$go_command" env GOVERSION)" = "go$required_version"

owned=$(mktemp -d "$repo_dir/.obs08-linux-XXXXXX")
results=$(mktemp -d "${TMPDIR:-/tmp}/acs-obs08-results-XXXXXX")
volume="acs-obs08-$(date +%s)-$$"
volume_created=false
cleanup() {
    status=$?
    trap - EXIT
    for phase in init module throughput readonly first restart replacement; do
        docker container rm -f "$volume-$phase" >/dev/null 2>&1 || true
    done
    if "$volume_created"; then docker volume rm "$volume" >/dev/null || status=1; fi
    rm -rf -- "$owned"
    printf 'OBS-08 Linux 결과: %s\n' "$results"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" "$go_command" test -c -o "$owned/diagnostic.test" ./internal/diagnostic
chmod 755 "$owned/diagnostic.test"
mkdir "$owned/readonly"
chmod 700 "$owned/readonly"
test_uid=$(id -u)
test_gid=$(id -g)

docker volume create --label agent-context.test=obs08 "$volume" >"$results/volume.txt"
volume_created=true
docker run --rm --name "$volume-init" --label agent-context.test=obs08 --pull never --network none --read-only --user 0:0 --cap-drop ALL --cap-add CHOWN \
    --security-opt no-new-privileges --mount "type=volume,src=$volume,dst=/evidence" \
    --entrypoint /bin/sh "$image" -ec 'mkdir /evidence/owned; chmod 700 /evidence/owned; chown "$1:$2" /evidence/owned' sh "$test_uid" "$test_gid"

run_phase() {
    phase=$1
    shift
    code=0
    "$@" >"$results/$phase.txt" 2>"$results/$phase.stderr" || code=$?
    "$go_command" tool test2json -p agent_context_sharing/internal/diagnostic -t <"$results/$phase.txt" >"$results/$phase.json"
    if test "$code" -ne 0; then cat "$results/$phase.txt" "$results/$phase.stderr"; exit "$code"; fi
}

run_phase module docker run --rm --name "$volume-module" --label agent-context.test=obs08 --pull never --network none --read-only \
    --user "$test_uid:$test_gid" --cap-drop ALL --security-opt no-new-privileges \
    --mount "type=volume,src=$volume,dst=/evidence" \
    --mount "type=bind,src=$owned/diagnostic.test,dst=/tests/diagnostic.test,readonly" \
    --tmpfs "/volatile:rw,nosuid,nodev,noexec,size=1m,uid=$test_uid,gid=$test_gid,mode=0700" \
    -e ACS_OBS02_TEST_ROOT=/evidence/owned -e ACS_OBS02_VOLATILE_ROOT=/volatile \
    --entrypoint /tests/diagnostic.test "$image" -test.v=test2json -test.timeout=180s \
    '-test.skip=^(TestOBS02ReadOnlyMount|TestOBS08ContainerPersistence|TestDiagnostic(Saturated)?ThroughputLinux)$'

run_phase throughput docker run --rm --name "$volume-throughput" --label agent-context.test=obs08 --pull never --network none --read-only \
    --user "$test_uid:$test_gid" --cap-drop ALL --security-opt no-new-privileges \
    --mount "type=volume,src=$volume,dst=/evidence" \
    --mount "type=bind,src=$owned/diagnostic.test,dst=/tests/diagnostic.test,readonly" \
    -e ACS_OBS02_TEST_ROOT=/evidence/owned --entrypoint /tests/diagnostic.test "$image" \
    -test.v=test2json -test.timeout=30s '-test.run=^TestDiagnostic(Saturated)?ThroughputLinux$'
cat "$results/throughput.txt"

run_phase readonly docker run --rm --name "$volume-readonly" --label agent-context.test=obs08 --pull never --network none --read-only --user 0:0 \
    --cap-drop ALL --security-opt no-new-privileges \
    --mount "type=bind,src=$owned/readonly,dst=/readonly,readonly" \
    --mount "type=bind,src=$owned/diagnostic.test,dst=/tests/diagnostic.test,readonly" \
    -e ACS_OBS02_READONLY_ROOT=/readonly --entrypoint /tests/diagnostic.test "$image" \
    -test.v=test2json -test.timeout=20s '-test.run=^TestOBS02ReadOnlyMount$'

for phase in first restart replacement; do
    run_phase "$phase" docker run --rm --name "$volume-$phase" --label agent-context.test=obs08 --pull never --network none --read-only \
        --user "$test_uid:$test_gid" --cap-drop ALL --security-opt no-new-privileges \
        --mount "type=volume,src=$volume,dst=/evidence" \
        --mount "type=bind,src=$owned/diagnostic.test,dst=/tests/diagnostic.test,readonly" \
        -e ACS_OBS08_PERSIST_ROOT=/evidence/owned/logs -e "ACS_OBS08_PHASE=$phase" \
        --entrypoint /tests/diagnostic.test "$image" -test.v=test2json -test.timeout=20s \
        '-test.run=^TestOBS08ContainerPersistence$'
done
printf 'Linux 기록기 전체·읽기 전용 마운트·컨테이너 재생성·슬롯 교체 통과\n'
