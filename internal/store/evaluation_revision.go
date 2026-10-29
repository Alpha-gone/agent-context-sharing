package store

import (
	"context"
	"fmt"

	"agent_context_sharing/internal/model"
)

// EvaluationContentRevision은 평가 전용 내용 판 후보가 읽는 단조 증가 판이다.
// 운영 검색은 이 메서드나 전체 흐름 재시도를 사용하지 않는다.
func (s *Store) EvaluationContentRevision(ctx context.Context, graphID model.ID) (int64, error) {
	if !graphID.IsV7() {
		return 0, fmt.Errorf("평가 내용 판의 그래프가 올바르지 않다")
	}
	var revision int64
	if err := s.pool.QueryRow(ctx, `SELECT content_revision FROM public.context_graph
		WHERE graph_id=$1 AND deleted_at IS NULL`, graphID.String()).Scan(&revision); err != nil {
		return 0, storageError("evaluation_content_revision", err)
	}
	return revision, nil
}
