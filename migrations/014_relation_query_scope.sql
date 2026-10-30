-- #158: 관계 조회를 label·그래프·정체성으로 제한하고 사건 제안의 입력 적재를 보호한다.
-- 기존 적용 SQL·checksum은 보존한다. openCypher 번역과 같은 agtype 표현식을 사용한다.
DO $relation_indexes$
DECLARE
    edge_label text;
BEGIN
    FOREACH edge_label IN ARRAY ARRAY['PRECEDES', 'CAUSES', 'PART_OF', 'RELATES_TO'] LOOP
        EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I.%I (
            ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, ''"graph_id"''::ag_catalog.agtype]),
            ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, ''"relation_id"''::ag_catalog.agtype]))',
            lower(edge_label) || '_graph_relation_idx', '{{.GraphName}}', edge_label);
        EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I.%I (
            ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, ''"graph_id"''::ag_catalog.agtype]),
            ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, ''"from_context_id"''::ag_catalog.agtype]),
            ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, ''"to_context_id"''::ag_catalog.agtype]))',
            lower(edge_label) || '_graph_identity_idx', '{{.GraphName}}', edge_label);
        EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I.%I (
            ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, ''"graph_id"''::ag_catalog.agtype]),
            ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, ''"to_context_id"''::ag_catalog.agtype]))',
            lower(edge_label) || '_graph_target_idx', '{{.GraphName}}', edge_label);
        EXECUTE format('ANALYZE %I.%I', '{{.GraphName}}', edge_label);
    END LOOP;
END
$relation_indexes$;

CREATE INDEX IF NOT EXISTS context_graph_context_lookup_idx
    ON "{{.GraphName}}"."Context" ((properties ->> 'graph_id'::text),
        ((properties ->> 'context_id'::text) COLLATE "C"));

CREATE INDEX IF NOT EXISTS context_active_event_proposal_idx
    ON "{{.GraphName}}"."Context" ((properties ->> 'graph_id'::text),
        ((properties ->> 'context_id'::text) COLLATE "C") DESC)
    WHERE properties ->> 'deleted_at'::text IS NULL AND properties ->> 'layer'::text = 'event';

ANALYZE "{{.GraphName}}"."Context";
