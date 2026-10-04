# 개발용 PostgreSQL 이미지. Apache AGE와 pgvector를 함께 담는다.
#
# 공식 이미지에는 둘이 함께 들어 있는 것이 없다. apache/age 이미지는 postgres:18 위에
# AGE만 얹으므로 pgvector를 PGDG 패키지로 더한다. AGE는 베이스에 이미 컴파일되어 있어
# 빌드 단계가 필요 없다.
#
# 태그를 release_PG18_1.8.0으로 고정하는 이유는 SDD.md의 「버전 요구」가 Apache AGE
# 1.8.0 이상을 요구하는데 공식 이미지가 PostgreSQL 18에만 그 판을 올려 두었기 때문이다.
# PostgreSQL 18은 「버전 요구」가 정한 13~18 범위의 상한이다.
#
# 태그와 함께 digest를 고정한다. release 태그도 다시 게시되어 가리키는 이미지가 바뀔 수
# 있고, 홉 탐색 구현 비교처럼 이미지에 따라 결과가 달라지는 측정은 같은 이미지에서
# 재현되어야 하기 때문이다. digest는 2026-09-27에 레지스트리에서 확인한 이 태그의 다중
# 아키텍처 인덱스이며, 판을 올릴 때 태그와 digest를 함께 바꾼다.
FROM apache/age:release_PG18_1.8.0@sha256:47a0b054c3663a3e64a25fc0c5a982f1b58348019274b326a691cdf77f78eb58

# pgvector 0.8.6. SDD.md의 「버전 요구」가 요구하는 0.7.0 이상을 만족하며
# halfvec과 이진 양자화를 포함한다. PGDG 패키지의 전체 버전을 고정하여 재빌드 때
# 최신 판으로 바뀌지 않도록 하며, 이 버전이 없으면 빌드를 실패시킨다.
RUN apt-get update \
    && apt-get install -y --no-install-recommends postgresql-18-pgvector=0.8.6-1.pgdg13+1 \
    && rm -rf /var/lib/apt/lists/*

# 이미지 빌드 시 만든 디렉터리는 볼륨 마운트로 가려지고 최초 초기화 스크립트는
# 기존 DB에서 실행되지 않는다. 매번 볼륨 마운트 뒤 준비한 다음 공식 entrypoint에
# 위임한다. ENTRYPOINT를 바꾸면 부모 CMD가 초기화되므로 AGE의 CMD도 명시한다.
COPY --chmod=0755 docker-db-entrypoint.sh /usr/local/bin/agent-context-db-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/agent-context-db-entrypoint.sh"]
CMD ["postgres", "-c", "shared_preload_libraries=age"]
