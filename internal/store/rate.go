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
	return tryIncrementRequestRate(ctx, s.pool, accountID, now, allowed)
}

// tryIncrementRequestRate는 같은 갱신을 풀과 저장 트랜잭션에서 함께 쓰도록 분리한 것이다.
//
// 「계정 플랜 값」이 이 갱신을 이미 열려 있는 저장 트랜잭션 안에서 하기로 확정했다.
// 밖에서 먼저 올리면 판 번호 충돌이나 한도 초과로 거부된 요청과 원천 중복 멱등 재요청도
// 한도를 소비한다.
func tryIncrementRequestRate(ctx context.Context, queryer cypherQueryer, accountID model.ID, now time.Time, allowed int) (int, bool, error) {
	if !accountID.IsV7() || allowed < 0 {
		return 0, false, fmt.Errorf("요청 빈도 인자가 올바르지 않다")
	}
	if allowed == 0 {
		return 0, true, nil
	}
	window := now.UTC().Truncate(time.Minute)
	var count int
	err := queryer.QueryRow(ctx, `
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
		if err := queryer.QueryRow(ctx, `SELECT count FROM public.request_rate WHERE account_id = $1 AND window_started_at = $2`, accountID.String(), window).Scan(&count); err != nil {
			return 0, false, fmt.Errorf("요청 빈도 현재 값 조회: %w", err)
		}
		return count, false, nil
	}
	return 0, false, fmt.Errorf("요청 빈도 증가: %w", err)
}

// OwnedGraphCount는 계정이 직접 또는 팀 상속으로 소유자 등급을 가진 활성 그래프 수를 센다.
func (s *Store) OwnedGraphCount(ctx context.Context, accountID model.ID) (int, error) {
	count, err := ownedGraphCount(ctx, s.pool, accountID)
	return int(count), err
}

// ownedGraphCount는 같은 질의를 풀과 트랜잭션에서 함께 쓰도록 분리한 것이다. 생성
// 트랜잭션은 이 값을 그 안에서 세어야 누적 한도가 동시 요청에도 성립한다.
func ownedGraphCount(ctx context.Context, queryer cypherQueryer, accountID model.ID) (int64, error) {
	var count int64
	err := queryer.QueryRow(ctx, `
		WITH owned AS (
			SELECT graph_id FROM public.graph_grant WHERE subject_type = 'account' AND subject_id = $1 AND grade = 'owner'
			UNION
			SELECT team_grant.graph_id FROM public.graph_grant AS team_grant
			JOIN public.team_member AS member ON member.team_id = team_grant.subject_id
			JOIN public.team ON team.team_id = member.team_id AND team.deleted_at IS NULL
			WHERE team_grant.subject_type = 'team' AND member.account_id = $1 AND team_grant.grade = 'owner'
		)
		SELECT count(*) FROM owned JOIN public.context_graph AS graph USING (graph_id) WHERE graph.deleted_at IS NULL`, accountID.String()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("소유 그래프 수 조회: %w", err)
	}
	return count, nil
}
