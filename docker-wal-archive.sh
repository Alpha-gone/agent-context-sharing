#!/bin/sh
# PostgreSQL archive_command. 사용: agent-context-wal-archive %p %f
#
# 「Continuous Archiving」의 계약을 따른다. 같은 이름의 보관본이 이미 있으면 내용이 같을
# 때만 성공하고 다르면 실패한다. 복사 뒤 .done 기록 전에 중단된 WAL을 다시 보관할 때
# 영구 실패하지 않게 하기 위해서다. 새 보관본은 임시 이름에 복사·fsync한 뒤 기존 파일을
# 덮어쓰지 않는 하드 링크로 공개하므로 부분 파일이 보관본 이름으로 남지 않는다.
set -eu

if [ "$#" -ne 2 ]; then
    echo "agent-context-wal-archive: 사용법: agent-context-wal-archive %p %f" >&2
    exit 2
fi
source=$1
name=$2
case "$name" in
    '' | .* | */*)
        echo "agent-context-wal-archive: 잘못된 WAL 이름" >&2
        exit 2
        ;;
esac
archive=/var/lib/postgresql/wal_archive
target="$archive/$name"
temporary="$archive/.$name.partial"

# matches는 기존 보관본이 원본과 같으면 영속화를 확인한 뒤 성공하고, 다르면 실패한다.
matches() {
    if cmp -s -- "$source" "$target"; then
        sync -- "$target" "$archive"
        return 0
    fi
    echo "agent-context-wal-archive: 같은 이름의 보관본 내용이 다르다: $name" >&2
    return 1
}

if [ -e "$target" ]; then
    matches
    exit
fi

# 같은 WAL의 이전 시도가 남긴 임시 파일은 단일 archiver만 쓰므로 지워도 안전하다.
rm -f -- "$temporary"
cp -- "$source" "$temporary"
sync -- "$temporary"
if ! ln -- "$temporary" "$target" 2>/dev/null; then
    rm -f -- "$temporary"
    if [ -e "$target" ]; then
        matches
        exit
    fi
    echo "agent-context-wal-archive: 보관본을 공개하지 못했다: $name" >&2
    exit 1
fi
rm -f -- "$temporary"
sync -- "$archive"
