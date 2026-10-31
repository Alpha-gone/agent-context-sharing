package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 이 파일의 진입점은 저장소 패키지 시험의 준비·대조에만 쓴다. 운영 경로는 인가·감사·한도를
// 함께 적용하는 진입점(ConfirmRelation, RemoveTeamMemberWithAudit, SetTeamDeleted,
// ExecuteWrite 등)을 사용하며, 시험 파일에 두어 운영 빌드에서 우회 경로로 쓰이지 않게 한다.

// CreateRelation은 같은 그래프의 사건 두 개를 잇는 사건 관계 간선을 만든다.
func (s *Store) CreateRelation(ctx context.Context, graphID model.ID, relation model.Relation) (result model.Relation, err error) {
	if !graphID.IsV7() || relation.GraphID != graphID {
		return model.Relation{}, fmt.Errorf("요청 graph_id와 관계 graph_id가 일치하지 않는다")
	}
	tx, err := s.writeTransaction(ctx)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 생성 트랜잭션 시작: %w", err)
	}
	defer finishWrite(ctx, tx, "CreateRelation", &err)
	if err := requireActiveGraph(ctx, tx, graphID); err != nil {
		return model.Relation{}, err
	}

	from, err := s.context(ctx, tx, graphID, relation.FromContextID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 시작 사건 조회: %w", err)
	}
	to, err := s.context(ctx, tx, graphID, relation.ToContextID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 대상 사건 조회: %w", err)
	}
	if err := model.ValidateRelation(relation, from, to); err != nil {
		return model.Relation{}, fmt.Errorf("관계 검증: %w", errors.Join(ErrInvalidRelation, err))
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
		" CREATE (from)-[edge:" + label + " " + properties + "]->(to) RETURN edge, startNode(edge), endNode(edge)"
	stored, err := s.relationFromCypher(ctx, tx, graphID, query)
	if err != nil {
		return model.Relation{}, fmt.Errorf("사건 관계 생성: %w", err)
	}
	if err := s.checkWriteInvariants(ctx, tx, graphID, []model.ID{relation.FromContextID, relation.ToContextID}, EmbeddingExpectation{}); err != nil {
		return model.Relation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Relation{}, fmt.Errorf("관계 생성 커밋: %w", err)
	}
	return stored, nil
}

// HasAppliedDiscard는 현재 삭제에 대응하는 MCP 폐기 기록이 있는지 확인한다.
// 상태 변경의 허용 판정은 RestoreContext가 정점 잠금 뒤 같은 트랜잭션에서 다시 수행한다.
func (s *Store) HasAppliedDiscard(ctx context.Context, graphID, contextID model.ID) (bool, error) {
	if !graphID.IsV7() || !contextID.IsV7() {
		return false, fmt.Errorf("그래프와 컨텍스트 식별자는 UUIDv7이어야 한다")
	}
	return s.hasAppliedDiscard(ctx, s.pool, graphID, contextID)
}

// RemoveTeamMember는 팀의 모든 영향 그래프를 정해진 순서로 잠근 뒤 구성원을 제거하고,
// 마지막 접근 가능 계정이 사라진 그래프의 유예를 시작한다.
func (s *Store) RemoveTeamMember(ctx context.Context, teamID, accountID model.ID) error {
	if !teamID.IsV7() || !accountID.IsV7() {
		return fmt.Errorf("팀 구성원 제거 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("팀 구성원 제거 트랜잭션 시작: %w", err)
	}
	defer rollbackTransaction(ctx, tx)

	graphIDs, err := lockTeamGraphs(ctx, tx, teamID)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `DELETE FROM public.team_member WHERE team_id = $1 AND account_id = $2`, teamID.String(), accountID.String())
	if err != nil {
		return fmt.Errorf("팀 구성원 제거: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := s.startGraceForDisconnectedGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("팀 구성원 제거 커밋: %w", err)
	}
	return nil
}

// DeleteTeam은 팀의 모든 영향 그래프를 정해진 순서로 잠근 뒤 팀을 소프트 삭제하고,
// 마지막 접근 가능 계정이 사라진 그래프의 유예를 시작한다.
func (s *Store) DeleteTeam(ctx context.Context, teamID model.ID) error {
	if !teamID.IsV7() {
		return fmt.Errorf("팀 삭제 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("팀 삭제 트랜잭션 시작: %w", err)
	}
	defer rollbackTransaction(ctx, tx)

	graphIDs, err := lockTeamGraphs(ctx, tx, teamID)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE public.team SET deleted_at = clock_timestamp() WHERE team_id = $1 AND deleted_at IS NULL`, teamID.String())
	if err != nil {
		return fmt.Errorf("팀 삭제: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := s.startGraceForDisconnectedGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("팀 삭제 커밋: %w", err)
	}
	return nil
}

// ReplayIdempotent는 기존 UUID 호출의 호환 진입점이다. 새 MCP 호출은
// 문자열 키·현재 권한 재검사를 필수로 받는 ExecuteWrite를 사용한다.
func (s *Store) ReplayIdempotent(ctx context.Context, request IdempotencyRequest, run func(context.Context) ([]byte, error)) ([]byte, error) {
	if !request.AccountID.IsV7() || !request.Key.IsV7() || request.ToolName == "" || run == nil {
		return nil, fmt.Errorf("멱등성 요청 인자가 올바르지 않다")
	}
	return s.replayWrite(ctx, request.AccountID, request.Key.String(), request.ToolName, request.Fingerprint, func(context.Context) error { return nil }, run)
}

// TryIncrementRequestRate는 1분 고정 창 카운터를 한 문장으로 늘린다. allowed가 0이면
// 플랜 한도가 없으므로 행을 만들지 않는다.
func (s *Store) TryIncrementRequestRate(ctx context.Context, accountID model.ID, now time.Time, allowed int) (int, bool, error) {
	count, accepted, err := tryIncrementRequestRate(ctx, s.pool, accountID, now, int64(allowed))
	return int(count), accepted, err
}

// OwnedGraphCount는 계정이 직접 또는 팀 상속으로 소유자 등급을 가진 활성 그래프 수를 센다.
func (s *Store) OwnedGraphCount(ctx context.Context, accountID model.ID) (int, error) {
	count, err := ownedGraphCount(ctx, s.pool, accountID)
	return int(count), err
}
