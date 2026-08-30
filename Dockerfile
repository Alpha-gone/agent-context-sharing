# 개발용 PostgreSQL 이미지. Apache AGE와 pgvector를 함께 담는다.
#
# 공식 이미지에는 둘이 함께 들어 있는 것이 없다. apache/age 이미지는 postgres:18 위에
# AGE만 얹으므로 pgvector를 PGDG 패키지로 더한다. AGE는 베이스에 이미 컴파일되어 있어
# 빌드 단계가 필요 없다.
#
# 태그를 release_PG18_1.8.0으로 고정하는 이유는 SDD.md의 「버전 요구」가 Apache AGE
# 1.8.0 이상을 요구하는데 공식 이미지가 PostgreSQL 18에만 그 판을 올려 두었기 때문이다.
# PostgreSQL 18은 「버전 요구」가 정한 13~18 범위의 상한이다.
FROM apache/age:release_PG18_1.8.0

# pgvector 0.8.6. SDD.md의 「버전 요구」가 요구하는 0.7.0 이상을 만족하며
# halfvec과 이진 양자화를 포함한다.
RUN apt-get update \
    && apt-get install -y --no-install-recommends postgresql-18-pgvector \
    && rm -rf /var/lib/apt/lists/*

# 베이스 이미지가 shared_preload_libraries=age를 CMD로 설정하므로 그대로 상속한다.
