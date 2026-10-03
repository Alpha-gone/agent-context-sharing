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

server_tests=$(cd "$server_root" && "$go_bin" test -tags=client_live -list '^TestClientServiceLive$' ./internal/mcp)
if ! printf '%s\n' "$server_tests" | grep -q '^TestClientServiceLive$'; then
	printf '%s\n' '실제 서버 fixture 시험이 없어 통과로 판정할 수 없습니다.' >&2
	exit 2
fi

result_file=$(mktemp)
trap 'rm -f "$result_file"' EXIT
# 서버 fixture가 클라이언트 시험을 자식 프로세스로 실행한다. 별도 실행은 fixture 없이
# 실패해야 하므로 모의 시험이나 접속 정보만 있는 실행을 종단 간 통과로 보지 않는다.
if ! (cd "$server_root" && GO_BIN="$go_bin" TEST_DATABASE_REQUIRED=1 "$go_bin" test -json -race -tags=client_live -count=1 -run '^TestClientServiceLive$' ./internal/mcp) >"$result_file" 2>&1; then
	cat "$result_file" >&2
	exit 1
fi
if ! (cd "$server_root" && TEST_DATABASE_REQUIRED=1 "$go_bin" test -json -race -count=1 ./internal/mcp ./internal/authz ./cmd/server) >>"$result_file" 2>&1; then
	cat "$result_file" >&2
	exit 1
fi
if grep -q '"Action":"skip"' "$result_file"; then
	cat "$result_file" >&2
	printf '%s\n' '건너뛴 실제 서버 시험이 있어 통과로 판정할 수 없습니다.' >&2
	exit 2
fi
if ! grep '"Action":"pass"' "$result_file" | grep -q '"Test":"TestClientServiceLive"'; then
	printf '%s\n' '실제 서버 fixture의 통과 결과가 없습니다.' >&2
	exit 2
fi
grep '"Action":"pass"' "$result_file" | grep -v '"Test":' || true
printf '%s\n' '실제 서버·인가·클라이언트 종단 간 시험이 건너뜀 없이 통과했습니다.'
