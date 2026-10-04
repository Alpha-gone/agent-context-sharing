#!/bin/sh
set -eu

# 최초 초기화와 기존 볼륨 재기동 모두에서 WAL 보관 경로를 준비한다.
# DB 데이터나 보관 파일의 소유권은 재귀적으로 바꾸지 않는다.
if [ "$(id -u)" = 0 ]; then
    install -d -m 0700 -o postgres -g postgres /var/lib/postgresql/wal_archive
else
    mkdir -p /var/lib/postgresql/wal_archive
    chmod 0700 /var/lib/postgresql/wal_archive
fi

# 공식 이미지의 초기화·권한 강하·신호 전달 동작과 AGE의 CMD를 보존한다.
exec /usr/local/bin/docker-entrypoint.sh "$@"
