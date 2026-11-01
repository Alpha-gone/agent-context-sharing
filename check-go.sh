#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
cd "$repo_dir"

go_command=${GO_BINARY:-go}
export GOTOOLCHAIN=local GOWORK=off
required_version=$(awk '$1 == "go" { print $2; exit }' go.mod)
actual_version=$("$go_command" env GOVERSION)
if [ "$actual_version" != "go$required_version" ]; then
    printf 'Go %s이 필요합니다. 선택된 SDK: %s\n' "$required_version" "$actual_version" >&2
    exit 1
fi
"$go_command" version
"$go_command" mod download
"$go_command" mod verify

# 업무 구현과 별도로 준비한 SDK까지 실제로 링크한다. 외부 연결은 시작하지 않는다.
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/agent-context-arch01.XXXXXX")
trap 'rm -rf -- "$build_dir"' EXIT
trap 'exit 1' HUP INT TERM
cat > "$build_dir/main.go" <<'GO'
package main

import (
	_ "github.com/go-jose/go-jose/v4"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {}
GO
"$go_command" build -mod=readonly -o "$build_dir/dependencies" "$build_dir/main.go"
"$go_command" build -mod=readonly ./...
"$go_command" vet -mod=readonly ./...

sdk_root=$("$go_command" env GOROOT)
# govulncheck도 GO_BINARY로 선택한 도구 체인을 사용한다.
PATH="$sdk_root/bin:$PATH"
export PATH
unformatted=$("$sdk_root/bin/gofmt" -l .)
if [ -n "$unformatted" ]; then
    printf 'gofmt이 필요한 파일:\n%s\n' "$unformatted" >&2
    exit 1
fi
# 도구는 애플리케이션 의존성에 추가하지 않고 정확한 판으로 실행한다.
# 기본 symbol 분석은 호출 경로상 취약점 발견 시 실패한다. JSON 모드는 종료 판정이 다르므로 쓰지 않는다.
"$go_command" run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
printf '의존성 빌드·go build·go vet·gofmt·govulncheck 검사 통과\n'
