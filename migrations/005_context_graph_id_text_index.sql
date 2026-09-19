-- 001이 만든 컨텍스트 graph_id 인덱스를 저장소 질의가 쓰는 형태로 바꾼다.
--
-- 001은 `properties -> 'graph_id'`로 만들었고 이 표현식은 agtype을 낸다. 그런데 격리
-- 필터와 키워드·시간·재색인·삭제 영향 질의는 모두 `properties ->> 'graph_id'`로 비교하므로
-- 표현식이 달라 인덱스를 쓰지 못하고 label 테이블 전체를 훑는다. 같은 표현식으로 다시
-- 만들고 옛 인덱스는 지운다.
CREATE INDEX IF NOT EXISTS context_graph_id_text_idx
    ON "{{.GraphName}}"."Context" ((properties ->> 'graph_id'::text));

DROP INDEX IF EXISTS "{{.GraphName}}".context_graph_id_idx;
