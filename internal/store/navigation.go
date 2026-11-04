package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func nullableID(id *model.ID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// validGraphEntry는 저장된 지정을 바꾸지 않고 현재 사용할 수 있는 대표만 돌려준다.
// 호출자의 읽기 스냅숏 또는 그래프 쓰기 잠금 아래에서 판정한다.
func (s *Store) validGraphEntry(ctx context.Context, queryer cypherQueryer, graphID model.ID, id *model.ID) (*model.ID, error) {
	if id == nil {
		return nil, nil
	}
	values, err := s.contextVerticesByIDs(ctx, queryer, graphID, []model.ID{*id})
	if err != nil {
		return nil, err
	}
	value, found := values[*id]
	if !found || !usableGraphEntry(value, nowUTC()) {
		return nil, nil
	}
	query := "MATCH (next:Context)-[edge:SUPERSEDES]->(previous:Context) WHERE previous.context_id = " + cypherString(id.String()) +
		" AND previous.graph_id = " + cypherString(graphID.String()) + " AND next.graph_id = " + cypherString(graphID.String()) +
		" AND edge.graph_id = " + cypherString(graphID.String()) + " RETURN next.context_id LIMIT 1"
	var raw string
	err = queryer.QueryRow(ctx, s.cypherSQL(query, "id agtype"), pgx.QueryExecModeExec).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return new(*id), nil
	}
	if err != nil {
		return nil, fmt.Errorf("대표 안내 대체 여부 확인: %w", err)
	}
	return nil, nil
}

func usableGraphEntry(value model.Context, at time.Time) bool {
	return value.DeletedAt == nil && value.Derived != nil && value.Derived.Kind == model.DerivationKindSummary &&
		(value.Derived.ValidFrom == nil || !at.Before(*value.Derived.ValidFrom)) &&
		(value.Derived.ValidTo == nil || !at.After(*value.Derived.ValidTo))
}

func (s *Store) validateNavigationTargets(ctx context.Context, tx pgx.Tx, graphID model.ID, ids []model.ID) error {
	if len(ids) == 0 {
		return nil
	}
	values, err := s.contextVerticesByIDs(ctx, tx, graphID, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		value, found := values[id]
		if !found || value.DeletedAt != nil {
			return model.FieldError{Field: "navigation_refs", Message: "탐색 대상은 같은 그래프의 활성 컨텍스트여야 한다"}
		}
	}
	return nil
}

func (s *Store) createNavigationEdges(ctx context.Context, tx pgx.Tx, graphID model.ID, value model.Context) error {
	if value.Derived == nil || len(value.Derived.NavigationRefs) == 0 {
		return nil
	}
	list, err := cypherIDList(value.Derived.NavigationRefs)
	if err != nil {
		return err
	}
	query := "UNWIND " + list + " AS target MATCH (from:Context), (to:Context) WHERE from.context_id = " + cypherString(value.ID.String()) +
		" AND from.graph_id = " + cypherString(graphID.String()) + " AND to.context_id = target AND to.graph_id = " + cypherString(graphID.String()) +
		" CREATE (from)-[:NAVIGATES_TO {graph_id: " + cypherString(graphID.String()) + "}]->(to)"
	if _, err := tx.Exec(ctx, s.cypherSQL(query, "created agtype"), pgx.QueryExecModeExec); err != nil {
		return fmt.Errorf("탐색 간선 생성: %w", err)
	}
	return nil
}
