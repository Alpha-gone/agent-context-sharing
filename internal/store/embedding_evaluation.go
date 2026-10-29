package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// EmbeddingFootprint는 평가 그래프의 지정 모델 벡터 열만 센다.
// 테이블·인덱스 전체 용량이나 다른 그래프의 저장량을 포함하지 않는다.
type EmbeddingFootprint struct {
	Rows  int64
	Bytes int64
}

func (s *Store) EmbeddingFootprint(ctx context.Context, graphID model.ID, modelID string) (EmbeddingFootprint, error) {
	if !graphID.IsV7() || strings.TrimSpace(modelID) == "" {
		return EmbeddingFootprint{}, fmt.Errorf("벡터 저장량 조회에 그래프와 모델 식별자가 필요하다")
	}
	var footprint EmbeddingFootprint
	var graph string
	err := s.pool.QueryRow(ctx, `SELECT graph_id::text, count(*), sum(pg_column_size(embedding))
		FROM public.context_embedding WHERE graph_id=$1 AND model_id=$2 GROUP BY graph_id`,
		graphID.String(), modelID).Scan(&graph, &footprint.Rows, &footprint.Bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return EmbeddingFootprint{}, nil
	}
	if err != nil {
		return EmbeddingFootprint{}, fmt.Errorf("벡터 저장량 조회: %w", err)
	}
	if graph != graphID.String() {
		return EmbeddingFootprint{}, fmt.Errorf("벡터 저장량의 그래프가 요청 범위와 다르다")
	}
	return footprint, nil
}

type EmbeddingSimilarity struct {
	ContextID  model.ID
	Similarity float64
}

// EmbeddingSimilarities는 평가에 주석된 후보만 거리·UUID 순으로 반환한다.
// 기준과 후보의 그래프·모델을 함께 제한하며 누락 벡터를 임의의 점수로 채우지 않는다.
func (s *Store) EmbeddingSimilarities(ctx context.Context, graphID model.ID, modelID string, anchor model.ID, candidates []model.ID) ([]EmbeddingSimilarity, error) {
	if !graphID.IsV7() || !anchor.IsV7() || strings.TrimSpace(modelID) == "" {
		return nil, fmt.Errorf("벡터 점수 조회에 그래프·모델·기준 컨텍스트가 필요하다")
	}
	ids := make([]string, len(candidates))
	for i, id := range candidates {
		if !id.IsV7() {
			return nil, fmt.Errorf("벡터 점수 후보 식별자가 UUIDv7이 아니다")
		}
		ids[i] = id.String()
	}
	rows, err := s.pool.Query(ctx, `SELECT b.graph_id::text, b.context_id::text, 1-(b.embedding <=> a.embedding)
		FROM public.context_embedding a JOIN public.context_embedding b ON b.graph_id=a.graph_id AND b.model_id=a.model_id
		WHERE a.graph_id=$1 AND a.model_id=$2 AND a.context_id=$3 AND b.context_id::text=ANY($4)
		ORDER BY b.embedding <=> a.embedding,b.context_id`, graphID.String(), modelID, anchor.String(), ids)
	if err != nil {
		return nil, fmt.Errorf("벡터 점수 조회: %w", err)
	}
	defer rows.Close()
	var result []EmbeddingSimilarity
	for rows.Next() {
		var graph, rawID string
		var score float64
		if err := rows.Scan(&graph, &rawID, &score); err != nil {
			return nil, fmt.Errorf("벡터 점수 읽기: %w", err)
		}
		if graph != graphID.String() {
			return nil, fmt.Errorf("벡터 점수의 그래프가 요청 범위와 다르다")
		}
		id, err := model.ParseID(rawID)
		if err != nil {
			return nil, fmt.Errorf("벡터 점수 식별자: %w", err)
		}
		result = append(result, EmbeddingSimilarity{ContextID: id, Similarity: score})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("벡터 점수 열거: %w", err)
	}
	return result, nil
}
