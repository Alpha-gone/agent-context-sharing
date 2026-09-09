-- 활성 서명 키는 하나만 허용하고, 인가 코드 재사용 시 원 토큰의 정확한 만료 시각까지
-- 폐기할 수 있게 한다. 이미 기록된 개발 행은 당시 정확한 발급 시각을 보관하지 않았으므로
-- 코드 수명만큼 늦춘 보수적 값을 사용해 조기 폐기를 막는다.

ALTER TABLE public.authorization_code
    ADD COLUMN IF NOT EXISTS issued_token_expires_at timestamptz;

UPDATE public.authorization_code
SET issued_token_expires_at = issued_at + INTERVAL '61 minutes'
WHERE issued_token_id IS NOT NULL
  AND issued_token_expires_at IS NULL;

WITH ranked_active_keys AS (
    SELECT key_id,
           row_number() OVER (ORDER BY created_at DESC, key_id DESC) AS position
    FROM public.signing_key
    WHERE state = 'active'
)
UPDATE public.signing_key AS signing_key
SET state = 'retired'
FROM ranked_active_keys
WHERE signing_key.key_id = ranked_active_keys.key_id
  AND ranked_active_keys.position > 1;

CREATE UNIQUE INDEX IF NOT EXISTS signing_key_active_idx
    ON public.signing_key (state)
    WHERE state = 'active';
