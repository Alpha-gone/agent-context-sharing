-- 002_content_revision: 요청 단위 일관 읽기 비교의 내용 판을 그래프 행에 더한다.
--
-- content_revision은 낙관적 잠금용 version과 다른 값이다. 컨텍스트 생성·갱신·폐기·복구,
-- 사건 관계 확정·폐기와 임베딩 공개 트랜잭션이 하나씩 증가시키며, 내용 판 재시도 검색이
-- 요청 전후 값을 대조한다. 열의 현재 계약은 SDD.md의 「테이블 열 정의」가 소유한다.
-- 001_init.sql은 이 열을 모르므로 적용된 개발 볼륨을 지우지 않고 추가 파일로 넣는다.
ALTER TABLE public.context_graph
    ADD COLUMN IF NOT EXISTS content_revision bigint NOT NULL DEFAULT 1;
