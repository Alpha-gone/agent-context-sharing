package store

import (
	"context"
	"fmt"

	"agent_context_sharing/internal/model"
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
