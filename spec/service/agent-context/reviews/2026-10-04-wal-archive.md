# 개발 DB WAL 보관 경로 복구 검증

- 날짜: 2026-10-04
- 범위: [#45](https://github.com/Alpha-gone/agent-context-sharing/issues/45)의 개발 구성 수정·격리 Compose 회귀 시험과 이후 사용자 요청에 따른 기존 개발 DB 재기동
- 계약: [SDD](../SDD.md)의 「백업과 복구」와 [개발 계획](../DEVELOPMENT_PLAN.md)의 9단계 보완

## 원인과 변경

`archive_command`와 `wal_cleanup`은 `/var/lib/postgresql/wal_archive`를 사용하지만 기동 전에 경로를 만드는 단계가 없었다. 초기 검증에서 기존 개발 DB를 읽기 전용으로 확인한 결과 보관 경로가 없고 `archived_count=0`, `failed_count=8403`이었다. 당시 이 DB의 서비스·데이터·볼륨은 변경하지 않았으며 이후 승인된 재기동 결과는 아래에 분리한다.

- [기동 래퍼](../../../../docker-db-entrypoint.sh)가 볼륨 마운트 뒤 매번 보관 경로를 준비한다. root 실행에서는 `postgres:postgres` 소유와 `0700` 권한을 설정하고 postgres 직접 실행에서는 경로 생성·권한 설정을 수행한다. 준비 실패는 DB 기동 전에 중단한다.
- DB 데이터나 기존 WAL 파일을 삭제하거나 재귀적으로 소유권을 바꾸지 않는다. 공식 PostgreSQL entrypoint에 `exec`로 위임해 초기화·권한 강하·신호 전달을 보존한다.
- Dockerfile에서 새 entrypoint와 기존 AGE의 기본 CMD를 함께 명시한다. ENTRYPOINT 변경 시 부모 CMD가 초기화되는 Docker 동작을 고려했다.
- [회귀 시험](../../../../test-wal-archive.sh)은 [시험 구성](../../../../compose.wal-test.yaml)으로 새 이름의 Compose 프로젝트를 만든다. 개발 `.env`·기존 프로젝트·공개 포트·Ollama·외부 API를 사용하지 않는다. 시험 종료 시 이 실행의 컨테이너·볼륨·이미지만 제거한다.

## 실제 검증 결과

Docker Compose v5.4.0, 로컬 ARM64 Docker의 고정 AGE 이미지와 PostgreSQL 18.6으로 실행했다. 기존 환경의 볼륨을 지우는 대신 독립 프로젝트의 새 볼륨에서 시작했다.

```shell
sh -n docker-db-entrypoint.sh test-wal-archive.sh
docker compose --env-file /dev/null -f compose.yaml -f compose.wal-test.yaml config --quiet
sh test-wal-archive.sh
```

최종 실행과 그 직전 회귀는 모두 통과했다. 최종 실행 결과는 다음과 같다.

- 새 볼륨: `postgres:postgres:700`, `archived_count=2`, `failed_count=0`.
- 준비 실패: 별도 일회성 컨테이너에서 디렉터리 자리에 파일을 두면 래퍼가 오류로 중단했고 PostgreSQL을 시작하지 않았다.
- 기존 볼륨 결함 재현: 정상 종료한 시험 DB의 보관 경로를 제거하고 원래 entrypoint로 실행했다. 보관 대기 11개, 측정 당시 `failed_count=2`, WAL 201326592바이트(192MiB)를 확인했다.
- 수정 기동 경로로 같은 볼륨 재기동: 초기화가 생략돼도 경로가 복구됐고 기존 12행이 보존됐다. 보관이 재개된 뒤 추가 쓰기도 성공해 13행이 됐다.
- 복구 뒤: `archived_count=15`, 보관 대기 0개, 수정 기동 이후 새 실패 0회. 누적 `failed_count=6`은 결함 재현 컨테이너의 종료 중 발생한 실패까지 포함하며 복구 후에도 초기화하지 않는다. 정상 db 서비스 로그에도 `archive command failed`가 없었다.
- 두 번의 체크포인트 뒤 WAL 67108864바이트(64MiB). 재활용 확인을 위해 시험 DB에만 `min_wal_size=32MB`, `max_wal_size=64MB`를 설정했다. 개발 기본값이나 운영 공간 상한의 측정 결과가 아니다.
- 재기동의 멱등성: 디렉터리만 root 소유·0755로 바꾼 뒤 다시 기동해 `postgres:postgres:700` 복원과 기존 WAL 파일 보존을 확인했다. postgres 사용자 직접 실행 경로도 통과했다.
- `wal_cleanup`: 10일 된 합성 파일은 삭제하고 최근 파일은 보존했다. 서비스가 실행 중이고 오류 로그가 없었다.
- 시험 자원 정리: 시험 프로젝트의 컨테이너·볼륨·이미지를 제거했다. 제거된 자료는 합성 자료이며 같은 시험으로 다시 생성할 수 있다. 기존 개발 DB의 컨테이너 ID와 시작 시각은 작업 전후 같았다.

Go 1.27.1로 서버·클라이언트의 `go build ./...`, `go vet ./...`와 루트 `gofmt -l .`을 확인했다. 모두 통과했다. Go 코드·의존성·SQL 마이그레이션은 변경하지 않았으며 Go 전체 테스트·과거 시점 복구·실제 운영 환경 시험은 재실행하지 않았다. ShellCheck는 로컬에 없어 실행하지 않았으며 쉘 문법과 실제 Docker 회귀로 검증했다.

## 기존 개발 환경 적용 절차

기존 DB의 백업·복구 경로를 확인하고 DB 컨테이너 재생성을 허용한 중단 시간에 저장소 루트에서 실행한다.

```shell
docker compose up -d --build --wait db wal_cleanup
```

컨테이너는 재생성될 수 있지만 기존 `db_data` 볼륨은 유지한다. 이 결함을 수정하려고 기존 프로젝트에서 `down -v`를 실행하거나 `pg_wal` 파일을 수동 삭제하지 않는다.

기동 후 경로의 소유권·권한과 다음 값을 확인한다. 보관이 끝난 뒤 체크포인트가 진행돼야 WAL이 재활용되며, 일부 공간은 재사용용으로 남으므로 디렉터리가 비는 것을 합격 기준으로 삼지 않는다.

```sql
SELECT archived_count, failed_count, last_archived_wal, last_failed_wal
FROM pg_stat_archiver;
SELECT pg_switch_wal();
SELECT pg_size_pretty(sum(size)) FROM pg_ls_waldir();
```

과거 실패 횟수가 0이 되는 것이 아니라 새 실패가 늘지 않고 보관 횟수가 증가하는지 확인한다. `wal_cleanup` 로그에도 경로 누락·권한 오류가 없어야 한다. 이 절차는 개발 구성용이며 운영 배포나 운영 백업 보존 정책은 변경하지 않는다.

## DOX와 제한

루트→spec→service→agent-context의 DOX 체인을 확인했다. 루트 설정 파일과 새 기동·시험 스크립트의 책임은 루트 AGENTS에 반영하고 SDD·개발 계획·요청 기록·공식 근거를 동기화했다. 서비스 경계·요구사항·Child DOX Index가 바뀌지 않아 하위 AGENTS와 SRS는 변경하지 않았다. 한국어 표현과 맞춤법, 상대 링크와 diff를 확인했다.

코드 및 격리 회귀와 실제 기존 개발 DB 적용 결과를 구분한다. 아래 사용자 요청에 따른 개발 환경 적용까지 확인했으며 커밋·푸시·PR 생성·GitHub 이슈 종료와 운영 적용은 하지 않았다. 재기동은 기존 기동 계약의 적용이므로 AGENTS·SDD·SRS를 추가 변경하지 않았다.

## 기존 개발 DB 적용 결과

2026-10-04 10:26~10:29 KST, 사용자가 개발 DB 재기동을 요청해 수정 이미지를 빌드하고 `db`와 `wal_cleanup`만 재생성했다. `docker compose up -d --build --wait --wait-timeout 120 db wal_cleanup`이 정상 완료됐고 DB는 healthy, 정리 서비스는 실행 중이었다. Ollama는 재기동하지 않았다.

- 재기동 직전: `archived_count=0`, `failed_count=8430`, 보관 대기 87개, `pg_wal` 1493172224바이트(1424MiB).
- `agent_context_sharing_db_data` 볼륨과 DB 클러스터 식별자는 전후 같았다. 데이터베이스 수 6개와 현재 DB의 시스템 스키마 제외 테이블 수 30개도 유지됐고 로그에서 기존 DB 초기화 생략을 확인했다. 테이블 행 전수 비교나 백업·시점 복구를 수행한 것은 아니다.
- 보관 경로는 `postgres:postgres:700`으로 준비됐다. 쌓인 WAL의 보관이 재개돼 초기 확인에서 88개가 보관됐고 대기는 0개였다.
- `pg_switch_wal()`과 보관 대기 완료 확인, 두 번의 `CHECKPOINT` 뒤 `archived_count=89`, `failed_count=0`, 대기 0개를 확인했다. 이후 재조회에서도 실패가 늘지 않았다. 통계 초기화 명령을 별도로 실행하지 않았으며 과거 카운터와 재기동 후 값은 같은 관측 구간으로 비교하지 않는다.
- `pg_wal`은 83886080바이트(80MiB)로 줄었다. 체크포인트 로그는 85개 제거·4개 재활용을 기록했고 WAL 보관 디렉터리에는 89개 파일이 남았다. 파일을 수동 삭제한 것이 아니며 전체 디스크에서 보관본 공간까지 없어진 것은 아니다.
- `wal_cleanup`의 새 로그에 경로·권한 오류가 없었다. 이 적용에서는 정리 시험용 파일이나 사용자 테이블을 새로 만들지 않았다.

기존 볼륨 삭제·DB 초기화·마이그레이션·구성 변경·외부 API 호출은 수행하지 않았다. 개발 환경의 WAL 보관 장애는 실제 적용에서도 해결됐으며 운영 환경의 적용을 뜻하지 않는다.
