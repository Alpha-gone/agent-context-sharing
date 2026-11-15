-- #311: 기존 관계와 감사 기록을 재작성하지 않고 추론 출처와 작업 기록을 추가한다.
ALTER TABLE public.operation_log ADD COLUMN IF NOT EXISTS judgment_source text;
ALTER TABLE public.operation_log ALTER COLUMN actor_agent_id DROP NOT NULL;
DO $operation_source$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'public.operation_log'::regclass AND conname = 'operation_log_judgment_source') THEN
        ALTER TABLE public.operation_log ADD CONSTRAINT operation_log_judgment_source CHECK (
            (COALESCE(judgment_source, 'agent') = 'agent' AND actor_agent_id IS NOT NULL)
            OR (judgment_source IS NOT NULL AND judgment_source = 'inference' AND actor_agent_id IS NULL));
    END IF;
END
$operation_source$;

CREATE TABLE IF NOT EXISTS public.relation_inference_task (
    task_id uuid PRIMARY KEY,
    graph_id uuid NOT NULL,
    context_id uuid NOT NULL UNIQUE,
    target_version bigint NOT NULL CHECK (target_version > 0),
    trigger_account_id uuid NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    output_attempts integer NOT NULL DEFAULT 0 CHECK (output_attempts >= 0),
    state text NOT NULL CHECK (state IN ('pending', 'failed')),
    last_error text,
    enqueued_at timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    correlation_id text NOT NULL
);
CREATE INDEX IF NOT EXISTS relation_inference_task_ready_idx ON public.relation_inference_task (next_attempt_at, task_id) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS relation_inference_task_graph_idx ON public.relation_inference_task (graph_id);

CREATE TABLE IF NOT EXISTS public.relation_inference_run (
    run_id uuid PRIMARY KEY,
    graph_id uuid NOT NULL,
    context_id uuid NOT NULL,
    target_version bigint NOT NULL CHECK (target_version > 0),
    profile_fingerprint text NOT NULL,
    profile_versions jsonb NOT NULL,
    candidates jsonb NOT NULL,
    result text NOT NULL,
    attempts integer NOT NULL CHECK (attempts >= 0),
    input_tokens bigint CHECK (input_tokens >= 0),
    output_tokens bigint CHECK (output_tokens >= 0),
    latency_ms bigint CHECK (latency_ms >= 0),
    provider_status text,
    created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS relation_inference_run_graph_idx ON public.relation_inference_run (graph_id, created_at);
CREATE INDEX IF NOT EXISTS relation_inference_run_context_idx ON public.relation_inference_run (graph_id, context_id, target_version);

CREATE TABLE IF NOT EXISTS public.relation_inference_decision (
    decision_id uuid PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES public.relation_inference_run(run_id) ON DELETE CASCADE,
    candidate_context_id uuid NOT NULL,
    candidate_version bigint NOT NULL CHECK (candidate_version > 0),
    relation_type text NOT NULL,
    direction text NOT NULL,
    decision text NOT NULL,
    evidence jsonb NOT NULL,
    reason text,
    validation_result text NOT NULL,
    validation_reason text,
    relation_id uuid
);
CREATE INDEX IF NOT EXISTS relation_inference_decision_run_idx ON public.relation_inference_decision (run_id);
CREATE INDEX IF NOT EXISTS relation_inference_decision_relation_idx ON public.relation_inference_decision (relation_id) WHERE relation_id IS NOT NULL;
