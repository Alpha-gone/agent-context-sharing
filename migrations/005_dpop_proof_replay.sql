-- DPoP proof는 원문 jti를 저장하지 않는다. 같은 공개 키와 jti의 재생을 모든 인스턴스에서
-- 한 번만 허용하려고 SHA-256 해시를 기본 키로 두며, 60초 허용 창이 지난 행은 주기 작업이 지운다.
CREATE TABLE IF NOT EXISTS public.dpop_proof_replay (
    jwk_thumbprint text NOT NULL,
    proof_id_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (jwk_thumbprint, proof_id_hash)
);

CREATE INDEX IF NOT EXISTS dpop_proof_replay_expires_at_idx
    ON public.dpop_proof_replay (expires_at);
