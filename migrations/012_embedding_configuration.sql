-- DATA-09: 기존 SQL·적용 이력을 유지하며 현재 벡터 타입·차원으로 전환한다.
-- 서비스와 모든 색인 작업자를 정지하고 백업한 뒤 적용한다. 새 DB 또는 같은 형상에서는
-- 벡터·작업·내용 판을 보존한다. 요청 없는 재등록의 상관 ID는 NULL로 초기화한다.
-- 원본 정점·간선·감사는 보존하며 열·HNSW·큐·이력은 실행기의 한 트랜잭션에 속한다.
ALTER TABLE public.index_task ADD COLUMN IF NOT EXISTS correlation_id text;

DO $embedding_configuration$
DECLARE
    actual text;
BEGIN
    SELECT format_type(atttypid, atttypmod) INTO actual
      FROM pg_attribute
     WHERE attrelid = 'public.context_embedding'::regclass
       AND attname = 'embedding' AND NOT attisdropped;
    IF actual IS DISTINCT FROM '{{.VectorType}}({{.VectorDim}})' THEN
        LOCK TABLE public.context_graph, {{.GraphName}}."Context",
                   public.index_task, public.context_embedding IN ACCESS EXCLUSIVE MODE;
        DROP INDEX IF EXISTS public.context_embedding_embedding_hnsw_idx;

        INSERT INTO public.index_task
            (task_id, context_id, graph_id, attempts, state, enqueued_at, next_attempt_at, correlation_id)
        SELECT uuidv7(), (properties ->> 'context_id'::text)::uuid,
               (properties ->> 'graph_id'::text)::uuid, 0, 'pending', now(), now(), NULL
          FROM {{.GraphName}}."Context"
         WHERE properties ->> 'deleted_at'::text IS NULL
           AND ('{{.IndexTargetLayers}}' <> 'without_source'
                OR properties ->> 'layer'::text <> 'source')
        ON CONFLICT (context_id) DO UPDATE SET
            attempts=0, state='pending', last_error=NULL, correlation_id=NULL,
            enqueued_at=EXCLUDED.enqueued_at, next_attempt_at=EXCLUDED.next_attempt_at;

        DELETE FROM public.index_task AS task USING {{.GraphName}}."Context" AS node
         WHERE '{{.IndexTargetLayers}}' = 'without_source'
           AND node.properties ->> 'layer'::text = 'source'
           AND task.context_id = (node.properties ->> 'context_id'::text)::uuid;

        UPDATE public.context_graph SET content_revision=content_revision+1;
        DELETE FROM public.context_embedding;
        ALTER TABLE public.context_embedding ALTER COLUMN embedding
            TYPE public.{{.VectorType}}({{.VectorDim}})
            USING embedding::public.{{.VectorType}}({{.VectorDim}});
    END IF;
END
$embedding_configuration$;

CREATE INDEX IF NOT EXISTS context_embedding_embedding_hnsw_idx
    ON public.context_embedding USING hnsw (embedding public.{{.VectorType}}_cosine_ops);
ANALYZE public.context_embedding;
