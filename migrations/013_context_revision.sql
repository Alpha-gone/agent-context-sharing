-- LIFE-03: 파생 판의 본문·참조와 일반 갱신/명시적 대체 연결을 별도로 보존한다.
-- 기존 정점과 과거 적용 이력은 변경하지 않는다. 기존 데이터의 관찰 이전 판은 추정하지 않는다.
CREATE TABLE IF NOT EXISTS public.context_revision (
    graph_id uuid NOT NULL,
    context_id uuid NOT NULL,
    version bigint NOT NULL CHECK (version >= 1),
    previous_version bigint,
    supersedes_context_id uuid,
    supersedes_version bigint,
    snapshot jsonb NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    PRIMARY KEY (graph_id, context_id, version),
    CHECK (previous_version IS NULL OR (previous_version >= 1 AND previous_version = version - 1)),
    CHECK ((supersedes_context_id IS NULL) = (supersedes_version IS NULL)),
    CHECK (supersedes_version IS NULL OR supersedes_version >= 1),
    CHECK (supersedes_context_id IS NULL OR supersedes_context_id <> context_id),
    FOREIGN KEY (graph_id, context_id, previous_version)
        REFERENCES public.context_revision (graph_id, context_id, version),
    FOREIGN KEY (graph_id, supersedes_context_id, supersedes_version)
        REFERENCES public.context_revision (graph_id, context_id, version)
);
