-- #303·#304: 기존 컨텍스트·적용 이력을 보존하며 명시적 대화 순서와 요약 안내를 추가한다.
DO $navigation_label$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM ag_catalog.ag_label l
        JOIN ag_catalog.ag_graph g ON g.graphid = l.graph
        WHERE g.name = '{{.GraphName}}' AND l.name = 'NAVIGATES_TO'
    ) THEN
        EXECUTE format('SELECT ag_catalog.create_elabel(%L, %L)', '{{.GraphName}}', 'NAVIGATES_TO');
    END IF;
END
$navigation_label$;

ALTER TABLE public.context_graph ADD COLUMN IF NOT EXISTS entry_context_id uuid;

CREATE INDEX IF NOT EXISTS navigates_to_hop_endpoints_idx
    ON "{{.GraphName}}"."NAVIGATES_TO" (start_id, end_id);
CREATE INDEX IF NOT EXISTS navigates_to_graph_id_text_idx
    ON "{{.GraphName}}"."NAVIGATES_TO" ((properties ->> 'graph_id'::text));

-- 삭제한 원천도 순서 정체성을 보존한다. 기존 순서는 추정하거나 채우지 않는다.
CREATE UNIQUE INDEX IF NOT EXISTS context_source_conversation_turn_idx
    ON "{{.GraphName}}"."Context" (
        (properties ->> 'graph_id'::text),
        (properties ->> 'conversation_id'::text),
        (properties ->> 'turn_index'::text))
    WHERE (properties ->> 'layer'::text) = 'source'
      AND (properties ->> 'conversation_id'::text) IS NOT NULL
      AND (properties ->> 'turn_index'::text) IS NOT NULL;

ANALYZE "{{.GraphName}}"."NAVIGATES_TO";
ANALYZE "{{.GraphName}}"."Context";
