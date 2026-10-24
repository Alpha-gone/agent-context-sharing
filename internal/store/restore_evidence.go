package store

import (
	"context"
	"fmt"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 팀 상속은 직접 부여와 구분하여 구성원의 과거 소유자 자격만 증명한다.
// 호출자는 팀과 연결 그래프 잠금을 소유하며, 감사 실패 시 원래 변경도 롤백한다.
func (s *Store) recordInheritedOwners(ctx context.Context, tx pgx.Tx, graphs []model.ID, teamID, actorID, memberID model.ID) error {
	var memberFilter any
	if memberID.IsV7() {
		memberFilter = memberID.String()
	}
	for _, graph := range graphs {
		rows, err := tx.Query(ctx, `SELECT member.account_id::text FROM public.team_member AS member
			JOIN public.team AS team ON team.team_id=member.team_id AND team.deleted_at IS NULL
			JOIN public.graph_grant AS permission ON permission.subject_type='team' AND permission.subject_id=team.team_id
			WHERE team.team_id=$1 AND permission.graph_id=$2 AND permission.grade='owner' AND ($3::uuid IS NULL OR member.account_id=$3)`, teamID.String(), graph.String(), memberFilter)
		if err != nil {
			return fmt.Errorf("상속 소유자 근거 조회: %w", err)
		}
		var members []model.ID
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return fmt.Errorf("상속 소유자 근거 읽기: %w", err)
			}
			id, err := model.ParseID(raw)
			if err != nil {
				rows.Close()
				return fmt.Errorf("상속 소유자 식별자: %w", err)
			}
			members = append(members, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("상속 소유자 근거 순회: %w", err)
		}
		for _, member := range members {
			if err := s.insertWebAudit(ctx, tx, webAuditRecord{TargetKind: "grant", Action: "grant", ActorID: actorID,
				GraphID: graph, SubjectType: GrantSubjectTeam, SubjectID: teamID, TeamID: teamID,
				MemberAccountID: member, AfterGrade: model.GraphGradeOwner}); err != nil {
				return err
			}
		}
	}
	return nil
}

// 같은 시각은 감사 ID로 순서를 고정한다. 목록·실제 복구·보존 정리가 같은 판정을 쓴다.
const laterOperatorRestoreSQL = `SELECT 1 FROM public.web_audit_log AS restored
	WHERE restored.target_kind='operator_restore' AND restored.graph_id=request.graph_id
	AND (restored.occurred_at, restored.audit_id) > (request.occurred_at, request.audit_id)`

const formerOwnerSQL = `(graph.created_by=$1 OR EXISTS (
	SELECT 1 FROM public.web_audit_log AS evidence WHERE evidence.graph_id=graph.graph_id AND evidence.after_grade='owner'
	AND ((evidence.subject_type='account' AND evidence.subject_id=$1)
	OR (evidence.subject_type='team' AND evidence.member_account_id=$1))))`
