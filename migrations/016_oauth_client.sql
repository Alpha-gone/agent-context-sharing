-- 사전 등록을 제거하고 DCR 공개 클라이언트를 인스턴스 간 공유한다.
-- 기존 계정·토큰·계열은 보존하되 고정 client ID를 자동 등록하지 않는다.
CREATE TABLE IF NOT EXISTS public.oauth_client (
    client_id text PRIMARY KEY,
    client_name text NOT NULL CHECK (char_length(client_name) <= 128),
    redirect_uris text[] NOT NULL CHECK (cardinality(redirect_uris) BETWEEN 1 AND 8),
    resource text NOT NULL,
    loopback_port_policy text NOT NULL CHECK (loopback_port_policy IN ('exact', 'same_ip')),
    created_at timestamptz NOT NULL
);
