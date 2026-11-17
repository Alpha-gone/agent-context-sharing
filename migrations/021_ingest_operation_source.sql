-- 수집 원천의 서버 판정을 에이전트 판정과 구분해 기록한다.
ALTER TABLE public.operation_log DROP CONSTRAINT IF EXISTS operation_log_judgment_source;
ALTER TABLE public.operation_log ADD CONSTRAINT operation_log_judgment_source CHECK (
    (COALESCE(judgment_source, 'agent') IN ('agent', 'ingest') AND actor_agent_id IS NOT NULL)
    OR (judgment_source IS NOT NULL AND judgment_source = 'inference' AND actor_agent_id IS NULL)
);
