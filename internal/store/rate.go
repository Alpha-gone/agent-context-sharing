package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// TryIncrementRequestRate는 1분 고정 창 카운터를 한 문장으로 늘린다. allowed가 0이면
// 플랜 한도가 없으므로 행을 만들지 않는다.
func (s *Store) TryIncrementRequestRate(ctx context.Context, accountID model.ID, now time.Time, allowed int) (int, bool, error) {
	if !accountID.IsV7() || allowed < 0 {
		return 0, false, fmt.Errorf("요청 빈도 인자가 올바르지 않다")
	}
	if allowed == 0 {
		return 0, true, nil
	}
	window := now.UTC().Truncate(time.Minute)
	var count int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO public.request_rate (account_id, window_started_at, count)
		VALUES ($1, $2, 1)
		ON CONFLICT (account_id, window_started_at) DO UPDATE
		SET count = public.request_rate.count + 1
		WHERE public.request_rate.count < $3
		RETURNING count`, accountID.String(), window, allowed).Scan(&count)
	if err == nil {
		return count, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.pool.QueryRow(ctx, `SELECT count FROM public.request_rate WHERE account_id = $1 AND window_started_at = $2`, accountID.String(), window).Scan(&count); err != nil {
			return 0, false, fmt.Errorf("요청 빈도 현재 값 조회: %w", err)
		}
		return count, false, nil
	}
	return 0, false, fmt.Errorf("요청 빈도 증가: %w", err)
}

// OwnedGraphCount는 계정이 직접 또는 팀 상속으로 소유자 등급을 가진 활성 그래프 수를 센다.
func (s *Store) OwnedGraphCount(ctx context.Context, accountID model.ID) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		WITH owned AS (
			SELECT graph_id FROM public.graph_grant WHERE subject_type = 'account' AND subject_id = $1 AND grade = 'owner'
			UNION
			SELECT grant.graph_id FROM public.graph_grant AS grant
			JOIN public.team_member AS member ON member.team_id = grant.subject_id
			JOIN public.team ON team.team_id = member.team_id AND team.deleted_at IS NULL
			WHERE grant.subject_type = 'team' AND member.account_id = $1 AND grant.grade = 'owner'
		)
		SELECT count(*) FROM owned JOIN public.context_graph AS graph USING (graph_id) WHERE graph.deleted_at IS NULL`, accountID.String()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("소유 그래프 수 조회: %w", err)
	}
	return count, nil
}
