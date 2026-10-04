-- 001_init.sql의 인가 코드에 검증된 웹 세션의 최초 인증 시각을 추가한다.
-- 기존 행의 로그인 시각은 복원할 수 없으므로 NULL로 남겨 교환을 거부한다.
-- 소비된 행과 발급 토큰 정보는 재사용 감지를 위해 보존한다.
ALTER TABLE public.authorization_code
    ADD COLUMN IF NOT EXISTS authenticated_at timestamptz;
