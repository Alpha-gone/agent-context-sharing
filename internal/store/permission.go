package store

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// GrantSubjectType은 그래프 등급을 직접 부여받는 주체의 종류다.
type GrantSubjectType string

const (
	// GrantSubjectAccount는 계정에 직접 부여한 그래프 등급이다.
	GrantSubjectAccount GrantSubjectType = "account"
	// GrantSubjectTeam은 팀 구성원에게 상속되는 그래프 등급이다.
	GrantSubjectTeam GrantSubjectType = "team"
)

// EffectiveGrade는 직접 부여와 활성 팀의 상속 부여를 한 질의로 합쳐 최고 등급을 계산한다.
// 소프트 삭제한 그래프는 소유자가 웹에서 복구할 수 있도록 여기서 제외하지 않는다. 자동
// 삭제 그래프에는 접근 가능한 계정이 없으므로 유효 등급도 남지 않는다. 삭제 상태에 따른
// 화면과 연산의 가용성은 이 계산과 별도로 각 조회·처리 경로가 판정한다. 부여가 하나도
// 없으면 집계가 NULL을 돌려주므로 등급을 nullable로 받아 부재와 구분한다.
func (s *Store) EffectiveGrade(ctx context.Context, graphID, accountID model.ID) (model.GraphGrade, bool, error) {
	var grade *string
	err := s.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT grade FROM public.graph_grant WHERE graph_id = $1 AND subject_type = 'account' AND subject_id = $2
			UNION ALL
			SELECT team_grant.grade FROM public.graph_grant AS team_grant
			JOIN public.team_member AS member ON member.team_id = team_grant.subject_id
			JOIN public.team ON team.team_id = member.team_id AND team.deleted_at IS NULL
			WHERE team_grant.graph_id = $1 AND team_grant.subject_type = 'team' AND member.account_id = $2
		)
		SELECT CASE MAX(CASE grade WHEN 'owner' THEN 3 WHEN 'editor' THEN 2 WHEN 'viewer' THEN 1 END)
			WHEN 3 THEN 'owner' WHEN 2 THEN 'editor' WHEN 1 THEN 'viewer' END
		FROM candidate`, graphID.String(), accountID.String()).Scan(&grade)
	if err != nil {
		return "", false, fmt.Errorf("유효 등급 조회: %w", err)
	}
	if grade == nil {
		return "", false, nil
	}
	value := model.GraphGrade(*grade)
	if !value.Valid() {
		return "", false, fmt.Errorf("저장된 유효 등급 %q가 올바르지 않다", *grade)
	}
	return value, true, nil
}

// RevokeGraphGrant는 그래프 행을 잠근 뒤 직접 또는 팀 등급을 회수한다. 마지막 소유자
// 등급은 회수하지 않으며, 마지막 접근 가능 계정이 사라지면 유예를 시작한다.
func (s *Store) RevokeGraphGrant(ctx context.Context, graphID model.ID, subjectType GrantSubjectType, subjectID model.ID) error {
	if !graphID.IsV7() || !subjectID.IsV7() || !subjectType.valid() {
		return fmt.Errorf("그래프 등급 회수 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("그래프 등급 회수 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	graphIDs, err := lockGraphs(ctx, tx, []model.ID{graphID})
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `
		DELETE FROM public.graph_grant
		WHERE graph_id = $1 AND subject_type = $2 AND subject_id = $3`,
		graphID.String(), string(subjectType), subjectID.String())
	if err != nil {
		return fmt.Errorf("그래프 등급 회수: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := requireOwner(ctx, tx, graphID); err != nil {
		return err
	}
	if err := startGraceForDisconnectedGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("그래프 등급 회수 커밋: %w", err)
	}
	return nil
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
	defer tx.Rollback(ctx)

	graphIDs, err := teamGraphIDs(ctx, tx, teamID)
	if err != nil {
		return err
	}
	graphIDs, err = lockGraphs(ctx, tx, graphIDs)
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
	if err := startGraceForDisconnectedGraphs(ctx, tx, graphIDs); err != nil {
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
	defer tx.Rollback(ctx)

	graphIDs, err := teamGraphIDs(ctx, tx, teamID)
	if err != nil {
		return err
	}
	graphIDs, err = lockGraphs(ctx, tx, graphIDs)
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
	if err := startGraceForDisconnectedGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("팀 삭제 커밋: %w", err)
	}
	return nil
}

func (subjectType GrantSubjectType) valid() bool {
	return subjectType == GrantSubjectAccount || subjectType == GrantSubjectTeam
}

func teamGraphIDs(ctx context.Context, tx pgx.Tx, teamID model.ID) ([]model.ID, error) {
	rows, err := tx.Query(ctx, `
		SELECT graph.graph_id
		FROM public.context_graph AS graph
		JOIN public.graph_grant AS team_grant ON team_grant.graph_id = graph.graph_id
		WHERE team_grant.subject_type = 'team' AND team_grant.subject_id = $1
		ORDER BY graph.graph_id`, teamID.String())
	if err != nil {
		return nil, fmt.Errorf("팀 영향 그래프 조회: %w", err)
	}
	defer rows.Close()

	var graphIDs []model.ID
	for rows.Next() {
		var rawID string
		if err := rows.Scan(&rawID); err != nil {
			return nil, fmt.Errorf("팀 영향 그래프 해석: %w", err)
		}
		graphID, err := model.ParseID(rawID)
		if err != nil {
			return nil, fmt.Errorf("저장된 graph_id: %w", err)
		}
		graphIDs = append(graphIDs, graphID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("팀 영향 그래프 행 읽기: %w", err)
	}
	return graphIDs, nil
}

// lockGraphs는 판정 단위인 그래프 행을 graph_id 오름차순으로 잠근다. 여러 팀 변경이
// 같은 그래프 집합을 건드려도 순서가 같아 교착을 만들지 않는다.
func lockGraphs(ctx context.Context, tx pgx.Tx, graphIDs []model.ID) ([]model.ID, error) {
	graphIDs = sortedUniqueGraphIDs(graphIDs)
	if len(graphIDs) == 0 {
		return nil, nil
	}
	rawIDs := make([]string, len(graphIDs))
	for index, graphID := range graphIDs {
		rawIDs[index] = graphID.String()
	}
	rows, err := tx.Query(ctx, `
		SELECT graph_id
		FROM public.context_graph
		WHERE graph_id = ANY($1::uuid[])
		ORDER BY graph_id
		FOR UPDATE`, rawIDs)
	if err != nil {
		return nil, fmt.Errorf("그래프 행 잠금: %w", err)
	}
	defer rows.Close()

	locked := make([]model.ID, 0, len(graphIDs))
	for rows.Next() {
		var rawID string
		if err := rows.Scan(&rawID); err != nil {
			return nil, fmt.Errorf("잠근 그래프 행 해석: %w", err)
		}
		graphID, err := model.ParseID(rawID)
		if err != nil {
			return nil, fmt.Errorf("저장된 graph_id: %w", err)
		}
		locked = append(locked, graphID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("그래프 행 잠금 읽기: %w", err)
	}
	if len(locked) != len(graphIDs) {
		return nil, ErrNotFound
	}
	return locked, nil
}

func sortedUniqueGraphIDs(graphIDs []model.ID) []model.ID {
	ids := slices.Clone(graphIDs)
	slices.SortFunc(ids, func(left, right model.ID) int { return bytes.Compare(left[:], right[:]) })
	unique := ids[:0]
	for _, graphID := range ids {
		if len(unique) == 0 || graphID != unique[len(unique)-1] {
			unique = append(unique, graphID)
		}
	}
	return unique
}

func requireOwner(ctx context.Context, tx pgx.Tx, graphID model.ID) error {
	var owners int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.graph_grant WHERE graph_id = $1 AND grade = 'owner'`, graphID.String()).Scan(&owners); err != nil {
		return fmt.Errorf("그래프 소유자 수 조회: %w", err)
	}
	if owners == 0 {
		return ErrLastOwner
	}
	return nil
}

func startGraceForDisconnectedGraphs(ctx context.Context, tx pgx.Tx, graphIDs []model.ID) error {
	disconnected := make([]string, 0, len(graphIDs))
	for _, graphID := range graphIDs {
		var accounts int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT subject_id AS account_id
				FROM public.graph_grant
				WHERE graph_id = $1 AND subject_type = 'account'
				UNION
				SELECT member.account_id
				FROM public.graph_grant AS team_grant
				JOIN public.team_member AS member ON member.team_id = team_grant.subject_id
				JOIN public.team ON team.team_id = member.team_id AND team.deleted_at IS NULL
				WHERE team_grant.graph_id = $1 AND team_grant.subject_type = 'team'
			) AS accessible`, graphID.String()).Scan(&accounts)
		if err != nil {
			return fmt.Errorf("접근 가능한 계정 수 조회: %w", err)
		}
		if accounts != 0 {
			continue
		}
		disconnected = append(disconnected, graphID.String())
	}
	if len(disconnected) == 0 {
		return nil
	}
	// 모든 판정을 끝낸 뒤 커밋 직전 한 갱신으로 기록하며, 이미 시작한 유예는 덮어쓰지 않는다.
	if _, err := tx.Exec(ctx, `
		UPDATE public.context_graph
		SET grace_started_at = clock_timestamp()
		WHERE graph_id = ANY($1::uuid[]) AND deleted_at IS NULL AND grace_started_at IS NULL`, disconnected); err != nil {
		return fmt.Errorf("유예 시작 시각 기록: %w", err)
	}
	return nil
}
