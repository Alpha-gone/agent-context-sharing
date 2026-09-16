-- 「소프트 삭제 수명주기」가 유예 만료 시각을 유예 시작 시점에 고정하기로 했다. 001은
-- grace_started_at만 두어 주기 작업이 실행 시점의 플랜 값으로 만료를 계산했고, 유예 중에
-- 바뀐 보관 기간이 이미 시작된 유예에 소급되었다.
--
-- 이미 유예 중인 활성 그래프는 시작 당시 플랜 값을 알 수 없으므로 기본 유예 기간 30일로
-- 채운다. 계정별 grace_days를 덮어쓴 배포라면 적용 뒤 해당 행을 운영자가 확인한다.

ALTER TABLE public.context_graph
    ADD COLUMN IF NOT EXISTS grace_expires_at timestamptz;

UPDATE public.context_graph
SET grace_expires_at = grace_started_at + INTERVAL '30 days'
WHERE grace_started_at IS NOT NULL
  AND grace_expires_at IS NULL
  AND deleted_at IS NULL;
