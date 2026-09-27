package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// RetentionDays는 그래프를 만든 계정에 적용되는 보존 기간을 돌려준다. 0은 기간을 두지
// 않는다는 계정 플랜 계약을 그대로 뜻한다.
type RetentionDays func(model.ID) int

// WithTryAdvisoryLock은 세션 단위 자문 잠금을 기다리지 않고 시도한다. 다른 인스턴스가
// 같은 작업을 실행 중이면 acquired가 false이며 실행 함수는 호출하지 않는다.
func (s *Store) WithTryAdvisoryLock(ctx context.Context, key int64, run func(context.Context) (int, error)) (count int, acquired bool, err error) {
	if run == nil {
		return 0, false, fmt.Errorf("주기 작업 실행 함수가 없다")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("주기 작업 잠금 연결 획득: %w", err)
	}
	defer conn.Release()
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		return 0, false, fmt.Errorf("주기 작업 자문 잠금 시도: %w", err)
	}
	if !acquired {
		return 0, false, nil
	}
	defer func() {
		unlockCtx := context.WithoutCancel(ctx)
		if _, unlockErr := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key); unlockErr != nil {
			// 세션 잠금은 연결이 살아 있는 동안 남는다. 풀에 돌려주면 그 연결이 잠금을 쥔 채
			// 재사용되어 다른 인스턴스가 이 작업을 계속 건너뛰므로, 연결을 닫아 풀이 폐기하게 한다.
			closeErr := conn.Conn().Close(unlockCtx)
			err = errors.Join(err, fmt.Errorf("주기 작업 자문 잠금 해제: %w", unlockErr), closeErr)
		}
	}()
	count, err = run(ctx)
	return count, true, err
}

// ExpireGrace는 유예 시작 시점에 고정한 만료 시각이 지난 활성 그래프를 자동 소프트 삭제한다.
// 한 갱신으로 판정과 삭제를 함께 해, 확인과 삭제 사이에 유예가 취소되거나 다시 시작된
// 그래프를 옛 판정으로 지우지 않는다.
func (s *Store) ExpireGrace(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, fmt.Errorf("유예 만료 기준 시각이 없다")
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE public.context_graph
		SET deleted_at = $1
		WHERE deleted_at IS NULL AND grace_expires_at IS NOT NULL AND grace_expires_at <= $1`, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("유예 만료 그래프 삭제: %w", err)
	}
	return int(result.RowsAffected()), nil
}

// CleanupExpiredProposals는 보존 기간을 지난 proposed 사건 관계를 AGE 그래프에서 지운다.
func (s *Store) CleanupExpiredProposals(ctx context.Context, now time.Time, daysFor RetentionDays) (int, error) {
	if now.IsZero() || daysFor == nil {
		return 0, fmt.Errorf("관계 후보 정리 인자가 올바르지 않다")
	}
	graphs, err := s.retentionGraphs(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, graph := range graphs {
		days := daysFor(graph.createdBy)
		if days == 0 {
			continue
		}
		cursor := ""
		for {
			relations, next, err := s.listRelations(ctx, s.pool, graph.id, model.ID{}, []model.RelationState{model.RelationStateProposed}, nil, cursor, relationScanPageSize)
			if err != nil {
				return deleted, fmt.Errorf("관계 후보 정리 목록: %w", err)
			}
			cutoff := now.AddDate(0, 0, -days)
			for _, relation := range relations {
				if relation.ProposedAt.After(cutoff) {
					continue
				}
				removed, err := s.deleteProposedRelation(ctx, graph.id, relation.ID)
				if err != nil {
					return deleted, err
				}
				if removed {
					deleted++
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return deleted, nil
}

func (s *Store) cleanupTeamAuditRecords(ctx context.Context, now time.Time, auditDaysFor RetentionDays) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT actor_account_id
		FROM public.web_audit_log
		WHERE graph_id IS NULL
	`)
	if err != nil {
		return 0, fmt.Errorf("팀 감사 로그 계정 조회: %w", err)
	}
	accountIDs := make([]model.ID, 0)
	for rows.Next() {
		var rawAccountID string
		if err := rows.Scan(&rawAccountID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("팀 감사 로그 계정 읽기: %w", err)
		}
		accountID, err := model.ParseID(rawAccountID)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("팀 감사 로그 계정 식별자: %w", err)
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("팀 감사 로그 계정 순회: %w", err)
	}
	rows.Close()

	deleted := 0
	for _, accountID := range accountIDs {
		days := auditDaysFor(accountID)
		if days <= 0 {
			continue
		}
		result, err := s.pool.Exec(ctx, `
			DELETE FROM public.web_audit_log
			WHERE graph_id IS NULL
			  AND actor_account_id = $1
			  AND occurred_at <= $2
		`, accountID.String(), now.AddDate(0, 0, -days))
		if err != nil {
			return deleted, fmt.Errorf("팀 감사 로그 정리: %w", err)
		}
		deleted += int(result.RowsAffected())
	}
	return deleted, nil
}

// CleanupAuditRecords는 적용된 관리 기록·웹 감사 기록과 거부된 관리 기록을 각각의 보존
// 계약에 맞춰 정리한다.
func (s *Store) CleanupAuditRecords(ctx context.Context, now time.Time, auditDaysFor, rejectedDaysFor RetentionDays) (int, error) {
	if now.IsZero() || auditDaysFor == nil || rejectedDaysFor == nil {
		return 0, fmt.Errorf("기록 보존 정리 인자가 올바르지 않다")
	}
	graphs, err := s.retentionGraphs(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, graph := range graphs {
		if days := auditDaysFor(graph.createdBy); days > 0 {
			cutoff := now.AddDate(0, 0, -days)
			result, err := s.pool.Exec(ctx, `DELETE FROM public.operation_log WHERE graph_id = $1 AND result = 'applied' AND applied_at <= $2`, graph.id.String(), cutoff)
			if err != nil {
				return deleted, fmt.Errorf("적용 관리 기록 정리: %w", err)
			}
			deleted += int(result.RowsAffected())
			result, err = s.pool.Exec(ctx, `DELETE FROM public.web_audit_log WHERE graph_id = $1 AND occurred_at <= $2`, graph.id.String(), cutoff)
			if err != nil {
				return deleted, fmt.Errorf("웹 감사 기록 정리: %w", err)
			}
			deleted += int(result.RowsAffected())
		}
		if days := rejectedDaysFor(graph.createdBy); days > 0 {
			result, err := s.pool.Exec(ctx, `DELETE FROM public.operation_log WHERE graph_id = $1 AND result = 'rejected' AND applied_at <= $2`, graph.id.String(), now.AddDate(0, 0, -days))
			if err != nil {
				return deleted, fmt.Errorf("거부 관리 기록 정리: %w", err)
			}
			deleted += int(result.RowsAffected())
		}
	}
	teamDeleted, err := s.cleanupTeamAuditRecords(ctx, now, auditDaysFor)
	if err != nil {
		return deleted, err
	}
	return deleted + teamDeleted, nil
}

// CleanupExpiredRevocations는 만료된 토큰 폐기 목록 행을 지운다.
func (s *Store) CleanupExpiredRevocations(ctx context.Context, now time.Time) (int, error) {
	return s.deleteBefore(ctx, "폐기 목록 정리", `DELETE FROM public.revoked_token WHERE expires_at <= $1`, now)
}

// CleanupExpiredAuthorizationCodes는 만료된 인가 코드 행을 지운다.
func (s *Store) CleanupExpiredAuthorizationCodes(ctx context.Context, now time.Time) (int, error) {
	return s.deleteBefore(ctx, "인가 코드 정리", `DELETE FROM public.authorization_code WHERE expires_at <= $1`, now)
}

// dpopProofCleanupGrace는 인스턴스 사이 시계 차이를 덮는 정리 여유다. iat 허용 창은 요청을
// 받은 인스턴스의 시계로, 정리는 작업을 맡은 인스턴스의 시계로 판정하므로 여유 없이 지우면
// 시계가 느린 인스턴스가 기록이 사라진 proof를 아직 허용 창 안으로 보고 다시 받는다.
const dpopProofCleanupGrace = time.Minute

// CleanupExpiredDPoPProofs는 60초 허용 창과 시계 차이 여유를 모두 지난 DPoP proof 재생
// 기록을 지운다.
func (s *Store) CleanupExpiredDPoPProofs(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, fmt.Errorf("DPoP proof 재생 기록 정리 기준 시각이 없다")
	}
	return s.deleteBefore(ctx, "DPoP proof 재생 기록 정리", `DELETE FROM public.dpop_proof_replay WHERE expires_at <= $1`, now.Add(-dpopProofCleanupGrace))
}

// CleanupRequestRateWindows는 현재 1분 창보다 오래된 요청 빈도 행을 지운다.
func (s *Store) CleanupRequestRateWindows(ctx context.Context, now time.Time) (int, error) {
	return s.deleteBefore(ctx, "요청 빈도 창 정리", `DELETE FROM public.request_rate WHERE window_started_at < $1`, now.UTC().Truncate(time.Minute))
}

type retentionGraph struct {
	id        model.ID
	createdBy model.ID
	deleted   bool
}

func (s *Store) retentionGraphs(ctx context.Context) ([]retentionGraph, error) {
	rows, err := s.pool.Query(ctx, `SELECT graph_id, created_by, deleted_at IS NOT NULL FROM public.context_graph`)
	if err != nil {
		return nil, fmt.Errorf("보존 기간 그래프 조회: %w", err)
	}
	defer rows.Close()
	graphs := make([]retentionGraph, 0)
	for rows.Next() {
		var rawGraphID, rawAccountID string
		var deleted bool
		if err := rows.Scan(&rawGraphID, &rawAccountID, &deleted); err != nil {
			return nil, fmt.Errorf("보존 기간 그래프 행 해석: %w", err)
		}
		graphID, err := model.ParseID(rawGraphID)
		if err != nil {
			return nil, fmt.Errorf("보존 기간 그래프 식별자: %w", err)
		}
		accountID, err := model.ParseID(rawAccountID)
		if err != nil {
			return nil, fmt.Errorf("보존 기간 생성 계정 식별자: %w", err)
		}
		graphs = append(graphs, retentionGraph{id: graphID, createdBy: accountID, deleted: deleted})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("보존 기간 그래프 행 읽기: %w", err)
	}
	return graphs, nil
}

// deleteProposedRelation은 아직 proposed인 관계만 지운다. 목록을 읽은 뒤 다른 요청이 확정하거나
// 버린 관계는 대상이 없으므로 오류가 아니라 지우지 않은 것으로 돌려준다.
func (s *Store) deleteProposedRelation(ctx context.Context, graphID, relationID model.ID) (bool, error) {
	query := "MATCH ()-[edge]->() WHERE edge.graph_id = " + cypherString(graphID.String()) +
		" AND edge.relation_id = " + cypherString(relationID.String()) +
		" AND edge.state = 'proposed' DELETE edge RETURN 1"
	var ignored string
	err := s.pool.QueryRow(ctx, s.cypherSQL(query, "deleted agtype"), pgx.QueryExecModeExec).Scan(&ignored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("관계 후보 삭제: %w", err)
	}
	return true, nil
}

func (s *Store) deleteBefore(ctx context.Context, action, query string, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, fmt.Errorf("%s 기준 시각이 없다", action)
	}
	result, err := s.pool.Exec(ctx, query, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("%s: %w", action, err)
	}
	return int(result.RowsAffected()), nil
}
