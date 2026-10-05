#!/bin/sh
# MCP 클라이언트의 빌드·구성 파일 생성·진단과 호스트 등록 정보 출력을 묶는다.
# 구성 파일에는 토큰이나 키를 두지 않으며 값 검증은 클라이언트의 구성 검사가 맡는다.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary="$repo_root/out/agent-context-client"
env_file="$repo_root/out/client.env"

usage() {
    cat >&2 <<'EOF'
사용법:
  ./client-setup.sh init <원격 /mcp 주소> [클라이언트 ID]
  ./client-setup.sh doctor
  ./client-setup.sh host-config

init은 클라이언트를 빌드하고 out/client.env를 만든다. 클라이언트 ID 기본값은
agent-context이며, 기존 구성의 에이전트 식별자는 유지하고 없으면 UUIDv7을 새로 만든다.
제한 시간·도구 정책은 같은 이름의 AGENT_CONTEXT_CLIENT_* 환경 변수로 바꿀 수 있다.
EOF
    exit 2
}

build() {
    mkdir -p "$repo_root/out"
    (cd "$repo_root/client" && go build -o "$binary" ./cmd/client)
}

load_env() {
    if [ ! -f "$env_file" ]; then
        echo "구성 파일이 없습니다. 먼저 ./client-setup.sh init을 실행하십시오." >&2
        exit 1
    fi
    set -a
    . "$env_file"
    set +a
}

# 초 단위 시각과 /dev/urandom의 난수로 RFC 9562 UUIDv7을 만든다.
uuid7() {
    ts=$(printf '%012x' "$(($(date +%s) * 1000))")
    rand=$(od -An -N10 -tx1 /dev/urandom | tr -d ' \n')
    variant=$(printf '%x' $((0x8 | (0x$(echo "$rand" | cut -c4) & 0x3))))
    printf '%s-%s-7%s-%s%s-%s\n' \
        "$(echo "$ts" | cut -c1-8)" "$(echo "$ts" | cut -c9-12)" \
        "$(echo "$rand" | cut -c1-3)" "$variant" "$(echo "$rand" | cut -c5-7)" \
        "$(echo "$rand" | cut -c8-19)"
}

write_var() {
    case $2 in
    *"'"*)
        echo "$1 값에는 작은따옴표를 쓸 수 없습니다." >&2
        exit 1
        ;;
    esac
    printf "%s='%s'\n" "$1" "$2"
}

init() {
    [ $# -ge 1 ] && [ $# -le 2 ] || usage
    agent_id=${AGENT_CONTEXT_CLIENT_AGENT_ID:-}
    if [ -z "$agent_id" ] && [ -f "$env_file" ]; then
        agent_id=$(sed -n "s/^AGENT_CONTEXT_CLIENT_AGENT_ID='\(.*\)'$/\1/p" "$env_file")
    fi
    [ -n "$agent_id" ] || agent_id=$(uuid7)

    build
    {
        write_var AGENT_CONTEXT_CLIENT_REMOTE_URL "$1"
        write_var AGENT_CONTEXT_CLIENT_ID "${2:-agent-context}"
        write_var AGENT_CONTEXT_CLIENT_AGENT_ID "$agent_id"
        write_var AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT "${AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT:-5m}"
        write_var AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT "${AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT:-30s}"
        write_var AGENT_CONTEXT_CLIENT_TOOL_POLICY "${AGENT_CONTEXT_CLIENT_TOOL_POLICY:-all}"
        write_var AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST "${AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST:-}"
    } > "$env_file.tmp"
    mv "$env_file.tmp" "$env_file"
    echo "구성을 저장했습니다: $env_file (에이전트 식별자 $agent_id)" >&2
}

host_config() {
    load_env
    [ -x "$binary" ] || build
    jq -n --arg command "$binary" '{
        command: $command,
        args: ["serve"],
        env: $ENV | with_entries(select(.key | startswith("AGENT_CONTEXT_CLIENT_")))
    }'
}

[ $# -ge 1 ] || usage
command=$1
shift
case $command in
init) init "$@" ;;
doctor)
    [ $# -eq 0 ] || usage
    load_env
    build
    exec "$binary" doctor
    ;;
host-config)
    [ $# -eq 0 ] || usage
    host_config
    ;;
*) usage ;;
esac
