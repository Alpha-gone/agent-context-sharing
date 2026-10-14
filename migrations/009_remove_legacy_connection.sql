-- 연결 전용 테이블을 제거한다. 기존 적용 파일의 checksum과 계정·웹 세션,
-- 서명 키·폐기 목록·그래프 및 멱등성 저장은 유지한다.
-- 서비스와 작업자를 정지하고 백업한 뒤 명시적으로 적용한다.
DROP TABLE IF EXISTS public.authorization_code;
DROP TABLE IF EXISTS public.dpop_proof_replay;
