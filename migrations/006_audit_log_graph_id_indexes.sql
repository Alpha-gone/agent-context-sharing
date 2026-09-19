-- 감사 기록 질의는 graph_id로 거르는데 두 테이블에 graph_id 인덱스가 없어 전체 스캔을
-- 한다. 감사 기록 화면, 대기 복구 요청, 기록 보존 정리가 이 경로를 지나며 보존 정리는
-- 그래프마다 삭제문을 반복하므로 비용이 그래프 수와 테이블 크기의 곱으로 커진다.
CREATE INDEX IF NOT EXISTS operation_log_graph_applied_idx
    ON public.operation_log (graph_id, applied_at);

CREATE INDEX IF NOT EXISTS web_audit_log_graph_occurred_idx
    ON public.web_audit_log (graph_id, occurred_at DESC);

-- 팀 감사 기록 정리는 graph_id가 없는 행만 대상이다. 부분 인덱스로 계정 조회와 계정별
-- 삭제가 같은 인덱스를 쓰게 한다.
CREATE INDEX IF NOT EXISTS web_audit_log_team_actor_idx
    ON public.web_audit_log (actor_account_id, occurred_at)
    WHERE graph_id IS NULL;
