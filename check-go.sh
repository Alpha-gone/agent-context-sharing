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
unformatted=$("$sdk_root/bin/gofmt" -l .)
if [ -n "$unformatted" ]; then
    printf 'gofmt이 필요한 파일:\n%s\n' "$unformatted" >&2
    exit 1
fi
printf '의존성 빌드·go build·go vet·gofmt 검사 통과\n'
