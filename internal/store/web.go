package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// GrantGraph은 계정 또는 팀의 그래프 등급을 만들거나 바꾸고 웹 감사 기록을 남긴다.
func (s *Store) GrantGraph(ctx context.Context, graphID, actorID, subjectID model.ID, subjectType GrantSubjectType, grade model.GraphGrade) error {
	if !graphID.IsV7() || !actorID.IsV7() || !subjectID.IsV7() || !subjectType.valid() || !grade.Valid() {
		return fmt.Errorf("그래프 등급 부여 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("그래프 등급 부여 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := lockGraphs(ctx, tx, []model.ID{graphID}); err != nil {
		return err
	}
	var before *string
	if err := tx.QueryRow(ctx, `SELECT grade FROM public.graph_grant WHERE graph_id = $1 AND subject_type = $2 AND subject_id = $3`, graphID.String(), subjectType, subjectID.String()).Scan(&before); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("기존 그래프 등급 조회: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.graph_grant (graph_id, subject_type, subject_id, grade)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (graph_id, subject_type, subject_id) DO UPDATE SET grade = EXCLUDED.grade`,
		graphID.String(), subjectType, subjectID.String(), grade); err != nil {
		return fmt.Errorf("그래프 등급 부여: %w", err)
	}
	if err := cancelGraceForConnectedGraphs(ctx, tx, []model.ID{graphID}); err != nil {
		return err
	}
	action, targetKind := "grant", "grant"
	if before != nil && *before == "owner" && grade != model.GraphGradeOwner {
		action, targetKind = "transfer", "ownership_transfer"
	}
	return s.commitWebAudit(ctx, tx, webAuditRecord{TargetKind: targetKind, Action: action, ActorID: actorID, GraphID: graphID, SubjectType: subjectType, SubjectID: subjectID, BeforeGrade: nullableGrade(before), AfterGrade: grade})
}

// RevokeGraphGrantWithAudit은 마지막 소유자를 보호하면서 등급을 회수하고 감사 기록을 남긴다.
func (s *Store) RevokeGraphGrantWithAudit(ctx context.Context, graphID, actorID, subjectID model.ID, subjectType GrantSubjectType) error {
	if !graphID.IsV7() || !actorID.IsV7() || !subjectID.IsV7() || !subjectType.valid() {
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
	var before string
	if err := tx.QueryRow(ctx, `DELETE FROM public.graph_grant WHERE graph_id = $1 AND subject_type = $2 AND subject_id = $3 RETURNING grade`, graphID.String(), subjectType, subjectID.String()).Scan(&before); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("그래프 등급 회수: %w", err)
	}
	if err := requireOwner(ctx, tx, graphID); err != nil {
		return err
	}
	if err := s.startGraceForDisconnectedGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	return s.commitWebAudit(ctx, tx, webAuditRecord{TargetKind: "grant", Action: "revoke", ActorID: actorID, GraphID: graphID, SubjectType: subjectType, SubjectID: subjectID, BeforeGrade: model.GraphGrade(before)})
}

// ListGraphGrants는 직접 부여와 팀 상속 등급을 구분해 웹에 돌려준다.
func (s *Store) ListGraphGrants(ctx context.Context, graphID model.ID) ([]model.GrantSubject, error) {
	if !graphID.IsV7() {
		return nil, fmt.Errorf("그래프 식별자가 UUIDv7이 아니다")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT permission.subject_type, permission.subject_id, COALESCE(account.login_id, team.name), permission.grade, false
		FROM public.graph_grant AS permission
		LEFT JOIN public.account AS account ON permission.subject_type = 'account' AND account.account_id = permission.subject_id
		LEFT JOIN public.team AS team ON permission.subject_type = 'team' AND team.team_id = permission.subject_id
		WHERE permission.graph_id = $1
		UNION ALL
		SELECT 'account', member.account_id, account.login_id, team_grant.grade, true
		FROM public.graph_grant AS team_grant
		JOIN public.team AS team ON team.team_id = team_grant.subject_id AND team.deleted_at IS NULL
		JOIN public.team_member AS member ON member.team_id = team.team_id
		JOIN public.account AS account ON account.account_id = member.account_id
		WHERE team_grant.graph_id = $1 AND team_grant.subject_type = 'team'
		ORDER BY 5, 1, 3`, graphID.String())
	if err != nil {
		return nil, fmt.Errorf("그래프 등급 목록 조회: %w", err)
	}
	defer rows.Close()
	grants := make([]model.GrantSubject, 0)
	for rows.Next() {
		var rawID, grade string
		var grant model.GrantSubject
		if err := rows.Scan(&grant.Type, &rawID, &grant.Name, &grade, &grant.Inherited); err != nil {
			return nil, fmt.Errorf("그래프 등급 목록 해석: %w", err)
		}
		var err error
		grant.ID, err = model.ParseID(rawID)
		if err != nil {
			return nil, fmt.Errorf("그래프 등급 대상 식별자: %w", err)
		}
		grant.Grade = model.GraphGrade(grade)
		if !grant.Grade.Valid() {
			return nil, fmt.Errorf("저장된 그래프 등급 %q가 올바르지 않다", grade)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("그래프 등급 목록 행 읽기: %w", err)
	}
	var owners int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.graph_grant WHERE graph_id = $1 AND grade = 'owner'`, graphID.String()).Scan(&owners); err != nil {
		return nil, fmt.Errorf("그래프 소유자 수 조회: %w", err)
	}
	for index := range grants {
		grants[index].CanRevoke = !grants[index].Inherited && (grants[index].Grade != model.GraphGradeOwner || owners > 1)
	}
	return grants, nil
}

// CreateTeam은 인증된 계정을 팀 관리자로 하여 팀을 만들고 감사 기록을 남긴다.
func (s *Store) CreateTeam(ctx context.Context, actorID model.ID, name string) (model.Team, error) {
	if !actorID.IsV7() || name == "" {
		return model.Team{}, fmt.Errorf("팀 생성 인자가 올바르지 않다")
	}
	teamID, err := model.NewID()
	if err != nil {
		return model.Team{}, fmt.Errorf("팀 식별자 생성: %w", err)
	}
	now := time.Now().UTC()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Team{}, fmt.Errorf("팀 생성 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO public.team (team_id, name, manager_account_id, created_at) VALUES ($1, $2, $3, $4)`, teamID.String(), name, actorID.String(), now); err != nil {
		return model.Team{}, fmt.Errorf("팀 생성: %w", err)
	}
	if err := s.insertWebAudit(ctx, tx, webAuditRecord{TargetKind: "team", Action: "add", ActorID: actorID, TeamID: teamID}); err != nil {
		return model.Team{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Team{}, fmt.Errorf("팀 생성 커밋: %w", err)
	}
	return model.Team{ID: teamID, Name: name, ManagerAccountID: actorID, CreatedAt: now}, nil
}

// ListManagedTeams는 요청 계정이 관리하는 활성·삭제 팀을 모두 읽는다.
func (s *Store) ListManagedTeams(ctx context.Context, managerID model.ID) ([]model.Team, error) {
	if !managerID.IsV7() {
		return nil, fmt.Errorf("팀 관리자 식별자가 UUIDv7이 아니다")
	}
	rows, err := s.pool.Query(ctx, `SELECT team_id, name, manager_account_id, created_at, deleted_at FROM public.team WHERE manager_account_id = $1 ORDER BY name, team_id`, managerID.String())
	if err != nil {
		return nil, fmt.Errorf("관리 팀 목록 조회: %w", err)
	}
	defer rows.Close()
	teams := make([]model.Team, 0)
	for rows.Next() {
		team, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		teams = append(teams, team)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("관리 팀 목록 행 읽기: %w", err)
	}
	return teams, nil
}

// AddTeamMember는 팀 관리자만 호출하도록 전송 계층이 확인한 뒤 구성원을 추가한다.
func (s *Store) AddTeamMember(ctx context.Context, teamID, actorID, accountID model.ID) error {
	if !teamID.IsV7() || !actorID.IsV7() || !accountID.IsV7() {
		return fmt.Errorf("팀 구성원 추가 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("팀 구성원 추가 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := requireTeamManager(ctx, tx, teamID, actorID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.team_member (team_id, account_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, teamID.String(), accountID.String()); err != nil {
		return fmt.Errorf("팀 구성원 추가: %w", err)
	}
	graphIDs, err := teamGraphIDs(ctx, tx, teamID)
	if err != nil {
		return err
	}
	if _, err := lockGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	if err := cancelGraceForConnectedGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	return s.commitWebAudit(ctx, tx, webAuditRecord{TargetKind: "team", Action: "add", ActorID: actorID, TeamID: teamID, MemberAccountID: accountID})
}

// RemoveTeamMemberWithAudit은 팀 관리자 확인 뒤 구성원을 빼고 감사 기록을 남긴다.
func (s *Store) RemoveTeamMemberWithAudit(ctx context.Context, teamID, actorID, accountID model.ID) error {
	if !teamID.IsV7() || !actorID.IsV7() || !accountID.IsV7() {
		return fmt.Errorf("팀 구성원 제거 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("팀 구성원 제거 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := requireTeamManager(ctx, tx, teamID, actorID); err != nil {
		return err
	}
	graphIDs, err := teamGraphIDs(ctx, tx, teamID)
	if err != nil {
		return err
	}
	if _, err := lockGraphs(ctx, tx, graphIDs); err != nil {
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
	return s.commitWebAudit(ctx, tx, webAuditRecord{TargetKind: "team", Action: "remove", ActorID: actorID, TeamID: teamID, MemberAccountID: accountID})
}

// SetTeamDeleted는 팀 관리자가 팀을 소프트 삭제하거나 복구하고 감사 기록을 남긴다.
func (s *Store) SetTeamDeleted(ctx context.Context, teamID, actorID model.ID, deleted bool) error {
	if !teamID.IsV7() || !actorID.IsV7() {
		return fmt.Errorf("팀 상태 변경 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("팀 상태 변경 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := requireTeamManager(ctx, tx, teamID, actorID); err != nil {
		return err
	}
	graphIDs, err := teamGraphIDs(ctx, tx, teamID)
	if err != nil {
		return err
	}
	if _, err := lockGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	query := `UPDATE public.team SET deleted_at = NULL WHERE team_id = $1 AND deleted_at IS NOT NULL`
	action := "restore"
	if deleted {
		query, action = `UPDATE public.team SET deleted_at = clock_timestamp() WHERE team_id = $1 AND deleted_at IS NULL`, "delete"
	}
	result, err := tx.Exec(ctx, query, teamID.String())
	if err != nil {
		return fmt.Errorf("팀 상태 변경: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrInvalidState
	}
	if deleted {
		if err := s.startGraceForDisconnectedGraphs(ctx, tx, graphIDs); err != nil {
			return err
		}
	} else if err := cancelGraceForConnectedGraphs(ctx, tx, graphIDs); err != nil {
		return err
	}
	return s.commitWebAudit(ctx, tx, webAuditRecord{TargetKind: "team", Action: action, ActorID: actorID, TeamID: teamID})
}

type webAuditRecord struct {
	TargetKind, Action                                                                        string
	ActorID, GraphID, SubjectID, TeamID, MemberAccountID, TargetContextID, RequesterAccountID model.ID
	SubjectType                                                                               GrantSubjectType
	BeforeGrade, AfterGrade                                                                   model.GraphGrade
}

func (s *Store) commitWebAudit(ctx context.Context, tx pgx.Tx, record webAuditRecord) error {
	if err := s.insertWebAudit(ctx, tx, record); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("웹 관리 변경 커밋: %w", err)
	}
	return nil
}

func (s *Store) insertWebAudit(ctx context.Context, tx pgx.Tx, record webAuditRecord) error {
	auditID, err := model.NewID()
	if err != nil {
		return fmt.Errorf("웹 감사 기록 식별자 생성: %w", err)
	}
	var graphID, subjectID, teamID, memberID, contextID, requesterID *string
	if record.GraphID.IsV7() {
		value := record.GraphID.String()
		graphID = &value
	}
	if record.SubjectID.IsV7() {
		value := record.SubjectID.String()
		subjectID = &value
	}
	if record.TeamID.IsV7() {
		value := record.TeamID.String()
		teamID = &value
	}
	if record.MemberAccountID.IsV7() {
		value := record.MemberAccountID.String()
		memberID = &value
	}
	if record.TargetContextID.IsV7() {
		value := record.TargetContextID.String()
		contextID = &value
	}
	if record.RequesterAccountID.IsV7() {
		value := record.RequesterAccountID.String()
		requesterID = &value
	}
	var subjectType, before, after *string
	if record.SubjectType.valid() {
		value := string(record.SubjectType)
		subjectType = &value
	}
	if record.BeforeGrade.Valid() {
		value := string(record.BeforeGrade)
		before = &value
	}
	if record.AfterGrade.Valid() {
		value := string(record.AfterGrade)
		after = &value
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.web_audit_log (audit_id, target_kind, action, actor_account_id, occurred_at, graph_id, subject_type, subject_id, before_grade, after_grade, team_id, member_account_id, target_context_id, requester_account_id)
		VALUES ($1, $2, $3, $4, clock_timestamp(), $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		auditID.String(), record.TargetKind, record.Action, record.ActorID.String(), graphID, subjectType, subjectID, before, after, teamID, memberID, contextID, requesterID); err != nil {
		return fmt.Errorf("웹 감사 기록: %w", err)
	}
	return nil
}

func requireTeamManager(ctx context.Context, tx pgx.Tx, teamID, accountID model.ID) error {
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.team WHERE team_id = $1 AND manager_account_id = $2)`, teamID.String(), accountID.String()).Scan(&found); err != nil {
		return fmt.Errorf("팀 관리자 확인: %w", err)
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

func scanTeam(row pgx.Row) (model.Team, error) {
	var rawID, rawManager string
	var team model.Team
	if err := row.Scan(&rawID, &team.Name, &rawManager, &team.CreatedAt, &team.DeletedAt); err != nil {
		return model.Team{}, fmt.Errorf("팀 행 해석: %w", err)
	}
	var err error
	team.ID, err = model.ParseID(rawID)
	if err != nil {
		return model.Team{}, fmt.Errorf("팀 식별자: %w", err)
	}
	team.ManagerAccountID, err = model.ParseID(rawManager)
	if err != nil {
		return model.Team{}, fmt.Errorf("팀 관리자 식별자: %w", err)
	}
	team.CreatedAt = team.CreatedAt.UTC()
	if team.DeletedAt != nil {
		team.DeletedAt = new(team.DeletedAt.UTC())
	}
	return team, nil
}

func nullableGrade(value *string) model.GraphGrade {
	if value == nil {
		return ""
	}
	return model.GraphGrade(*value)
}

// DeletionImpact은 그래프나 컨텍스트를 지우기 전에 화면에 보여 줄 영향 수를 계산한다.
// 삭제 대상만 바꾸며 이 값에 포함된 항목을 연쇄 삭제하지 않는다.
func (s *Store) DeletionImpact(ctx context.Context, graphID, contextID model.ID) (model.DeletionImpact, error) {
	if !graphID.IsV7() || (!contextID.IsZero() && !contextID.IsV7()) {
		return model.DeletionImpact{}, fmt.Errorf("삭제 영향 조회 인자가 올바르지 않다")
	}
	if contextID.IsZero() {
		var impact model.DeletionImpact
		err := s.pool.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM "`+s.graphName+`"."Context" WHERE properties ->> 'graph_id'::text = $1 AND properties ->> 'deleted_at'::text IS NULL),
				(SELECT count(*) FROM public.graph_grant WHERE graph_id = $1::uuid AND subject_type = 'account') +
				(SELECT count(DISTINCT member.account_id) FROM public.graph_grant AS team_grant JOIN public.team_member AS member ON member.team_id = team_grant.subject_id JOIN public.team ON team.team_id = member.team_id AND team.deleted_at IS NULL WHERE team_grant.graph_id = $1::uuid AND team_grant.subject_type = 'team')`, graphID.String()).Scan(&impact.Contexts, &impact.Accounts)
		if err != nil {
			return model.DeletionImpact{}, fmt.Errorf("그래프 삭제 영향 조회: %w", err)
		}
		relations, err := s.confirmedRelationCount(ctx, graphID, model.ID{})
		if err != nil {
			return model.DeletionImpact{}, err
		}
		impact.Relations = relations
		return impact, nil
	}
	value, err := s.Context(ctx, graphID, contextID)
	if err != nil {
		return model.DeletionImpact{}, err
	}
	var impact model.DeletionImpact
	switch value.Layer {
	case model.LayerSource, model.LayerDerived:
		query := "MATCH (derived:Context)-[edge:DERIVED_FROM]->(source:Context) WHERE source.context_id = " + cypherString(contextID.String()) + " AND source.graph_id = " + cypherString(graphID.String()) + " AND derived.graph_id = " + cypherString(graphID.String()) + " AND edge.graph_id = " + cypherString(graphID.String()) + " RETURN count(derived)"
		count, err := s.agtypeCount(ctx, query)
		if err != nil {
			return model.DeletionImpact{}, fmt.Errorf("근거 삭제 영향 조회: %w", err)
		}
		impact.Derived = count
	case model.LayerEvent:
		relations, err := s.confirmedRelationCount(ctx, graphID, contextID)
		if err != nil {
			return model.DeletionImpact{}, err
		}
		impact.Relations = relations
	}
	return impact, nil
}

func (s *Store) confirmedRelationCount(ctx context.Context, graphID, contextID model.ID) (int, error) {
	query := "MATCH (from:Context)-[edge]->(to:Context) WHERE edge.graph_id = " + cypherString(graphID.String()) + " AND edge.relation_type IS NOT NULL AND edge.state = 'confirmed'"
	if contextID.IsV7() {
		query += " AND (from.context_id = " + cypherString(contextID.String()) + " OR to.context_id = " + cypherString(contextID.String()) + ")"
	}
	query += " RETURN count(edge)"
	count, err := s.agtypeCount(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("확정 관계 수 조회: %w", err)
	}
	return count, nil
}

func (s *Store) agtypeCount(ctx context.Context, query string) (int, error) {
	var raw string
	if err := s.pool.QueryRow(ctx, s.cypherSQL(query, "count agtype")).Scan(&raw); err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.Trim(raw, `"`))
	if err != nil {
		return 0, fmt.Errorf("AGE 개수 해석 %q: %w", raw, err)
	}
	return value, nil
}

// SetGraphDeleted는 소유자가 웹에서 요청한 그래프 소프트 삭제 또는 복구를 적용한다.
func (s *Store) SetGraphDeleted(ctx context.Context, graphID, actorID model.ID, deleted bool) error {
	if !graphID.IsV7() || !actorID.IsV7() {
		return fmt.Errorf("그래프 상태 변경 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("그래프 상태 변경 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := lockGraphs(ctx, tx, []model.ID{graphID}); err != nil {
		return err
	}
	query, action := `UPDATE public.context_graph SET deleted_at = NULL, grace_started_at = NULL, grace_expires_at = NULL WHERE graph_id = $1 AND deleted_at IS NOT NULL`, "restore"
	if deleted {
		query, action = `UPDATE public.context_graph SET deleted_at = clock_timestamp() WHERE graph_id = $1 AND deleted_at IS NULL`, "delete"
	}
	result, err := tx.Exec(ctx, query, graphID.String())
	if err != nil {
		return fmt.Errorf("그래프 상태 변경: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrInvalidState
	}
	return s.commitWebAudit(ctx, tx, webAuditRecord{TargetKind: "web_delete", Action: action, ActorID: actorID, GraphID: graphID})
}

// SetContextDeleted는 웹에서 요청한 컨텍스트 상태 변경과 웹 감사 기록을 같은 트랜잭션에 남긴다.
func (s *Store) SetContextDeleted(ctx context.Context, graphID, contextID, actorID model.ID, deleted bool) (model.Context, error) {
	if !graphID.IsV7() || !contextID.IsV7() || !actorID.IsV7() {
		return model.Context{}, fmt.Errorf("컨텍스트 웹 상태 변경 인자가 올바르지 않다")
	}
	value, err := s.changeContextDeletion(ctx, graphID, contextID, nil, deleted)
	if err != nil {
		return model.Context{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 웹 감사 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	action := "restore"
	if deleted {
		action = "delete"
	}
	if err := s.insertWebAudit(ctx, tx, webAuditRecord{TargetKind: "web_delete", Action: action, ActorID: actorID, GraphID: graphID, TargetContextID: contextID}); err != nil {
		return model.Context{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 웹 감사 커밋: %w", err)
	}
	return value, nil
}

// ListRestoreEligibleGraphs는 요청 계정이 소유자였던 자동 삭제 그래프만 반환한다.
func (s *Store) ListRestoreEligibleGraphs(ctx context.Context, accountID model.ID) ([]model.Graph, error) {
	if !accountID.IsV7() {
		return nil, fmt.Errorf("계정 식별자가 UUIDv7이 아니다")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT graph_id, name, description, created_by, created_at, last_activity_at, grace_started_at, stored_chars, version, deleted_at
		FROM public.context_graph AS graph
		WHERE graph.deleted_at IS NOT NULL AND graph.grace_started_at IS NOT NULL
		  AND (graph.created_by = $1 OR EXISTS (
			SELECT 1 FROM public.web_audit_log WHERE graph_id = graph.graph_id AND subject_id = $1 AND after_grade = 'owner'
		  ))
		ORDER BY graph.deleted_at DESC, graph.graph_id DESC`, accountID.String())
	if err != nil {
		return nil, fmt.Errorf("복구 요청 가능 그래프 목록 조회: %w", err)
	}
	defer rows.Close()
	graphs := make([]model.Graph, 0)
	for rows.Next() {
		graph, err := scanGraph(rows)
		if err != nil {
			return nil, err
		}
		graphs = append(graphs, graph)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("복구 요청 가능 그래프 목록 행 읽기: %w", err)
	}
	return graphs, nil
}

// ListOwnedDeletedGraphs는 요청 계정이 소유자 등급으로 직접 삭제한 그래프를 읽는다.
//
// 자동 삭제와 나누는 이유는 복구 주체가 다르기 때문이다. 「운영자 복구」가 사용자 직접
// 삭제를 운영자 대상에서 빼고 소유자가 웹에서 복구하기로 확정했고, 두 경로는 「그래프
// 소프트 삭제」가 확정한 대로 grace_started_at의 유무로 갈린다.
func (s *Store) ListOwnedDeletedGraphs(ctx context.Context, accountID model.ID) ([]model.Graph, error) {
	if !accountID.IsV7() {
		return nil, fmt.Errorf("계정 식별자가 UUIDv7이 아니다")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT graph_id, name, description, created_by, created_at, last_activity_at, grace_started_at, stored_chars, version, deleted_at
		FROM public.context_graph AS graph
		WHERE graph.deleted_at IS NOT NULL AND graph.grace_started_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM public.graph_grant AS permission
			WHERE permission.graph_id = graph.graph_id AND permission.grade = 'owner'
			  AND ((permission.subject_type = 'account' AND permission.subject_id = $1)
				OR (permission.subject_type = 'team' AND EXISTS (
					SELECT 1 FROM public.team
					JOIN public.team_member AS member ON member.team_id = team.team_id
					WHERE team.team_id = permission.subject_id AND team.deleted_at IS NULL AND member.account_id = $1
				)))
		  )
		ORDER BY graph.deleted_at DESC, graph.graph_id DESC`, accountID.String())
	if err != nil {
		return nil, fmt.Errorf("직접 삭제한 그래프 목록 조회: %w", err)
	}
	defer rows.Close()
	graphs := make([]model.Graph, 0)
	for rows.Next() {
		graph, err := scanGraph(rows)
		if err != nil {
			return nil, err
		}
		graphs = append(graphs, graph)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("직접 삭제한 그래프 목록 행 읽기: %w", err)
	}
	return graphs, nil
}

// RequestGraphRestore는 소유자였던 계정의 자동 삭제 그래프 복구 요청을 기록한다.
func (s *Store) RequestGraphRestore(ctx context.Context, graphID, actorID model.ID) error {
	if !graphID.IsV7() || !actorID.IsV7() {
		return fmt.Errorf("그래프 복구 요청 인자가 올바르지 않다")
	}
	eligible, err := s.ListRestoreEligibleGraphs(ctx, actorID)
	if err != nil {
		return err
	}
	if !containsGraph(eligible, graphID) {
		return ErrNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("그래프 복구 요청 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	return s.commitWebAudit(ctx, tx, webAuditRecord{TargetKind: "restore_request", Action: "request", ActorID: actorID, GraphID: graphID, RequesterAccountID: actorID})
}

// PendingRestoreRequests는 아직 운영자 복구 기록이 뒤따르지 않은 요청만 표시한다.
func (s *Store) PendingRestoreRequests(ctx context.Context) ([]model.RestoreRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT request.graph_id, graph.name, request.requester_account_id, request.occurred_at
		FROM public.web_audit_log AS request
		JOIN public.context_graph AS graph ON graph.graph_id = request.graph_id
		WHERE request.target_kind = 'restore_request' AND request.action = 'request'
		  AND NOT EXISTS (
			SELECT 1 FROM public.web_audit_log AS restored
			WHERE restored.target_kind = 'operator_restore' AND restored.graph_id = request.graph_id AND restored.occurred_at > request.occurred_at
		  )
		  AND graph.deleted_at IS NOT NULL AND graph.grace_started_at IS NOT NULL
		ORDER BY request.occurred_at ASC, request.graph_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("대기 복구 요청 목록 조회: %w", err)
	}
	defer rows.Close()
	requests := make([]model.RestoreRequest, 0)
	for rows.Next() {
		var graphID, accountID string
		var request model.RestoreRequest
		if err := rows.Scan(&graphID, &request.GraphName, &accountID, &request.RequestedAt); err != nil {
			return nil, fmt.Errorf("대기 복구 요청 해석: %w", err)
		}
		var err error
		request.GraphID, err = model.ParseID(graphID)
		if err != nil {
			return nil, fmt.Errorf("복구 요청 그래프 식별자: %w", err)
		}
		request.RequestedBy, err = model.ParseID(accountID)
		if err != nil {
			return nil, fmt.Errorf("복구 요청 계정 식별자: %w", err)
		}
		request.RequestedAt = request.RequestedAt.UTC()
		requests = append(requests, request)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("대기 복구 요청 목록 행 읽기: %w", err)
	}
	return requests, nil
}

// OperatorRestoreGraph은 대기 요청이 있는 자동 삭제 그래프만 내용 조회 없이 복구한다.
func (s *Store) OperatorRestoreGraph(ctx context.Context, graphID, operatorID model.ID) error {
	if !graphID.IsV7() || !operatorID.IsV7() {
		return fmt.Errorf("운영자 그래프 복구 인자가 올바르지 않다")
	}
	requests, err := s.PendingRestoreRequests(ctx)
	if err != nil {
		return err
	}
	var request model.RestoreRequest
	found := false
	for _, candidate := range requests {
		if candidate.GraphID == graphID {
			request, found = candidate, true
			break
		}
	}
	if !found {
		return ErrNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("운영자 그래프 복구 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE public.context_graph SET deleted_at = NULL, grace_started_at = NULL, grace_expires_at = NULL WHERE graph_id = $1 AND deleted_at IS NOT NULL AND grace_started_at IS NOT NULL`, graphID.String())
	if err != nil {
		return fmt.Errorf("운영자 그래프 복구: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrInvalidState
	}
	return s.commitWebAudit(ctx, tx, webAuditRecord{TargetKind: "operator_restore", Action: "restore", ActorID: operatorID, GraphID: graphID, RequesterAccountID: request.RequestedBy})
}

func containsGraph(graphs []model.Graph, graphID model.ID) bool {
	for _, graph := range graphs {
		if graph.ID == graphID {
			return true
		}
	}
	return false
}

// 시각화 시작점은 「웹 화면 계약」이 그래프 상세에 확정한 전역 요약 파생이다. 검색의
// 전역 요약 조회와 달리 유효 기간으로 좁히지 않는다. 화면은 특정 시점의 후보를 고르는
// 것이 아니라 그래프에 있는 전역 요약을 그대로 보여준다.
const (
	activeContexts        = "node.deleted_at IS NULL"
	deletedContexts       = "node.deleted_at IS NOT NULL"
	activeGlobalSummaries = activeContexts + " AND node.layer = 'derived' AND node.derivation_kind = 'summary' AND node.summary_scope = 'global'"
)

// GraphVisualization은 그래프 상세 화면의 데이터를 「홉 범위 조회」 규칙으로 읽는다.
//
// 화면 전용 조회를 따로 만들지 않는 이유는 「웹 화면 계약」이 확정한 것이다. 그 규칙이
// 결과 상한과 절단 경계를 이미 갖고 있고, 방문한 정점만으로 연결선을 걸러 삭제된
// 컨텍스트를 가리키는 간선을 남기지 않는다. 전역 요약 파생이 없는 그래프에서는 활성
// 컨텍스트를 시작점으로 삼아 같은 상한과 절단 규칙 아래에서 전체 보기를 만든다.
func (s *Store) GraphVisualization(ctx context.Context, graphID model.ID, hops, limit int) (HopResult, error) {
	if !graphID.IsV7() || hops < 0 || limit < 0 {
		return HopResult{}, fmt.Errorf("시각화 조회 인자가 올바르지 않다")
	}
	starts, err := s.graphContexts(ctx, graphID, activeGlobalSummaries, limit)
	if err != nil {
		return HopResult{}, err
	}
	if len(starts) == 0 {
		if starts, err = s.graphContexts(ctx, graphID, activeContexts, limit); err != nil {
			return HopResult{}, err
		}
	}
	if len(starts) == 0 {
		return HopResult{}, nil
	}
	// 탐색 필터를 비워 두면 확정된 참조와 사건 관계를 모두 따른다. 「시각 표현 규칙」이
	// 연결선 종류로 구분하라고 했으므로 종류를 줄이지 않는다.
	return s.HopContextsFrom(ctx, graphID, starts, hops, "both", nil, limit)
}

// ListDeletedContexts는 복구 화면이 쓸 소프트 삭제된 컨텍스트만 읽는다. 「화면 접근 제어」가
// 삭제된 컨텍스트를 복구 화면 밖에서 보이지 않게 했으므로 다른 화면은 이 조회를 쓰지 않는다.
func (s *Store) ListDeletedContexts(ctx context.Context, graphID model.ID, limit int) ([]model.Context, error) {
	return s.graphContexts(ctx, graphID, deletedContexts, limit)
}

// ListActiveContexts는 삭제 대상을 고르는 화면이 쓸 활성 컨텍스트를 읽는다.
func (s *Store) ListActiveContexts(ctx context.Context, graphID model.ID, limit int) ([]model.Context, error) {
	return s.graphContexts(ctx, graphID, activeContexts, limit)
}

// graphContexts는 한 그래프의 정점을 주어진 조건과 상한으로 읽는다. limit 0은 「계정 플랜」이
// 선언한 대로 한도 없음이다.
func (s *Store) graphContexts(ctx context.Context, graphID model.ID, predicate string, limit int) ([]model.Context, error) {
	if !graphID.IsV7() || limit < 0 {
		return nil, fmt.Errorf("그래프 정점 조회 인자가 올바르지 않다")
	}
	query := "MATCH (node:Context) WHERE node.graph_id = " + cypherString(graphID.String()) + " AND " + predicate + " RETURN node ORDER BY node.context_id"
	if limit > 0 {
		query += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := s.pool.Query(ctx, s.cypherSQL(query, "node agtype"))
	if err != nil {
		return nil, fmt.Errorf("그래프 정점 목록 조회: %w", err)
	}
	defer rows.Close()
	contexts := make([]model.Context, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("그래프 정점 행 해석: %w", err)
		}
		value, err := parseContext(raw, graphID)
		if err != nil {
			return nil, err
		}
		contexts = append(contexts, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("그래프 정점 행 읽기: %w", err)
	}
	return contexts, nil
}

// ListAuditEntries는 컨텍스트 관리 기록과 웹 관리 기록을 최신순으로 합쳐 읽는다.
func (s *Store) ListAuditEntries(ctx context.Context, graphID model.ID, limit int) ([]model.AuditEntry, error) {
	if !graphID.IsV7() || limit <= 0 {
		return nil, fmt.Errorf("감사 기록 조회 인자가 올바르지 않다")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT kind, action, actor_id, occurred_at, target, detail
		FROM (
			SELECT 'operation' AS kind, operation_kind AS action, actor_account_id::text AS actor_id, applied_at AS occurred_at,
			       COALESCE(context_id::text, relation_id::text, '') AS target, result || COALESCE(': ' || reject_reason, '') AS detail
			FROM public.operation_log WHERE graph_id = $1
			UNION ALL
			SELECT target_kind, action, actor_account_id::text, occurred_at,
			       COALESCE(target_context_id::text, subject_id::text, team_id::text, graph_id::text, '') AS target,
			       COALESCE(before_grade || ' → ' || after_grade, '') AS detail
			FROM public.web_audit_log WHERE graph_id = $1
		) AS entries
		ORDER BY occurred_at DESC, actor_id DESC
		LIMIT $2`, graphID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("감사 기록 목록 조회: %w", err)
	}
	defer rows.Close()
	entries := make([]model.AuditEntry, 0, limit)
	for rows.Next() {
		var rawActor string
		var entry model.AuditEntry
		if err := rows.Scan(&entry.Kind, &entry.Action, &rawActor, &entry.OccurredAt, &entry.Target, &entry.Detail); err != nil {
			return nil, fmt.Errorf("감사 기록 행 해석: %w", err)
		}
		var err error
		entry.ActorID, err = model.ParseID(rawActor)
		if err != nil {
			return nil, fmt.Errorf("감사 기록 수행 계정 식별자: %w", err)
		}
		entry.OccurredAt = entry.OccurredAt.UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("감사 기록 행 읽기: %w", err)
	}
	return entries, nil
}
