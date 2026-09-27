-- 004_idempotency_record_columns: 적용된 003의 멱등성 열 이름을 현재 SDD 용어로 맞춘다.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'idempotency_record'
          AND column_name = 'request_digest'
    ) THEN
        ALTER TABLE public.idempotency_record RENAME COLUMN request_digest TO request_fingerprint;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'idempotency_record'
          AND column_name = 'result_json'
    ) THEN
        ALTER TABLE public.idempotency_record RENAME COLUMN result_json TO tool_result;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'idempotency_record'
          AND column_name = 'accepted_at'
    ) THEN
        ALTER TABLE public.idempotency_record RENAME COLUMN accepted_at TO received_at;
    END IF;
END
$$;
