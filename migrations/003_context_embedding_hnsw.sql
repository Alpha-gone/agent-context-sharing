-- 0단계가 이 배포의 벡터 인덱스로 HNSW와 코사인 거리를 확정했다. 001은 방식을 정하지 않아
-- 벡터 인덱스를 만들지 않았으므로 여기에서 추가한다.
--
-- 연산자 클래스는 열 타입에 따라 달라진다. 001이 열 타입을 배포 구성으로 템플릿하므로
-- 인덱스도 같은 값을 써야 하며, 고정하면 vector가 아닌 배포에서 이 파일이 실패한다.
CREATE INDEX IF NOT EXISTS context_embedding_embedding_hnsw_idx
    ON public.context_embedding USING hnsw (embedding public.{{.VectorType}}_cosine_ops);
