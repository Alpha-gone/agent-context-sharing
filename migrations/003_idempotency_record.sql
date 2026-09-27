-- 003_idempotency_record: 협상된 MCP 쓰기 재시도 결과를 계정별로 보관한다.
--
-- 완료 결과는 같은 논리적 호출의 전송 재시도에만 쓰며, 관리 연산 기록을 대신하지 않는다.
-- 최초 접수 시각부터 24시간 뒤 만료하고, 만료 행은 새 키처럼 취급한다.
CREATE TABLE IF NOT EXISTS public.idempotency_record (
    actor_account_id uuid        NOT NULL,
    idempotency_key  uuid        NOT NULL,
    tool_name        text        NOT NULL,
    request_digest   bytea       NOT NULL,
    result_json      jsonb       NOT NULL,
    accepted_at      timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    PRIMARY KEY (actor_account_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idempotency_record_expires_idx
    ON public.idempotency_record (expires_at);
