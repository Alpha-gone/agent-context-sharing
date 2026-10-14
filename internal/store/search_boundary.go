package store

import (
	"context"
	"fmt"

	"agent_context_sharing/internal/model"
)

// SearchHopContextsFrom은 검색 호출자에게 저장소 내부 계측을 제외한 결과를 전달한다.
// 기존 홉 경계가 그래프 격리·읽기 정리·결과 완전성을 계속 책임진다.
func (s *Store) SearchHopContextsFrom(ctx context.Context, graphID model.ID, starts []model.Context, hops int, direction string, filter []string, limit int) (model.HopResult, error) {
	result, err := s.HopContextsFrom(ctx, graphID, starts, hops, direction, filter, limit)
	if err != nil {
		return model.HopResult{}, err
	}
	return model.HopResult{
		Contexts: result.Contexts, Distances: result.Distances, Edges: result.Edges,
		Truncated: result.Truncated, Boundary: result.Boundary,
	}, nil
}

// WithReadSnapshot은 저장소가 스냅숏의 시작·종료를 소유하는 검색 읽기 경계다.
// 호출자는 읽기용 context만 받으며 트랜잭션·질의 실행기를 받지 않는다.
func (s *Store) WithReadSnapshot(ctx context.Context, read func(context.Context) error) (stats model.ReadStats, err error) {
	if read == nil {
		return stats, fmt.Errorf("스냅숏 읽기 함수가 필요하다")
	}
	snapshot, err := s.BeginReadSnapshot(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { stats = snapshot.Close(ctx) }()
	err = read(snapshot.Context(ctx))
	return stats, err
}
