-- 수집 턴과 같은 트랜잭션에서 등록할 대화별 구간화 작업을 보존한다.
CREATE TABLE IF NOT EXISTS public.conversation_segment_task (
    task_id uuid PRIMARY KEY,
    graph_id uuid NOT NULL,
    conversation_id text NOT NULL CHECK (char_length(conversation_id) BETWEEN 1 AND 128),
    trigger_account_id uuid NOT NULL,
    trigger_agent_id uuid NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    state text NOT NULL CHECK (state IN ('pending', 'failed')),
    last_error text,
    enqueued_at timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    correlation_id text NOT NULL,
    UNIQUE (graph_id, conversation_id)
);

CREATE INDEX IF NOT EXISTS conversation_segment_task_ready_idx
    ON public.conversation_segment_task (next_attempt_at, task_id) WHERE state = 'pending';

ANALYZE public.conversation_segment_task;
