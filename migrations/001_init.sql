-- 초기 스키마. SDD.md의 「스키마 적용」이 정한 순서를 따른다.
-- 구성 값은 실행기가 검증한 뒤 치환한다.

-- 1. 확장
-- 베이스 이미지가 initdb 단계에서 age 확장을 만들지만, 초기화 경로와 무관하게
-- 같은 결과가 나오도록 여기에서 다시 선언한다.
CREATE EXTENSION IF NOT EXISTS age;
-- vector도 스키마를 명시한다. search_path의 첫 항목이 ag_catalog라 명시하지 않으면
-- 확장이 AGE의 카탈로그 스키마에 만들어지고 벡터 타입 이름이 거기에 묶인다.
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;

-- 2. AGE 그래프. 「물리 배치」가 인스턴스 안에서 하나로 고정한다.
SELECT ag_catalog.create_graph('{{.GraphName}}')
WHERE NOT EXISTS (SELECT 1 FROM ag_catalog.ag_graph WHERE name = '{{.GraphName}}');

-- 3. label. 「그래프 스키마」의 정점 하나와 간선 일곱 종이다.
-- 인덱스를 걸려면 label 테이블이 먼저 있어야 하므로 정점이 들어오길 기다리지 않고
-- 명시적으로 만든다.
SELECT ag_catalog.create_vlabel('{{.GraphName}}', 'Context')
WHERE NOT EXISTS (
    SELECT 1 FROM ag_catalog.ag_label l
    JOIN ag_catalog.ag_graph g ON g.graphid = l.graph
    WHERE g.name = '{{.GraphName}}' AND l.name = 'Context');

DO $$
DECLARE
    edge_label text;
BEGIN
    FOREACH edge_label IN ARRAY ARRAY[
        'DERIVED_FROM', 'SUPERSEDES', 'HAS_MEMBER',
        'PRECEDES', 'CAUSES', 'PART_OF', 'RELATES_TO'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM ag_catalog.ag_label l
            JOIN ag_catalog.ag_graph g ON g.graphid = l.graph
            WHERE g.name = '{{.GraphName}}' AND l.name = edge_label
        ) THEN
            -- create_elabel은 cstring을 받으므로 변수를 그대로 넘길 수 없다.
            -- format의 %L로 리터럴을 만들어 넘긴다.
            EXECUTE format('SELECT ag_catalog.create_elabel(%L, %L)',
                           '{{.GraphName}}', edge_label);
        END IF;
    END LOOP;
END $$;

-- 4. 관계형 테이블. 「테이블 열 정의」와 각 소절이 정한 열을 쓴다.
--
-- 스키마를 public으로 명시하는 이유는 「데이터베이스 연결」이 정한 search_path의
-- 첫 항목이 ag_catalog이기 때문이다. 명시하지 않으면 테이블이 AGE의 카탈로그
-- 스키마에 만들어진다.

-- 「계정 속성」의 4개 열. login_id의 형식은 「입력 검증 세부」가 정한다.
-- deleted_at을 두지 않는 이유는 「계정 소멸」이 소멸 경로를 두지 않기로 확정해
-- 표시할 상태가 없기 때문이다.
CREATE TABLE IF NOT EXISTS public.account (
    account_id    uuid        PRIMARY KEY,
    login_id      text        NOT NULL CHECK (login_id ~ '^[a-z0-9_]{3,32}$'),
    password_hash text        NOT NULL CHECK (password_hash <> ''),
    created_at    timestamptz NOT NULL
);

-- 「컨텍스트 그래프 속성」의 10개 열에 「소프트 삭제 수명주기」의 내부 열
-- grace_expires_at을 더한다. 만료 시각을 유예 시작 트랜잭션에서 계산해 남겨야
-- 유예 중에 바뀐 보관 기간이 이미 시작된 유예에 소급되지 않는다.
CREATE TABLE IF NOT EXISTS public.context_graph (
    graph_id         uuid        PRIMARY KEY,
    name             text        NOT NULL CHECK (name <> ''),
    description      text,
    created_by       uuid        NOT NULL,
    created_at       timestamptz NOT NULL,
    last_activity_at timestamptz NOT NULL,
    grace_started_at timestamptz,
    grace_expires_at timestamptz,
    stored_chars     bigint      NOT NULL DEFAULT 0 CHECK (stored_chars >= 0),
    version          integer     NOT NULL DEFAULT 1 CHECK (version >= 1),
    deleted_at       timestamptz
);

CREATE TABLE IF NOT EXISTS public.graph_grant (
    graph_id     uuid NOT NULL,
    subject_type text NOT NULL CHECK (subject_type IN ('account', 'team')),
    subject_id   uuid NOT NULL,
    grade        text NOT NULL CHECK (grade IN ('owner', 'editor', 'viewer')),
    PRIMARY KEY (graph_id, subject_type, subject_id)
);

-- 「권한과 팀 관리 절차」가 팀 삭제를 소프트 삭제로 확정했다. 삭제된 팀의
-- graph_grant 행은 남지만 유효 등급 계산이 deleted_at을 함께 보므로 등급이
-- 적용되지 않는다.
CREATE TABLE IF NOT EXISTS public.team (
    team_id            uuid        PRIMARY KEY,
    name               text        NOT NULL CHECK (name <> ''),
    manager_account_id uuid        NOT NULL,
    created_at         timestamptz NOT NULL,
    deleted_at         timestamptz
);

-- 외래 키를 두지 않는다. 「폐기와 사용자 직접 삭제」가 영구 삭제를 하지 않기로 했고
-- 「계정 소멸」이 계정을 없애는 경로를 두지 않기로 확정해, 참조 무결성 제약이 걸릴
-- 삭제 경로가 애초에 없다. graph_grant.subject_id는 계정과 팀을 함께 가리키므로
-- 한쪽 테이블로 외래 키를 걸 수도 없다.
CREATE TABLE IF NOT EXISTS public.team_member (
    team_id    uuid NOT NULL,
    account_id uuid NOT NULL,
    PRIMARY KEY (team_id, account_id)
);

-- 「임베딩 스키마」의 열. 벡터 타입과 차원은 배포 구성이 정한다. storage_tier를
-- 파티션 키로 두어 활성 검색은 두 계층을 한 테이블처럼 읽고, 이동은 행 갱신으로 끝낸다.
CREATE TABLE IF NOT EXISTS public.context_embedding (
    context_id uuid        NOT NULL,
    graph_id   uuid        NOT NULL,
    embedding  {{.VectorType}}({{.VectorDim}}) NOT NULL,
    model_id   text        NOT NULL,
    indexed_at timestamptz NOT NULL,
    last_accessed_at timestamptz NOT NULL,
    storage_tier text NOT NULL DEFAULT 'hot' CHECK (storage_tier IN ('hot', 'cold'))
) PARTITION BY LIST (storage_tier);

CREATE TABLE IF NOT EXISTS public.context_embedding_hot
    PARTITION OF public.context_embedding FOR VALUES IN ('hot');

CREATE TABLE IF NOT EXISTS public.context_embedding_cold
    PARTITION OF public.context_embedding FOR VALUES IN ('cold'){{.ColdTablespaceClause}};

-- PostgreSQL의 partitioned unique index는 파티션 키를 포함해야 한다. 컨텍스트 하나가
-- 두 계층에 동시에 존재하지 않게 하는 책임은 단일 저장 경로의 delete-insert와 계층 이동
-- 갱신이 맡고, 각 파티션 안에서는 context_id를 유일하게 유지한다.
CREATE UNIQUE INDEX IF NOT EXISTS context_embedding_hot_context_idx
    ON public.context_embedding_hot (context_id);

CREATE UNIQUE INDEX IF NOT EXISTS context_embedding_cold_context_idx
    ON public.context_embedding_cold (context_id);

-- 「색인 작업 큐」의 9개 열. 컨텍스트마다 한 행만 두고 등록을 upsert로 처리하므로
-- context_id에 유일 인덱스를 건다. 작업자는 next_attempt_at이 지난 행을
-- FOR UPDATE SKIP LOCKED로 확보하며 처리 중을 나타내는 상태를 두지 않는다.
CREATE TABLE IF NOT EXISTS public.index_task (
    task_id         uuid        PRIMARY KEY,
    context_id      uuid        NOT NULL,
    graph_id        uuid        NOT NULL,
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    state           text        NOT NULL CHECK (state IN ('pending', 'failed')),
    last_error      text,
    enqueued_at     timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    correlation_id  text
);

-- 「기록 항목」의 7개 항목을 편 열. 대상은 컨텍스트이거나 사건 관계이며 둘을 동시에
-- 가리키지 않는다. 한 열에 두 종류를 섞으면 어느 쪽인지 판별할 값이 없어 「되짚기」가
-- 관계 기록을 컨텍스트로 잘못 읽는다. 관계에는 판 번호가 없어 target_version을 비우고,
-- 추가와 확정의 거부는 대상이 끝내 만들어지지 않으므로 대상을 모두 비운다.
CREATE TABLE IF NOT EXISTS public.operation_log (
    operation_id     uuid        PRIMARY KEY,
    graph_id         uuid        NOT NULL,
    operation_kind   text        NOT NULL CHECK (operation_kind IN ('add', 'update', 'supersede', 'discard', 'keep')),
    context_id       uuid,
    relation_id      uuid,
    target_version   integer,
    judgment_input   text        NOT NULL,
    actor_account_id uuid        NOT NULL,
    actor_agent_id   uuid        NOT NULL,
    applied_at       timestamptz NOT NULL,
    result           text        NOT NULL CHECK (result IN ('applied', 'rejected')),
    reject_reason    text,
    CONSTRAINT operation_log_reject_reason CHECK (
        (result = 'rejected') OR (reject_reason IS NULL)),
    CONSTRAINT operation_log_target CHECK (
        (context_id IS NOT NULL AND relation_id IS NULL AND target_version IS NOT NULL)
        OR (context_id IS NULL AND relation_id IS NOT NULL AND target_version IS NULL)
        OR (result = 'rejected' AND context_id IS NULL AND relation_id IS NULL AND target_version IS NULL)),
    -- 관계에는 판이 없어 추가와 갱신을 나눌 기준이 없으므로 두 종류만 쓴다.
    CONSTRAINT operation_log_relation_kind CHECK (
        relation_id IS NULL OR operation_kind IN ('add', 'discard'))
);

-- 「감사 기록」의 대상 여섯 종. 공통 다섯 열 외에는 대상에 따라 비어 있을 수 있다.
-- 복구 요청과 운영자 복구를 나누는 이유는 요청과 처리가 다른 시점의 다른 행위이고,
-- 대기 중인 요청을 두 대상의 차이로 계산하기 때문이다.
CREATE TABLE IF NOT EXISTS public.web_audit_log (
    audit_id             uuid        PRIMARY KEY,
    target_kind          text        NOT NULL CHECK (target_kind IN (
                                         'grant', 'ownership_transfer', 'team',
                                         'web_delete', 'restore_request',
                                         'operator_restore')),
    action               text        NOT NULL CHECK (action IN (
                                         'grant', 'revoke', 'transfer',
                                         'add', 'remove', 'delete', 'restore',
                                         'request')),
    actor_account_id     uuid        NOT NULL,
    occurred_at          timestamptz NOT NULL,
    graph_id             uuid,
    subject_type         text        CHECK (subject_type IN ('account', 'team')),
    subject_id           uuid,
    before_grade         text        CHECK (before_grade IN ('owner', 'editor', 'viewer')),
    after_grade          text        CHECK (after_grade IN ('owner', 'editor', 'viewer')),
    team_id              uuid,
    member_account_id    uuid,
    target_context_id    uuid,
    requester_account_id uuid,
    CONSTRAINT web_audit_log_operator_restore CHECK (
        (target_kind <> 'operator_restore') OR (requester_account_id IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS public.signing_key (
    key_id      text        PRIMARY KEY,
    algorithm   text        NOT NULL,
    public_key  text        NOT NULL,
    private_key text        NOT NULL,
    state       text        NOT NULL CHECK (state IN ('active', 'retired')),
    created_at  timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS public.revoked_token (
    token_id   text        PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

-- 「계정 플랜 값」의 1분 고정 창 카운터. 한도를 켠 계정만 행을 가지므로 기본
-- 배포에서는 비어 있다. 지난 창의 행은 「주기 작업」이 지운다.
CREATE TABLE IF NOT EXISTS public.request_rate (
    account_id        uuid        NOT NULL,
    window_started_at timestamptz NOT NULL,
    count             integer     NOT NULL DEFAULT 0 CHECK (count >= 0),
    PRIMARY KEY (account_id, window_started_at)
);

-- 「인가 코드 흐름」의 10개 열에 발급 토큰의 만료 시각을 더한다. 코드 원문을 저장하지
-- 않고 해시를 기본 키로 둔다. consumed_at을 두고 행을 지우지 않는 이유는 재사용과 없는
-- 코드를 구분하고 폐기할 토큰을 찾기 위해서다. code_challenge_method는 S256만 받으므로
-- 열로 두지 않는다. issued_token_expires_at은 재사용을 만났을 때 원 토큰을 그 만료
-- 시각까지 정확히 폐기하려고 둔다.
CREATE TABLE IF NOT EXISTS public.authorization_code (
    code_hash                text        PRIMARY KEY,
    client_id                text        NOT NULL,
    account_id               uuid        NOT NULL,
    redirect_uri             text        NOT NULL,
    code_challenge           text        NOT NULL,
    resource                 text        NOT NULL,
    issued_at                timestamptz NOT NULL,
    expires_at               timestamptz NOT NULL,
    consumed_at              timestamptz,
    issued_token_id          text,
    issued_token_expires_at  timestamptz
);

-- 5. 인덱스. 「인덱스」가 정한 인덱스를 만든다.

-- 로그인 조회와 중복 등록 거부. 「로그인 아이디의 유일성」이 유일성을 기능의 전제로
-- 확정했고, 접근 계층의 검사만으로는 같은 아이디를 동시에 등록하는 두 요청을 막지
-- 못하므로 유일 인덱스로 데이터베이스가 거절하게 한다.
CREATE UNIQUE INDEX IF NOT EXISTS account_login_id_idx
    ON public.account (login_id);

-- 그래프 목록의 기본 정렬과 커서 페이지 처리
CREATE INDEX IF NOT EXISTS context_graph_active_activity_idx
    ON public.context_graph (last_activity_at DESC, graph_id DESC)
    WHERE deleted_at IS NULL;

-- 계정이 접근 가능한 그래프 목록
CREATE INDEX IF NOT EXISTS graph_grant_subject_idx
    ON public.graph_grant (subject_id);

-- 유효 등급 계산
CREATE INDEX IF NOT EXISTS team_member_account_idx
    ON public.team_member (account_id);

-- 그래프 단위 필터와 재색인
CREATE INDEX IF NOT EXISTS context_embedding_graph_idx
    ON public.context_embedding (graph_id);

-- 의미 유사도 채널. 0단계가 이 배포의 벡터 인덱스로 HNSW와 코사인 거리를 확정했다.
-- 연산자 클래스는 열 타입에 따라 달라지므로 열과 같은 배포 구성 값을 쓴다. 고정하면
-- vector가 아닌 배포에서 이 파일이 실패한다.
CREATE INDEX IF NOT EXISTS context_embedding_embedding_hnsw_idx
    ON public.context_embedding USING hnsw (embedding public.{{.VectorType}}_cosine_ops);

-- 색인 작업자의 대기 작업 조회
CREATE INDEX IF NOT EXISTS index_task_state_next_attempt_idx
    ON public.index_task (state, next_attempt_at);

-- 컨텍스트당 한 작업과 등록 upsert의 충돌 대상
CREATE UNIQUE INDEX IF NOT EXISTS index_task_context_idx
    ON public.index_task (context_id);

-- 복구 판정과 보존 기간 정리
CREATE INDEX IF NOT EXISTS operation_log_context_applied_idx
    ON public.operation_log (context_id, applied_at);

CREATE INDEX IF NOT EXISTS operation_log_relation_idx
    ON public.operation_log (relation_id, applied_at)
    WHERE relation_id IS NOT NULL;

-- 감사 기록 화면과 그래프별 보존 정리. 두 기록 모두 graph_id로 거르며, 보존 정리는
-- 그래프마다 삭제문을 반복하므로 인덱스가 없으면 비용이 그래프 수와 테이블 크기의
-- 곱으로 커진다.
CREATE INDEX IF NOT EXISTS operation_log_graph_applied_idx
    ON public.operation_log (graph_id, applied_at);

CREATE INDEX IF NOT EXISTS web_audit_log_graph_occurred_idx
    ON public.web_audit_log (graph_id, occurred_at DESC);

-- 팀 감사 기록 정리는 graph_id가 없는 행만 대상이다. 부분 인덱스로 계정 조회와 계정별
-- 삭제가 같은 인덱스를 쓰게 한다.
CREATE INDEX IF NOT EXISTS web_audit_log_team_actor_idx
    ON public.web_audit_log (actor_account_id, occurred_at)
    WHERE graph_id IS NULL;

-- 서명용 활성 키는 하나만 둔다. 둘 이상이 활성이면 어느 키로 서명했는지가 발급
-- 시점의 조회 순서에 달리게 된다.
CREATE UNIQUE INDEX IF NOT EXISTS signing_key_active_idx
    ON public.signing_key (state)
    WHERE state = 'active';

-- 만료된 인가 코드의 주기 정리
CREATE INDEX IF NOT EXISTS authorization_code_expires_idx
    ON public.authorization_code (expires_at);

-- 지난 창의 주기 정리
CREATE INDEX IF NOT EXISTS request_rate_window_idx
    ON public.request_rate (window_started_at);

-- 격리 필터와 탐색 시작점 탐색, 키워드 채널
-- AGE의 label 테이블은 일반 PostgreSQL 테이블이므로 property를 꺼내는 표현식에
-- 인덱스를 걸 수 있다.
-- 표현식은 저장소 질의와 같은 ->> 여야 한다. -> 는 agtype을 내므로 표현식이 달라
-- 인덱스를 쓰지 못하고 label 테이블 전체를 훑는다.
CREATE INDEX IF NOT EXISTS context_graph_id_text_idx
    ON "{{.GraphName}}"."Context" ((properties ->> 'graph_id'::text));

-- 키워드 채널. 「채널 구현」이 텍스트 검색 구성을 simple로 확정했다. PostgreSQL이
-- 기본 제공하는 구성 중 한국어를 다루는 것이 없고 형태소 분석을 붙이려면 확장
-- 의존이 하나 늘기 때문이다. simple은 어간 추출을 하지 않아 조사가 붙은 표기가
-- 일치하지 않으며, 형태소 분석 도입은 TBD-AGENT_CONTEXT-061이 소유한다.
-- 질의도 같은 구성을 써야 하므로 plainto_tsquery('simple', ...)로 짝을 맞춘다.
CREATE INDEX IF NOT EXISTS context_body_fts_idx
    ON "{{.GraphName}}"."Context"
    USING gin (to_tsvector('simple', properties ->> 'body'::text));

-- 원천 중복 판정. 「생성과 수정 검증」이 같은 그래프에서 source_ref가 같은 원천을
-- 중복으로 확정했고, 접근 계층의 조회만으로는 동시 요청을 막지 못한다.
-- source_ref는 원천에만 있으므로 layer로 거른 부분 인덱스를 쓴다.
CREATE UNIQUE INDEX IF NOT EXISTS context_source_ref_idx
    ON "{{.GraphName}}"."Context" (
        (properties ->> 'graph_id'::text),
        (properties ->> 'source_ref_locator'::text))
    WHERE (properties ->> 'layer'::text) = 'source';
