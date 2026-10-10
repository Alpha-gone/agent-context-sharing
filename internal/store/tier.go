package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	embeddingTierHot  = "hot"
	embeddingTierCold = "cold"
)

type embeddingAccess struct {
	graphID    model.ID
	contextIDs []model.ID
	at         time.Time
}

// touchEmbeddings는 실제 조회에 쓰인 활성 컨텍스트의 임베딩 접근 시각을 갱신하고
// cold 행을 hot 파티션으로 되돌린다. 임베딩이 없는 컨텍스트는 그대로 통과한다.
// 스냅숏 읽기 안이면 요청을 모으고, enterReadScope의 종료 함수가 읽기 연결 반환 뒤 실행한다.
func (s *Store) touchEmbeddings(ctx context.Context, graphID model.ID, contextIDs []model.ID, at time.Time) error {
	if len(contextIDs) == 0 {
		return nil
	}
	if !graphID.IsV7() || at.IsZero() {
		return fmt.Errorf("임베딩 접근 기록 인자가 올바르지 않다")
	}
	ids := make([]string, 0, len(contextIDs))
	for _, contextID := range contextIDs {
		if !contextID.IsV7() {
			return fmt.Errorf("컨텍스트 식별자가 UUIDv7이 아니다")
		}
		ids = append(ids, contextID.String())
	}
	if accesses, ok := ctx.Value(readAccessKey{}).(*[]embeddingAccess); ok {
		*accesses = append(*accesses, embeddingAccess{graphID: graphID, contextIDs: slices.Clone(contextIDs), at: at})
		return nil
	}
	// 대상 행을 context_id 순서로 잠그고 다른 요청이 잠근 행은 건너뛴다. 흐름 응답마다 담긴
	// 컨텍스트 전부를 기록하므로 동시 요청은 겹치는 행을 서로 다른 순서로 잠그게 되고, 한
	// 문장으로 갱신하면 교착과 교착 감지 대기가 연결 풀을 고갈시킨다. 건너뛴 행은 잠근 요청이
	// 같은 시각으로 기록하므로 접근 기록으로서 잃는 것이 없다.
	err := retryEmbeddingAccess(func() error {
		_, err := s.pool.Exec(ctx, `
		UPDATE public.context_embedding AS embedding
		SET last_accessed_at = $3, storage_tier = $4
		FROM (
			SELECT context_id FROM public.context_embedding
			WHERE graph_id = $1 AND context_id = ANY($2::uuid[])
			ORDER BY context_id
			FOR UPDATE SKIP LOCKED
		) AS target
		WHERE embedding.graph_id = $1 AND embedding.context_id = target.context_id
	`, graphID.String(), ids, at.UTC(), embeddingTierHot)
		return err
	})
	if err != nil {
		return fmt.Errorf("임베딩 되읽기 기록: %w", err)
	}
	return nil
}

// retryEmbeddingAccess는 동시 파티션 이동의 40001에만 한 번 다시 시도한다. 접근 기록은
// 같은 대상·시각으로 반복해도 같은 갱신이며, 앞선 자동 커밋 문장의 연결은 이미 반환됐다.
func retryEmbeddingAccess(execute func() error) error {
	err := execute()
	if failure, ok := errors.AsType[*pgconn.PgError](err); ok && failure.Code == "40001" {
		return execute()
	}
	return err
}

// MoveColdEmbeddings는 접근이 차단됐거나 플랜 기준보다 오래 쓰이지 않은 임베딩을
// cold 파티션으로 옮긴다. 행을 갱신할 뿐 삭제하지 않으므로 부모 테이블을 통한 검색과
// 재색인 경로는 계층과 무관하게 같은 행을 본다.
func (s *Store) MoveColdEmbeddings(ctx context.Context, now time.Time, daysFor RetentionDays) (int, error) {
	if now.IsZero() || daysFor == nil {
		return 0, fmt.Errorf("임베딩 계층 이동 인자가 올바르지 않다")
	}
	graphs, err := s.retentionGraphs(ctx)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, graph := range graphs {
		if graph.deleted {
			count, err := s.moveGraphEmbeddingsCold(ctx, graph.id)
			if err != nil {
				return moved, err
			}
			moved += count
			continue
		}
		count, err := s.moveDeletedContextEmbeddingsCold(ctx, graph.id)
		if err != nil {
			return moved, err
		}
		moved += count
		days := daysFor(graph.createdBy)
		if days == 0 {
			continue
		}
		count, err = s.moveInactiveEmbeddingsCold(ctx, graph.id, now.UTC().AddDate(0, 0, -days))
		if err != nil {
			return moved, err
		}
		moved += count
	}
	return moved, nil
}

func (s *Store) moveGraphEmbeddingsCold(ctx context.Context, graphID model.ID) (int, error) {
	result, err := s.pool.Exec(ctx, `
		UPDATE public.context_embedding SET storage_tier = $2
		WHERE graph_id = $1 AND storage_tier = $3
	`, graphID.String(), embeddingTierCold, embeddingTierHot)
	if err != nil {
		return 0, fmt.Errorf("접근 차단 그래프 임베딩 계층 이동: %w", err)
	}
	return int(result.RowsAffected()), nil
}

func (s *Store) moveDeletedContextEmbeddingsCold(ctx context.Context, graphID model.ID) (int, error) {
	result, err := s.pool.Exec(ctx, `
		UPDATE public.context_embedding AS embedding
		SET storage_tier = $2
		FROM `+s.contextTable()+` AS node
		WHERE embedding.graph_id = $1 AND embedding.storage_tier = $3
		  AND (node.properties ->> 'context_id'::text)::uuid = embedding.context_id
		  AND node.properties ->> 'graph_id'::text = $1::text
		  AND node.properties ->> 'deleted_at'::text IS NOT NULL
	`, graphID.String(), embeddingTierCold, embeddingTierHot)
	if err != nil {
		return 0, fmt.Errorf("폐기 컨텍스트 임베딩 계층 이동: %w", err)
	}
	return int(result.RowsAffected()), nil
}

func (s *Store) moveInactiveEmbeddingsCold(ctx context.Context, graphID model.ID, cutoff time.Time) (int, error) {
	result, err := s.pool.Exec(ctx, `
		UPDATE public.context_embedding AS embedding
		SET storage_tier = $3
		FROM `+s.contextTable()+` AS node
		WHERE embedding.graph_id = $1 AND embedding.storage_tier = $4
		  AND embedding.last_accessed_at <= $2
		  AND (node.properties ->> 'context_id'::text)::uuid = embedding.context_id
		  AND node.properties ->> 'graph_id'::text = $1::text
		  AND node.properties ->> 'deleted_at'::text IS NULL
	`, graphID.String(), cutoff.UTC(), embeddingTierCold, embeddingTierHot)
	if err != nil {
		return 0, fmt.Errorf("미사용 활성 임베딩 계층 이동: %w", err)
	}
	return int(result.RowsAffected()), nil
}
