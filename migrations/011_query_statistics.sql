-- DATA-05: 만료 폐기 토큰의 정리 인덱스를 추가한다. 기존 적용 SQL·이력은 보존한다.
-- 나머지 조회 인덱스는 001·010이 소유하며 동일 열의 인덱스를 중복 생성하지 않는다.
CREATE INDEX IF NOT EXISTS revoked_token_expires_idx
    ON public.revoked_token (expires_at);

-- 이전 적용 이후 쌓인 데이터에서도 조회·표현식 인덱스의 통계를 같은 적용에 확보한다.
ANALYZE public.account;
ANALYZE public.context_graph;
ANALYZE public.graph_grant;
ANALYZE public.team_member;
ANALYZE public.context_embedding;
ANALYZE public.index_task;
ANALYZE public.operation_log;
ANALYZE public.idempotency_record;
ANALYZE public.web_audit_log;
ANALYZE public.request_rate;
ANALYZE public.signing_key;
ANALYZE public.revoked_token;
ANALYZE public.oauth_grant;
ANALYZE public.oauth_authorization_code;
ANALYZE public.oauth_refresh_token;
ANALYZE public.agent_registration;
ANALYZE "{{.GraphName}}"."Context";
ANALYZE "{{.GraphName}}"."DERIVED_FROM";
ANALYZE "{{.GraphName}}"."SUPERSEDES";
ANALYZE "{{.GraphName}}"."HAS_MEMBER";
ANALYZE "{{.GraphName}}"."PRECEDES";
ANALYZE "{{.GraphName}}"."CAUSES";
ANALYZE "{{.GraphName}}"."PART_OF";
ANALYZE "{{.GraphName}}"."RELATES_TO";
