-- 서버가 만든 사건과 대화 턴 범위 및 에이전트 수정 여부를 보존한다.
CREATE TABLE IF NOT EXISTS public.conversation_segment (
    event_context_id uuid PRIMARY KEY,
    graph_id uuid NOT NULL,
    conversation_id text NOT NULL CHECK (char_length(conversation_id) BETWEEN 1 AND 128),
    first_turn_index bigint NOT NULL CHECK (first_turn_index >= 0),
    last_turn_index bigint NOT NULL CHECK (last_turn_index >= first_turn_index),
    created_at timestamptz NOT NULL,
    frozen_at timestamptz,
    UNIQUE (graph_id, conversation_id, first_turn_index)
);
CREATE INDEX IF NOT EXISTS conversation_segment_range_idx
    ON public.conversation_segment (graph_id, conversation_id, last_turn_index);
ANALYZE public.conversation_segment;
