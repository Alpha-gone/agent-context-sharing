-- #270: 전체 감사가 다른 그래프의 간선을 반복해서 훑지 않도록 SQL 소속 조건을 지원한다.
-- 기존 적용 SQL과 openCypher 인덱스는 보존한다. 끝점 인덱스는 기존 AGE/복합 인덱스를 쓴다.
DO $audit_indexes$
DECLARE
    edge_label text;
BEGIN
    FOREACH edge_label IN ARRAY ARRAY['DERIVED_FROM', 'SUPERSEDES', 'HAS_MEMBER',
                                     'PRECEDES', 'CAUSES', 'PART_OF', 'RELATES_TO'] LOOP
        EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I.%I ((properties ->> ''graph_id''::text))',
            lower(edge_label) || '_graph_id_text_idx', '{{.GraphName}}', edge_label);
        EXECUTE format('ANALYZE %I.%I', '{{.GraphName}}', edge_label);
    END LOOP;
END
$audit_indexes$;
