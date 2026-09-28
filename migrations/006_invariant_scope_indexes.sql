-- 006_invariant_scope_indexes: 그래프 불변식 쓰기 검사가 영향 주변부만 읽게 인덱스를 더한다.
--
-- 쓰기 검사는 openCypher로 읽고, AGE는 속성 접근 n.key를
-- agtype_access_operator(VARIADIC ARRAY[properties, '"key"'::agtype])로 번역한다. 001의
-- ->> 표현식 인덱스는 이 식과 달라 쓰이지 않으므로, 인덱스가 없으면 쓰기 한 번마다 모든
-- 그래프의 간선과 Context 정점 전체를 해시 조인한다. 근거는 SDD.md의 「인덱스」다.

-- 영향 주변부 정점을 context_id로 찾는다. 간선은 label 테이블의 start_id·end_id 인덱스로
-- 이어진다.
CREATE INDEX IF NOT EXISTS context_context_id_agtype_idx
    ON "{{.GraphName}}"."Context" (
        ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, '"context_id"'::ag_catalog.agtype]));

-- 순환 판정이 그래프의 확정 관계를 읽는 세 label이다.
CREATE INDEX IF NOT EXISTS precedes_graph_id_agtype_idx
    ON "{{.GraphName}}"."PRECEDES" (
        ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, '"graph_id"'::ag_catalog.agtype]));

CREATE INDEX IF NOT EXISTS causes_graph_id_agtype_idx
    ON "{{.GraphName}}"."CAUSES" (
        ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, '"graph_id"'::ag_catalog.agtype]));

CREATE INDEX IF NOT EXISTS part_of_graph_id_agtype_idx
    ON "{{.GraphName}}"."PART_OF" (
        ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, '"graph_id"'::ag_catalog.agtype]));

-- 표현식 인덱스는 만든 뒤 ANALYZE 전까지 표현식 통계가 없다. 그동안 플래너가 선택도를
-- 과대 추정해 간선 label 전체를 해시 조인하므로, 같은 적용 안에서 통계를 모은다.
ANALYZE "{{.GraphName}}"."Context";
ANALYZE "{{.GraphName}}"."PRECEDES";
ANALYZE "{{.GraphName}}"."CAUSES";
ANALYZE "{{.GraphName}}"."PART_OF";
