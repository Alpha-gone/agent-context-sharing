-- 수집 자격과 결합 등록을 추가한다. 기존 OAuth 등록은 유지한다.
CREATE TABLE IF NOT EXISTS public.ingest_credential (
    credential_id uuid PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    account_id uuid NOT NULL,
    label text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 64),
    issued_at timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at timestamptz
);

CREATE INDEX IF NOT EXISTS ingest_credential_account_issued_idx
    ON public.ingest_credential (account_id, issued_at, credential_id);

ALTER TABLE public.agent_registration
    ADD COLUMN IF NOT EXISTS ingest_credential_id uuid;
ALTER TABLE public.agent_registration ALTER COLUMN client_id DROP NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS agent_registration_ingest_credential_idx
    ON public.agent_registration (ingest_credential_id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conrelid = 'public.agent_registration'::regclass
          AND conname = 'agent_registration_one_credential_check'
    ) THEN
        ALTER TABLE public.agent_registration
            ADD CONSTRAINT agent_registration_one_credential_check
            CHECK ((client_id IS NOT NULL) <> (ingest_credential_id IS NOT NULL));
    END IF;
END
$$;

ANALYZE public.ingest_credential;
