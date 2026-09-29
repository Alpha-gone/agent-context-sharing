#!/bin/sh
set -eu

go_bin=${GO_BIN:-go}
client_root=$(CDPATH= cd "$(dirname "$0")/../.." && pwd)
server_root=$(CDPATH= cd "$client_root/.." && pwd)

if [ -z "${TEST_DATABASE_URL:-}" ]; then
	printf '%s\n' '실제 서버 시험에는 TEST_DATABASE_URL이 필요합니다.' >&2
	exit 2
fi

case "$("$go_bin" version)" in
	*'go1.27.1 '*) ;;
	*)
		printf '%s\n' '실제 서버 시험에는 Go 1.27.1이 필요합니다.' >&2
		exit 2
		;;
esac

live_tests=$(cd "$client_root" && "$go_bin" test -tags=client_live -list '^TestClientLive' ./internal/client/...)
if ! printf '%s\n' "$live_tests" | grep -q '^TestClientLive'; then
	printf '%s\n' '실제 서버용 TestClientLive* 시험이 없어 통과로 판정할 수 없습니다.' >&2
	exit 2
fi

result_file=$(mktemp)
trap 'rm -f "$result_file"' EXIT
if ! (cd "$client_root" && TEST_DATABASE_REQUIRED=1 "$go_bin" test -json -tags=client_live -count=1 ./internal/client/...) >"$result_file" 2>&1; then
	cat "$result_file" >&2
	exit 1
fi
if ! (cd "$server_root" && TEST_DATABASE_REQUIRED=1 "$go_bin" test -json -count=1 ./internal/mcp) >>"$result_file" 2>&1; then
	cat "$result_file" >&2
	exit 1
fi
if grep -q '"Action":"skip"' "$result_file"; then
	cat "$result_file" >&2
	printf '%s\n' '건너뛴 실제 서버 시험이 있어 통과로 판정할 수 없습니다.' >&2
	exit 2
fi
printf '%s\n' '실제 서버 시험이 건너뜀 없이 통과했습니다.'
