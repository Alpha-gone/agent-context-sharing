-- DATA-01의 목표 스키마를 추가한다. 001·009와 적용 이력은 변경하지 않는다.
-- 공개 판·누계의 bigint 대응, 문자열 멱등성 키와 예약 NULL, OAuth·등록 저장이 목적이다.
-- 기존 UUID 키의 텍스트 값·지문·완료 결과·최초 만료·감사·벡터를 그대로 보존한다.
-- 서비스와 작업자를 정지하고 백업한 뒤 적용한다. 제약에 어긋난 기존 행은 자동 정리하지
-- 않으며 전체 010 트랜잭션을 실패시켜 운영자가 원인을 확인하게 한다.
ALTER TABLE public.context_graph ALTER COLUMN version TYPE bigint;
ALTER TABLE public.operation_log ALTER COLUMN target_version TYPE bigint;
ALTER TABLE public.request_rate ALTER COLUMN count TYPE bigint;
ALTER TABLE public.idempotency_record
    ALTER COLUMN idempotency_key TYPE text USING idempotency_key::text,
    ALTER COLUMN tool_result DROP NOT NULL;

DO $constraints$
DECLARE
    item record;
BEGIN
    FOR item IN SELECT * FROM (VALUES
        ('context_graph', 'context_graph_content_revision_positive', 'content_revision >= 1'),
        ('operation_log', 'operation_log_target_version_positive', 'target_version IS NULL OR target_version >= 1'),
        ('idempotency_record', 'idempotency_record_key_format', $$idempotency_key COLLATE "C" ~ '^[A-Za-z0-9_-]{16,128}$'$$),
        ('idempotency_record', 'idempotency_record_fingerprint_length', 'octet_length(request_fingerprint) = 32'),
        ('idempotency_record', 'idempotency_record_tool_name', $$tool_name IN ('graph_create', 'graph_update', 'node_create', 'node_update', 'node_discard', 'node_restore', 'relation_confirm', 'relation_discard')$$)
    ) AS checks(table_name, constraint_name, expression)
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_constraint
                       WHERE conrelid = to_regclass('public.' || item.table_name)
                         AND conname = item.constraint_name) THEN
            EXECUTE format('ALTER TABLE public.%I ADD CONSTRAINT %I CHECK (%s)',
                           item.table_name, item.constraint_name, item.expression);
        END IF;
    END LOOP;
END
$constraints$;

CREATE TABLE IF NOT EXISTS public.oauth_grant (
    grant_id uuid PRIMARY KEY,
    account_id uuid NOT NULL,
    client_id text NOT NULL,
    resource text NOT NULL,
    scope text NOT NULL,
    authorized_at timestamptz NOT NULL,
    last_rotated_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    idle_expires_at timestamptz NOT NULL,
    retain_until timestamptz NOT NULL,
    revoked_at timestamptz,
    revocation_reason text,
    CONSTRAINT oauth_grant_revocation_pair CHECK ((revoked_at IS NULL) = (revocation_reason IS NULL))
);

CREATE TABLE IF NOT EXISTS public.oauth_authorization_code (
    code_hash bytea PRIMARY KEY CHECK (octet_length(code_hash) = 32),
    account_id uuid NOT NULL,
    client_id text NOT NULL,
    redirect_uri text NOT NULL,
    resource text NOT NULL,
    scope text NOT NULL,
    code_challenge text NOT NULL,
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    grant_id uuid REFERENCES public.oauth_grant(grant_id) ON DELETE NO ACTION,
    CONSTRAINT oauth_authorization_code_consumption_pair CHECK ((consumed_at IS NULL) = (grant_id IS NULL))
);

CREATE TABLE IF NOT EXISTS public.oauth_refresh_token (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    grant_id uuid NOT NULL REFERENCES public.oauth_grant(grant_id) ON DELETE NO ACTION,
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    successor_token_hash bytea CHECK (octet_length(successor_token_hash) = 32),
    CONSTRAINT oauth_refresh_token_consumption_pair CHECK ((consumed_at IS NULL) = (successor_token_hash IS NULL))
);

CREATE TABLE IF NOT EXISTS public.agent_registration (
    agent_id uuid PRIMARY KEY,
    account_id uuid NOT NULL,
    client_id text NOT NULL,
    registration_key text NOT NULL CHECK (registration_key COLLATE "C" ~ '^[A-Za-z0-9_-]{16,128}$'),
    created_at timestamptz NOT NULL,
    revoked_at timestamptz,
    UNIQUE (account_id, client_id, registration_key)
);

CREATE INDEX IF NOT EXISTS oauth_grant_account_client_idx
    ON public.oauth_grant (account_id, client_id, authorized_at DESC, grant_id DESC);
CREATE INDEX IF NOT EXISTS oauth_grant_retain_until_idx ON public.oauth_grant (retain_until);
CREATE INDEX IF NOT EXISTS oauth_authorization_code_grant_idx ON public.oauth_authorization_code (grant_id);
CREATE INDEX IF NOT EXISTS oauth_authorization_code_unconsumed_expires_idx
    ON public.oauth_authorization_code (expires_at) WHERE consumed_at IS NULL;
CREATE INDEX IF NOT EXISTS oauth_refresh_token_grant_idx ON public.oauth_refresh_token (grant_id);
CREATE INDEX IF NOT EXISTS agent_registration_account_client_idx
    ON public.agent_registration (account_id, client_id, created_at, agent_id);
CREATE INDEX IF NOT EXISTS agent_registration_active_account_idx
    ON public.agent_registration (account_id) WHERE revoked_at IS NULL;

ANALYZE public.context_graph;
ANALYZE public.operation_log;
ANALYZE public.request_rate;
ANALYZE public.idempotency_record;
ANALYZE public.oauth_grant;
ANALYZE public.oauth_authorization_code;
ANALYZE public.oauth_refresh_token;
ANALYZE public.agent_registration;
