package store

import (
	"context"
	"fmt"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// CreateRelation은 같은 그래프의 사건 두 개를 잇는 사건 관계 간선을 만든다.
func (s *Store) CreateRelation(ctx context.Context, graphID model.ID, relation model.Relation) (model.Relation, error) {
	if !graphID.IsV7() || relation.GraphID != graphID {
		return model.Relation{}, fmt.Errorf("요청 graph_id와 관계 graph_id가 일치하지 않는다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 생성 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	from, err := s.context(ctx, tx, graphID, relation.FromContextID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 시작 사건 조회: %w", err)
	}
	to, err := s.context(ctx, tx, graphID, relation.ToContextID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 대상 사건 조회: %w", err)
	}
	if err := model.ValidateRelation(relation, from, to); err != nil {
		return model.Relation{}, fmt.Errorf("관계 검증: %w", err)
	}
	properties, err := encodeProperties(relationProperties(relation))
	if err != nil {
		return model.Relation{}, err
	}
	label, err := relationLabel(relation.Type)
	if err != nil {
		return model.Relation{}, err
	}
	query := "MATCH (from:Context), (to:Context) WHERE from.context_id = " + cypherString(relation.FromContextID.String()) +
		" AND from.graph_id = " + cypherString(graphID.String()) +
		" AND to.context_id = " + cypherString(relation.ToContextID.String()) +
		" AND to.graph_id = " + cypherString(graphID.String()) +
		" CREATE (from)-[edge:" + label + " " + properties + "]->(to) RETURN edge"
	stored, err := s.relationFromCypher(ctx, tx, graphID, query)
	if err != nil {
		return model.Relation{}, fmt.Errorf("사건 관계 생성: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Relation{}, fmt.Errorf("관계 생성 커밋: %w", err)
	}
	return stored, nil
}

// ListRelations는 graph_id에 속한 관계를 relation_id 내림차순으로 한 페이지 읽는다.
func (s *Store) ListRelations(ctx context.Context, graphID model.ID, cursor string, limit int) ([]model.Relation, string, error) {
	if !graphID.IsV7() || limit <= 0 {
		return nil, "", fmt.Errorf("관계 목록 인자가 올바르지 않다")
	}
	// Apache AGE 1.8은 label 대안(`:A|B`) 문법을 지원하지 않으므로, 네 사건 관계에만
	// 공통으로 있는 relation_type property로 참조 간선을 제외한다.
	query := "MATCH ()-[edge]->() WHERE edge.graph_id = " + cypherString(graphID.String()) + " AND edge.relation_type IS NOT NULL"
	if cursor != "" {
		relationID, err := decodeRelationCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		query += " AND edge.relation_id < " + cypherString(relationID.String())
	}
	query += " RETURN edge ORDER BY edge.relation_id DESC LIMIT " + fmt.Sprint(limit+1)
	rows, err := s.pool.Query(ctx, s.cypherSQL(query, "edge agtype"))
	if err != nil {
		return nil, "", fmt.Errorf("관계 목록 조회: %w", err)
	}
	defer rows.Close()

	relations := make([]model.Relation, 0, limit)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, "", fmt.Errorf("관계 목록 행 해석: %w", err)
		}
		relation, err := parseRelation(raw, graphID)
		if err != nil {
			return nil, "", err
		}
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("관계 목록 행 읽기: %w", err)
	}
	if len(relations) <= limit {
		return relations, "", nil
	}
	relations = relations[:limit]
	return relations, encodeRelationCursor(relations[len(relations)-1].ID), nil
}

// relationFromCypher는 사건 관계 간선을 받는 모든 AGE 질의의 응답 조립 지점이다.
func (s *Store) relationFromCypher(ctx context.Context, queryer cypherQueryer, graphID model.ID, query string) (model.Relation, error) {
	var raw string
	if err := queryer.QueryRow(ctx, s.cypherSQL(query, "edge agtype")).Scan(&raw); err != nil {
		return model.Relation{}, err
	}
	return parseRelation(raw, graphID)
}

// relationLabel은 모델의 관계 유형을 AGE edge label로 고정 변환한다.
func relationLabel(relationType model.RelationType) (string, error) {
	switch relationType {
	case model.RelationTypePrecedes:
		return "PRECEDES", nil
	case model.RelationTypeCauses:
		return "CAUSES", nil
	case model.RelationTypePartOf:
		return "PART_OF", nil
	case model.RelationTypeRelatesTo:
		return "RELATES_TO", nil
	default:
		return "", fmt.Errorf("관계 유형 %q가 AGE label로 변환될 수 없다", relationType)
	}
}
