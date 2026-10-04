-- 기존 파일은 수정하지 않고 현재 구성의 벡터 타입·차원으로 전환한다.
-- 서비스와 작업자를 정지한 뒤 적용한다. 원본 컨텍스트는 보존하며 재생성 가능한
-- 벡터만 제거한다. 작업 등록·열 변경·HNSW 재생성은 실행기의 같은 트랜잭션에 속한다.
DO $embedding_transition$
DECLARE
    actual text;
BEGIN
    SELECT format_type(atttypid, atttypmod) INTO actual
      FROM pg_attribute
     WHERE attrelid = 'public.context_embedding'::regclass
       AND attname = 'embedding' AND NOT attisdropped;
    IF actual IS DISTINCT FROM '{{.VectorType}}({{.VectorDim}})' THEN
        DROP INDEX IF EXISTS public.context_embedding_embedding_hnsw_idx;

        INSERT INTO public.index_task
            (task_id, context_id, graph_id, attempts, state, enqueued_at, next_attempt_at)
        SELECT uuidv7(), (properties ->> 'context_id'::text)::uuid,
               (properties ->> 'graph_id'::text)::uuid, 0, 'pending', now(), now()
          FROM {{.GraphName}}."Context"
         WHERE properties ->> 'deleted_at'::text IS NULL
           AND ('{{.IndexTargetLayers}}' <> 'without_source'
                OR properties ->> 'layer'::text <> 'source')
        ON CONFLICT (context_id) DO UPDATE SET
            attempts=0, state='pending', last_error=NULL,
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
$embedding_transition$;

CREATE INDEX IF NOT EXISTS context_embedding_embedding_hnsw_idx
    ON public.context_embedding USING hnsw (embedding public.{{.VectorType}}_cosine_ops);
ANALYZE public.context_embedding;
