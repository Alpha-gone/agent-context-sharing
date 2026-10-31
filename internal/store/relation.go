package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// ConfirmRelation은 관계 정체성으로 후보를 확정하거나, 없는 관계를 에이전트 제안으로 만든다.
// 같은 정체성의 재확정은 현재 관계를 돌려주는 멱등 연산이다.
func (s *Store) ConfirmRelation(ctx context.Context, graphID model.ID, relation model.Relation, operation *OperationRecord) (result model.Relation, err error) {
	ctx, observation := beginManagement(ctx, "relation_confirm")
	defer func() {
		if result.ID.IsV7() {
			observation.fields.Identity.RelationID = result.ID.String()
		}
		s.finishManagement(ctx, observation, err)
	}()

	if !graphID.IsV7() || relation.GraphID != graphID {
		return model.Relation{}, fmt.Errorf("요청 graph_id와 관계 graph_id가 일치하지 않는다")
	}
	relation = normalizeRelation(relation)
	if relation.State != model.RelationStateConfirmed {
		return model.Relation{}, model.FieldError{Field: "state", Message: "관계 확정 입력은 confirmed 상태여야 한다"}
	}
	if err := relation.Validate(); err != nil {
		return model.Relation{}, fmt.Errorf("관계 확정 입력: %w", errors.Join(ErrInvalidRelation, err))
	}
	if err := validateRelationOperation(operation, graphID, relation.ID, OperationAdd); err != nil {
		return model.Relation{}, err
	}
	observation.stage = "actor_binding"
	if operation != nil && (operation.AccountID != relation.ConfirmedBy || operation.AgentID != relation.ConfirmedByAgent) {
		return model.Relation{}, model.FieldError{Field: "confirmed_by", Message: "관계 확정 주체와 관리 기록의 실행 주체가 일치해야 한다"}
	}
	observation.stage = "write"
	tx, err := s.writeTransaction(ctx)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 확정 트랜잭션 시작: %w", err)
	}
	defer finishWrite(ctx, tx, "ConfirmRelation", &err)

	// 정체성 조회와 생성 사이를 그래프 행 잠금으로 직렬화한다. AGE 간선에는 유일
	// 인덱스가 없어 같은 (유형, from, to)의 동시 확정이 간선을 여러 개 만들고, 순환
	// 검사도 서로의 미커밋 간선을 보지 못해 A→B와 B→A가 함께 통과한다. 「판정의
	// 직렬화」가 같은 행을 이미 쓰기 경로의 직렬화 지점으로 두었으므로 새 경합 지점은
	// 아니며, 이 트랜잭션도 외부 호출 없이 끝난다.
	observation.stage = "permission"
	if err := requireActiveGraph(ctx, tx, graphID); err != nil {
		return model.Relation{}, err
	}

	observation.fields.Identity.GraphID = graphID.String()
	observation.stage = "endpoint_load"
	from, err := s.context(ctx, tx, graphID, relation.FromContextID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 시작 사건 조회: %w", err)
	}
	to, err := s.context(ctx, tx, graphID, relation.ToContextID)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 대상 사건 조회: %w", err)
	}
	observation.fields.Identity.ContextID = from.ID.String()
	observation.fields.Management.RelationType = string(relation.Type)
	observation.stage = "layer_check"
	observation.fields.Management.ExpectedLayer = "event"
	if from.Layer != model.LayerEvent {
		observation.fields.Management.ActualLayer = string(from.Layer)
		return model.Relation{}, errors.Join(ErrInvalidRelation, model.FieldError{Field: "from_context_id", Message: "관계 양 끝은 사건이어야 한다", Cause: model.ErrRelationEndpointNotEvent})
	}
	if to.Layer != model.LayerEvent {
		observation.fields.Management.ActualLayer = string(to.Layer)
		return model.Relation{}, errors.Join(ErrInvalidRelation, model.FieldError{Field: "to_context_id", Message: "관계 양 끝은 사건이어야 한다", Cause: model.ErrRelationEndpointNotEvent})
	}
	observation.fields.Management.ActualLayer = "event"
	observation.stage = "time_check"
	if err := model.ValidateRelation(relation, from, to); err != nil {
		return model.Relation{}, fmt.Errorf("관계 검증: %w", errors.Join(ErrInvalidRelation, err))
	}

	observation.stage = "endpoint_load"
	existing, err := s.relationByIdentity(ctx, tx, graphID, relation.Type, relation.FromContextID, relation.ToContextID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return model.Relation{}, err
	}
	if errors.Is(err, ErrNotFound) {
		observation.stage = "cycle_check"
		if err := s.validateRelationCycle(ctx, tx, graphID, relation); err != nil {
			return model.Relation{}, err
		}
		// 기존 후보의 출처는 보존하되, 후보 없이 직접 확정한 관계는 에이전트 제안이다.
		relation.ProposedBy = model.ProposalSourceAgent
		observation.stage = "write"
		stored, err := s.createRelation(ctx, tx, graphID, relation)
		if err != nil {
			return model.Relation{}, err
		}
		if err := bumpContentRevision(ctx, tx, graphID); err != nil {
			return model.Relation{}, err
		}
		observation.stage = "audit"
		if err := s.recordAppliedRelationOperation(ctx, tx, operation, stored.ID); err != nil {
			return model.Relation{}, err
		}
		if err := s.checkWriteInvariants(ctx, tx, graphID, []model.ID{relation.FromContextID, relation.ToContextID}, EmbeddingExpectation{}); err != nil {
			return model.Relation{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return model.Relation{}, fmt.Errorf("관계 확정 커밋: %w", err)
		}
		return stored, nil
	}

	if existing.State != model.RelationStateConfirmed {
		// 폐기하거나 제안 상태인 관계를 확정으로 되돌리는 것도 새 확정 간선을 만드는
		// 것과 같다. A→B를 폐기하고 B→A를 확정한 뒤 A→B를 재확정하면 순환이 확정되므로
		// 새 관계와 같은 검사를 지나야 한다.
		observation.stage = "cycle_check"
		if err := s.validateRelationCycle(ctx, tx, graphID, relation); err != nil {
			return model.Relation{}, err
		}
		existing.State = model.RelationStateConfirmed
		existing.DeletedAt = nil
		existing.ConfirmedBy = relation.ConfirmedBy
		existing.ConfirmedByAgent = relation.ConfirmedByAgent
		existing.ConfirmedAt = relation.ConfirmedAt
		observation.stage = "write"
		stored, err := s.updateRelation(ctx, tx, graphID, existing)
		if err != nil {
			return model.Relation{}, err
		}
		if err := bumpContentRevision(ctx, tx, graphID); err != nil {
			return model.Relation{}, err
		}
		existing = stored
	}
	observation.stage = "audit"
	if err := s.recordAppliedRelationOperation(ctx, tx, operation, existing.ID); err != nil {
		return model.Relation{}, err
	}
	if err := s.checkWriteInvariants(ctx, tx, graphID, []model.ID{relation.FromContextID, relation.ToContextID}, EmbeddingExpectation{}); err != nil {
		return model.Relation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Relation{}, fmt.Errorf("관계 확정 커밋: %w", err)
	}
	return existing, nil
}

// DiscardRelation은 활성 관계를 폐기 상태로 바꾸고 관리 연산 기록을 함께 남긴다.
func (s *Store) DiscardRelation(ctx context.Context, graphID, relationID model.ID, operation *OperationRecord) (result model.Relation, err error) {
	ctx, observation := beginManagement(ctx, "relation_discard")
	defer func() {
		if result.ID.IsV7() {
			observation.fields.Identity.GraphID = result.GraphID.String()
			observation.fields.Identity.RelationID = result.ID.String()
		}
		s.finishManagement(ctx, observation, err)
	}()

	if !graphID.IsV7() || !relationID.IsV7() {
		return model.Relation{}, fmt.Errorf("그래프와 관계 식별자는 UUIDv7이어야 한다")
	}
	if err := validateRelationOperation(operation, graphID, relationID, OperationDiscard); err != nil {
		return model.Relation{}, err
	}
	observation.stage = "write"
	tx, err := s.writeTransaction(ctx)
	if err != nil {
		return model.Relation{}, fmt.Errorf("관계 폐기 트랜잭션 시작: %w", err)
	}
	defer finishWrite(ctx, tx, "DiscardRelation", &err)
	// 확정과 같은 그래프 행을 잠근다. 관계에는 판 번호가 없어 낙관적 잠금으로 거를 수
	// 없으므로, 같은 관계에 폐기가 동시에 오면 읽은 상태가 둘 다 통과해 AGE가 동시 갱신
	// 오류를 낸다. 잠금이 조회와 갱신 사이를 직렬화해 뒤에 온 요청이 이미 폐기된 상태를
	// 보고 정상적으로 거부된다.
	if err := requireActiveGraph(ctx, tx, graphID); err != nil {
		return model.Relation{}, err
	}
	stored, err := s.relationByID(ctx, tx, graphID, relationID)
	if err != nil {
		return model.Relation{}, err
	}
	if stored.State == model.RelationStateDiscarded {
		return model.Relation{}, fmt.Errorf("관계가 이미 폐기됐다: %w", errors.Join(ErrInvalidState, model.FieldError{Field: "relation_id", Message: "관계가 이미 폐기됐다"}))
	}
	now := time.Now().UTC()
	stored.State = model.RelationStateDiscarded
	stored.DeletedAt = &now
	stored.ConfirmedBy = model.ID{}
	stored.ConfirmedByAgent = model.ID{}
	stored.ConfirmedAt = nil
	stored, err = s.updateRelation(ctx, tx, graphID, stored)
	if err != nil {
		return model.Relation{}, err
	}
	if err := bumpContentRevision(ctx, tx, graphID); err != nil {
		return model.Relation{}, err
	}
	if err := s.recordAppliedRelationOperation(ctx, tx, operation, stored.ID); err != nil {
		return model.Relation{}, err
	}
	if err := s.checkWriteInvariants(ctx, tx, graphID, []model.ID{stored.FromContextID, stored.ToContextID}, EmbeddingExpectation{}); err != nil {
		return model.Relation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Relation{}, fmt.Errorf("관계 폐기 커밋: %w", err)
	}
	return stored, nil
}

// ListRelations는 graph_id에 속한 관계를 relation_id 내림차순으로 한 페이지 읽는다.
func (s *Store) ListRelations(ctx context.Context, graphID model.ID, cursor string, limit int) ([]model.Relation, string, error) {
	return s.ListContextRelations(ctx, graphID, model.ID{}, nil, nil, cursor, limit)
}

// ListContextRelations는 지정 사건에 붙은 관계를 상태·유형 필터와 커서로 읽는다.
func (s *Store) ListContextRelations(ctx context.Context, graphID, contextID model.ID, states []model.RelationState, types []model.RelationType, cursor string, limit int) (result []model.Relation, next string, err error) {
	if !graphID.IsV7() || limit <= 0 {
		return nil, "", fmt.Errorf("관계 목록 인자가 올바르지 않다")
	}
	ctx, release, err := s.enterReadScope(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if closeErr := release(); closeErr != nil {
			result, next, err = nil, "", errors.Join(err, closeErr)
		}
		err = writeFailure(ctx, "relation_list", err)
	}()
	return s.listRelations(ctx, s.reader(ctx), graphID, contextID, states, types, cursor, limit)
}

// ProposeEventRelations는 저장된 사건과 같은 그래프의 사건을 비교해 시간 인접과
// 구성원 진부분집합 후보를 만든다. 임베딩 유사도 후보는 색인 성공이 방아쇠이므로
// index 작업자가 이 메서드와 분리해 처리한다.
func (s *Store) ProposeEventRelations(ctx context.Context, graphID, eventID model.ID) error {
	if s.relationProposals == nil {
		return nil
	}
	if !graphID.IsV7() || !eventID.IsV7() {
		return fmt.Errorf("그래프와 사건 식별자는 UUIDv7이어야 한다")
	}
	// 시스템 제안은 사용자 조회가 아니므로 접근 시각 갱신·계층 승격을 하지 않는 내부 조회를 쓴다.
	target, err := s.context(ctx, s.pool, graphID, eventID)
	if err != nil {
		return err
	}
	if target.Layer != model.LayerEvent || target.DeletedAt != nil {
		return nil
	}
	events, err := s.activeEventContexts(ctx, graphID, eventID)
	if err != nil {
		return err
	}

	precedes := make([]rankedRelationCandidate, 0)
	partOf := make([]rankedRelationCandidate, 0)
	for _, other := range events {
		if other.ID == target.ID {
			continue
		}
		if relation, gap, ok := adjacentPrecedesCandidate(graphID, target, other, s.relationProposals.AdjacencyWindow); ok {
			precedes = append(precedes, rankedRelationCandidate{relation: relation, rank: int64(gap)})
		}
		if relation, memberCount, ok := memberSubsetCandidate(graphID, target, other); ok {
			partOf = append(partOf, rankedRelationCandidate{relation: relation, rank: int64(memberCount)})
		}
	}
	slices.SortFunc(precedes, func(left, right rankedRelationCandidate) int {
		if order := cmp.Compare(left.rank, right.rank); order != 0 {
			return order
		}
		return compareRelationIdentity(left.relation, right.relation)
	})
	slices.SortFunc(partOf, func(left, right rankedRelationCandidate) int {
		if order := cmp.Compare(right.rank, left.rank); order != 0 {
			return order
		}
		return compareRelationIdentity(left.relation, right.relation)
	})
	if err := s.proposeRankedRelations(ctx, graphID, precedes); err != nil {
		return fmt.Errorf("시간 인접 관계 후보 제안: %w", err)
	}
	if err := s.proposeRankedRelations(ctx, graphID, partOf); err != nil {
		return fmt.Errorf("구성원 부분집합 관계 후보 제안: %w", err)
	}
	return nil
}

type rankedRelationCandidate struct {
	relation model.Relation
	rank     int64
}

func (s *Store) proposeEventRelationsAfterSave(ctx context.Context, graphID model.ID, event model.Context) {
	if event.Layer != model.LayerEvent || event.DeletedAt != nil {
		return
	}
	if deferAfterWriteCommit(ctx, func() {
		s.proposeEventRelations(context.WithoutCancel(ctx), graphID, event)
	}) {
		return
	}
	s.proposeEventRelations(ctx, graphID, event)
}

func (s *Store) proposeEventRelations(ctx context.Context, graphID model.ID, event model.Context) {
	if err := s.ProposeEventRelations(ctx, graphID, event.ID); err != nil {
		slog.ErrorContext(ctx, "사건 관계 후보 제안 실패", "graph_id", graphID.String(), "context_id", event.ID.String(),
			"stage", "relation_proposal", "result", "failed", "reason_code", "proposal_failed")
	}
}

// validateEventRelationTimes은 사건 갱신이 이미 확정된 관계를 깨뜨리지 않는지 확인한다.
func (s *Store) validateEventRelationTimes(ctx context.Context, tx pgx.Tx, graphID model.ID, event model.Context) error {
	relations, err := s.allRelations(ctx, tx, graphID, event.ID, []model.RelationState{model.RelationStateConfirmed}, nil)
	if err != nil {
		return err
	}
	otherIDs := make([]model.ID, 0, len(relations))
	for _, relation := range relations {
		otherID := relation.FromContextID
		if otherID == event.ID {
			otherID = relation.ToContextID
		}
		otherIDs = append(otherIDs, otherID)
	}
	slices.SortFunc(otherIDs, func(a, b model.ID) int { return cmp.Compare(a.String(), b.String()) })
	others, err := s.contextVerticesByIDs(ctx, tx, graphID, slices.Compact(otherIDs))
	if err != nil {
		return err
	}
	for _, relation := range relations {
		otherID := relation.FromContextID
		if otherID == event.ID {
			otherID = relation.ToContextID
		}
		other, found := others[otherID]
		if !found || other.Event == nil {
			return ErrInvariantViolation
		}
		from, to := *other.Event, *event.Event
		if relation.FromContextID == event.ID {
			from, to = to, from
		}
		if err := model.ValidateRelationTime(relation.Type, from, to); err != nil {
			return fmt.Errorf("확정 관계 시간 제약: %w", errors.Join(ErrInvalidRelation, err))
		}
	}
	return nil
}

// eventProposalInputLimit은 생성 수 상한과 별개인 사건 비교 입력의 적재 상한이다.
const eventProposalInputLimit = 1000

func (s *Store) activeEventContexts(ctx context.Context, graphID, excludedID model.ID) ([]model.Context, error) {
	rows, err := s.reader(ctx).Query(ctx, `SELECT properties ->> 'context_id'::text
		FROM `+s.contextTable()+` WHERE properties ->> 'graph_id'::text = $1
		AND properties ->> 'layer'::text = 'event' AND properties ->> 'deleted_at'::text IS NULL
		AND (properties ->> 'context_id'::text) COLLATE "C" <> $2 COLLATE "C"
		ORDER BY (properties ->> 'context_id'::text) COLLATE "C" DESC LIMIT $3`, graphID.String(), excludedID.String(), eventProposalInputLimit)
	if err != nil {
		return nil, fmt.Errorf("사건 제안 입력 조회: %w", err)
	}
	ids, err := searchCandidateIDs(rows)
	if err != nil {
		return nil, err
	}
	return s.contextsByIDs(ctx, s.reader(ctx), graphID, ids)
}

// adjacentPrecedesCandidate는 시간 범위가 겹치지 않고 간격이 임계값 안인 두 사건에서
// 앞선 쪽을 시작으로 하는 precedes 후보를 만든다.
//
// 앞선 사건에만 event_end가 필요하다. 뒤따르는 사건은 진행 중이어도 되며, 「사건 모델」이
// event_end 없는 사건을 진행 중으로 정의하고 제안에서 빼지 않았다. 양쪽 모두에 종료를
// 요구하면 가장 최근 사건이 직전 사건의 후속 후보로 올라오지 않는다.
func adjacentPrecedesCandidate(graphID model.ID, left, right model.Context, window time.Duration) (model.Relation, time.Duration, bool) {
	if left.Event == nil || right.Event == nil {
		return model.Relation{}, 0, false
	}
	var from, to model.Context
	switch {
	case left.Event.End != nil && !left.Event.End.After(right.Event.Start):
		from, to = left, right
	case right.Event.End != nil && !right.Event.End.After(left.Event.Start):
		from, to = right, left
	default:
		return model.Relation{}, 0, false
	}
	gap := to.Event.Start.Sub(*from.Event.End)
	if gap > window {
		return model.Relation{}, 0, false
	}
	return proposedRelation(graphID, model.RelationTypePrecedes, from.ID, to.ID), gap, true
}

func memberSubsetCandidate(graphID model.ID, left, right model.Context) (model.Relation, int, bool) {
	if left.Event == nil || right.Event == nil {
		return model.Relation{}, 0, false
	}
	part, whole := left, right
	if strictMemberSubset(left.Event.MemberIDs, right.Event.MemberIDs) {
		part, whole = left, right
	} else if strictMemberSubset(right.Event.MemberIDs, left.Event.MemberIDs) {
		part, whole = right, left
	} else {
		return model.Relation{}, 0, false
	}
	if !eventContains(*whole.Event, *part.Event) {
		return model.Relation{}, 0, false
	}
	return proposedRelation(graphID, model.RelationTypePartOf, part.ID, whole.ID), len(part.Event.MemberIDs), true
}

func proposedRelation(graphID model.ID, relationType model.RelationType, fromID, toID model.ID) model.Relation {
	return model.Relation{
		GraphID:       graphID,
		Type:          relationType,
		FromContextID: fromID,
		ToContextID:   toID,
		State:         model.RelationStateProposed,
		ProposedBy:    model.ProposalSourceSystem,
		ProposedAt:    time.Now().UTC(),
	}
}

func strictMemberSubset(part, whole []model.ID) bool {
	if len(part) >= len(whole) {
		return false
	}
	wholeMembers := make(map[model.ID]struct{}, len(whole))
	for _, memberID := range whole {
		wholeMembers[memberID] = struct{}{}
	}
	for _, memberID := range part {
		if _, found := wholeMembers[memberID]; !found {
			return false
		}
	}
	return true
}

func eventContains(whole, part model.EventAttributes) bool {
	if whole.Start.After(part.Start) {
		return false
	}
	return whole.End == nil || (part.End != nil && !part.End.After(*whole.End))
}

func compareRelationIdentity(left, right model.Relation) int {
	return cmp.Or(cmp.Compare(left.FromContextID.String(), right.FromContextID.String()),
		cmp.Compare(left.ToContextID.String(), right.ToContextID.String()))
}

func (s *Store) proposeRankedRelations(ctx context.Context, graphID model.ID, candidates []rankedRelationCandidate) (err error) {
	if len(candidates) == 0 {
		return nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("후보 신호 트랜잭션 시작: %w", err)
	}
	defer finishWrite(ctx, tx, "relation_proposal", &err)
	if err := requireActiveGraph(ctx, tx, graphID); err != nil {
		return err
	}
	created := 0
	for _, candidate := range candidates {
		if created >= s.relationProposals.Limit {
			break
		}
		wasCreated, err := s.createProposedRelationInTx(ctx, tx, graphID, candidate.relation, "")
		if err != nil {
			return err
		}
		if wasCreated {
			created++
		}
	}
	return tx.Commit(ctx)
}

// validateRelationCycle은 확정으로 가는 모든 경로가 쓰는 순환 검사다. 새 관계의 확정과
// 기존 관계의 재확정이 같은 규칙을 지나야 한 쪽으로만 순환이 들어오지 않는다.
func (s *Store) validateRelationCycle(ctx context.Context, tx pgx.Tx, graphID model.ID, relation model.Relation) error {
	pathExists, err := s.hasConfirmedRelationPath(ctx, tx, graphID, relation.Type, relation.ToContextID, relation.FromContextID)
	if err != nil {
		return fmt.Errorf("관계 순환 경로 조회: %w", err)
	}
	if err := model.ValidateRelationCycle(relation, pathExists); err != nil {
		return fmt.Errorf("관계 순환 검증: %w", errors.Join(ErrInvalidRelation, err))
	}
	return nil
}

func (s *Store) createProposedRelation(ctx context.Context, graphID model.ID, relation model.Relation, modelID string) (created bool, err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("후보 관계 트랜잭션 시작: %w", err)
	}
	defer finishWrite(ctx, tx, "relation_proposal", &err)
	if err := requireActiveGraph(ctx, tx, graphID); err != nil {
		return false, err
	}
	created, err = s.createProposedRelationInTx(ctx, tx, graphID, relation, modelID)
	if err != nil || !created {
		return created, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("후보 관계 커밋: %w", err)
	}
	return true, nil
}

// 신호 묶음의 그래프 잠금 아래에서도 각 후보의 정체성과 현재 신호를 다시 확인한다.
func (s *Store) createProposedRelationInTx(ctx context.Context, tx pgx.Tx, graphID model.ID, relation model.Relation, modelID string) (bool, error) {
	relationID, err := model.NewID()
	if err != nil {
		return false, fmt.Errorf("관계 식별자 생성: %w", err)
	}
	relation.ID = relationID
	if _, err := s.relationByIdentity(ctx, tx, graphID, relation.Type, relation.FromContextID, relation.ToContextID); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	from, err := s.context(ctx, tx, graphID, relation.FromContextID)
	if err != nil {
		return false, err
	}
	to, err := s.context(ctx, tx, graphID, relation.ToContextID)
	if err != nil {
		return false, err
	}
	// 후보 계산과 그래프 잠금 사이에 사건·벡터가 바뀔 수 있다. 현재 신호가 사라진
	// 후보는 건너뛰고 다음 후보를 시도하며, 과거 구성원이나 시간으로 간선을 만들지 않는다.
	valid, err := s.validProposalSignal(ctx, tx, relation, from, to, modelID)
	if err != nil || !valid {
		return false, err
	}
	if err := model.ValidateRelation(relation, from, to); err != nil {
		return false, fmt.Errorf("후보 관계 검증: %w", errors.Join(ErrInvalidRelation, err))
	}
	if _, err := s.createRelation(ctx, tx, graphID, relation); err != nil {
		return false, err
	}
	if err := s.checkWriteInvariants(ctx, tx, graphID, []model.ID{relation.FromContextID, relation.ToContextID}, EmbeddingExpectation{}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) validProposalSignal(ctx context.Context, tx pgx.Tx, relation model.Relation, from, to model.Context, modelID string) (bool, error) {
	if from.Layer != model.LayerEvent || to.Layer != model.LayerEvent || from.DeletedAt != nil || to.DeletedAt != nil {
		return false, nil
	}
	var current model.Relation
	var valid bool
	switch relation.Type {
	case model.RelationTypePrecedes:
		current, _, valid = adjacentPrecedesCandidate(relation.GraphID, from, to, s.relationProposals.AdjacencyWindow)
	case model.RelationTypePartOf:
		current, _, valid = memberSubsetCandidate(relation.GraphID, from, to)
	case model.RelationTypeRelatesTo:
		err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM public.context_embedding AS source
			JOIN public.context_embedding AS target ON target.graph_id=source.graph_id AND target.model_id=source.model_id
			WHERE source.graph_id=$1 AND source.context_id=$2 AND target.context_id=$3
				AND source.model_id=$4 AND source.embedding <=> target.embedding <= $5
		)`, relation.GraphID.String(), from.ID.String(), to.ID.String(), modelID,
			1-s.relationProposals.SimilarityThreshold).Scan(&valid)
		return valid, err
	default:
		return false, nil
	}
	return valid && current.FromContextID == relation.FromContextID && current.ToContextID == relation.ToContextID, nil
}

func (s *Store) listRelations(ctx context.Context, queryer cypherQueryer, graphID, contextID model.ID, states []model.RelationState, types []model.RelationType, cursor string, limit int) ([]model.Relation, string, error) {
	if !graphID.IsV7() || limit <= 0 {
		return nil, "", fmt.Errorf("관계 목록 인자가 올바르지 않다")
	}
	if contextID != (model.ID{}) && !contextID.IsV7() {
		return nil, "", fmt.Errorf("컨텍스트 식별자는 UUIDv7이어야 한다")
	}
	for _, state := range states {
		if state != model.RelationStateProposed && state != model.RelationStateConfirmed && state != model.RelationStateDiscarded {
			return nil, "", fmt.Errorf("관계 상태 필터가 올바르지 않다")
		}
	}
	for _, kind := range types {
		if _, err := relationLabel(kind); err != nil {
			return nil, "", err
		}
	}
	if len(types) == 0 {
		types = []model.RelationType{model.RelationTypePrecedes, model.RelationTypeCauses, model.RelationTypePartOf, model.RelationTypeRelatesTo}
	}
	var all []model.Relation
	seen := make(map[model.RelationType]bool)
	for _, kind := range types {
		if seen[kind] {
			continue
		}
		seen[kind] = true
		page, err := s.relationLabelPage(ctx, queryer, graphID, contextID, states, kind, cursor, limit)
		if err != nil {
			return nil, "", err
		}
		all = append(all, page...)
	}
	slices.SortFunc(all, func(a, b model.Relation) int { return cmp.Compare(b.ID.String(), a.ID.String()) })
	if len(all) <= limit {
		return all, "", nil
	}
	all = all[:limit]
	return all, encodeRelationCursor(all[len(all)-1].ID), nil
}

func (s *Store) relationLabelPage(ctx context.Context, queryer cypherQueryer, graphID, contextID model.ID, states []model.RelationState, kind model.RelationType, cursor string, limit int) ([]model.Relation, error) {
	label, err := relationLabel(kind)
	if err != nil {
		return nil, err
	}
	query := "MATCH ()-[edge:" + label + "]->() WHERE edge.graph_id = " + cypherString(graphID.String())
	if contextID.IsV7() {
		// 실제 끝점으로 고른 뒤 속성의 정체성도 재검사한다. 손상된 속성으로 대상을
		// 먼저 거르면 연결된 관계가 목록·사건 폐기 검사에서 조용히 사라질 수 있다.
		query = "MATCH (from:Context)-[edge:" + label + "]->(to:Context) WHERE edge.graph_id = " + cypherString(graphID.String()) +
			" AND (from.context_id = " + cypherString(contextID.String()) + " OR to.context_id = " + cypherString(contextID.String()) + ")"
	}
	if len(states) > 0 {
		query += " AND edge.state IN [" + relationStateValues(states) + "]"
	}
	if cursor != "" {
		relationID, err := decodeRelationCursor(cursor)
		if err != nil {
			return nil, err
		}
		query += " AND edge.relation_id < " + cypherString(relationID.String())
	}
	fetch := limit
	if limit < int(^uint(0)>>1) {
		fetch++
	}
	query += " RETURN edge, startNode(edge), endNode(edge) ORDER BY edge.relation_id DESC LIMIT " + fmt.Sprint(fetch)
	rows, err := queryer.Query(ctx, s.cypherSQL(query, "edge agtype, source agtype, target agtype"), pgx.QueryExecModeExec)
	if err != nil {
		return nil, fmt.Errorf("관계 목록 조회: %w", err)
	}
	defer rows.Close()

	relations := make([]model.Relation, 0)
	for rows.Next() {
		var raw, rawSource, rawTarget string
		if err := rows.Scan(&raw, &rawSource, &rawTarget); err != nil {
			return nil, fmt.Errorf("관계 목록 행 해석: %w", err)
		}
		relation, err := parseStoredRelation(raw, rawSource, rawTarget, graphID)
		if err != nil {
			return nil, err
		}
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("관계 목록 행 읽기: %w", err)
	}
	return relations, nil
}

func (s *Store) createRelation(ctx context.Context, tx pgx.Tx, graphID model.ID, relation model.Relation) (model.Relation, error) {
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
	return stored, nil
}

func (s *Store) updateRelation(ctx context.Context, tx pgx.Tx, graphID model.ID, relation model.Relation) (model.Relation, error) {
	properties, err := encodeProperties(relationProperties(relation))
	if err != nil {
		return model.Relation{}, err
	}
	label, err := relationLabel(relation.Type)
	if err != nil {
		return model.Relation{}, err
	}
	query := "MATCH ()-[edge:" + label + "]->() WHERE edge.relation_id = " + cypherString(relation.ID.String()) +
		" AND edge.graph_id = " + cypherString(graphID.String()) + " SET edge = " + properties + " RETURN edge, startNode(edge), endNode(edge)"
	stored, err := s.relationFromCypher(ctx, tx, graphID, query)
	if err != nil {
		return model.Relation{}, fmt.Errorf("사건 관계 갱신: %w", err)
	}
	return stored, nil
}

func (s *Store) relationByID(ctx context.Context, queryer cypherQueryer, graphID, relationID model.ID) (model.Relation, error) {
	for _, label := range []string{"PRECEDES", "CAUSES", "PART_OF", "RELATES_TO"} {
		query := "MATCH ()-[edge:" + label + "]->() WHERE edge.relation_id = " + cypherString(relationID.String()) +
			" AND edge.graph_id = " + cypherString(graphID.String()) + " RETURN edge, startNode(edge), endNode(edge)"
		stored, err := s.relationFromCypher(ctx, queryer, graphID, query)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		return stored, err
	}
	return model.Relation{}, ErrNotFound
}

func (s *Store) relationByIdentity(ctx context.Context, queryer cypherQueryer, graphID model.ID, relationType model.RelationType, fromID, toID model.ID) (model.Relation, error) {
	label, err := relationLabel(relationType)
	if err != nil {
		return model.Relation{}, err
	}
	query := "MATCH ()-[edge:" + label + "]->() WHERE edge.graph_id = " + cypherString(graphID.String()) +
		" AND edge.relation_type = " + cypherString(string(relationType)) +
		" AND edge.from_context_id = " + cypherString(fromID.String()) +
		" AND edge.to_context_id = " + cypherString(toID.String()) + " RETURN edge, startNode(edge), endNode(edge)"
	stored, err := s.relationFromCypher(ctx, queryer, graphID, query)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Relation{}, ErrNotFound
	}
	return stored, err
}

// relationScanPageSize는 전체 순회가 필요한 내부 조회의 한 페이지 크기다.
const relationScanPageSize = 500

// allRelations는 조건에 맞는 관계를 커서 끝까지 읽어 모두 돌려준다.
//
// 한 페이지만 읽으면 페이지를 넘긴 관계가 조용히 빠진다. 순환 검사는 못 본 간선 때문에
// 순환을 허용하고, 폐기 사건의 관계 정리는 확정 관계를 남겨 삭제된 사건이 탐색 대상으로
// 남는다. 둘 다 오류가 아니라 잘못된 성공으로 나타나므로 끝까지 읽는다.
func (s *Store) allRelations(ctx context.Context, queryer cypherQueryer, graphID, contextID model.ID, states []model.RelationState, types []model.RelationType) ([]model.Relation, error) {
	all := make([]model.Relation, 0)
	cursor := ""
	for {
		page, next, err := s.listRelations(ctx, queryer, graphID, contextID, states, types, cursor, relationScanPageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
}

func (s *Store) hasConfirmedRelationPath(ctx context.Context, queryer cypherQueryer, graphID model.ID, relationType model.RelationType, fromID, toID model.ID) (bool, error) {
	relations, err := s.allRelations(ctx, queryer, graphID, model.ID{}, []model.RelationState{model.RelationStateConfirmed}, []model.RelationType{relationType})
	if err != nil {
		return false, err
	}
	adjacent := make(map[model.ID][]model.ID)
	for _, relation := range relations {
		adjacent[relation.FromContextID] = append(adjacent[relation.FromContextID], relation.ToContextID)
	}
	visited := map[model.ID]struct{}{fromID: {}}
	frontier := []model.ID{fromID}
	for len(frontier) > 0 {
		var next []model.ID
		for _, current := range frontier {
			for _, target := range adjacent[current] {
				if target == toID {
					return true, nil
				}
				if _, found := visited[target]; !found {
					visited[target] = struct{}{}
					next = append(next, target)
				}
			}
		}
		frontier = next
	}
	return false, nil
}

// discardConfirmedRelationsForContext는 폐기한 사건이 가리키거나 가리켜지는 확정 관계를
// 같은 트랜잭션에서 폐기한다. 관계만 남아 삭제된 사건을 탐색 대상으로 만들지 않게 한다.
func (s *Store) discardConfirmedRelationsForContext(ctx context.Context, tx pgx.Tx, graphID, contextID model.ID, operation *OperationRecord) error {
	relations, err := s.allRelations(ctx, tx, graphID, contextID, []model.RelationState{model.RelationStateConfirmed}, nil)
	if err != nil {
		return fmt.Errorf("폐기 사건의 확정 관계 조회: %w", err)
	}
	for _, relation := range relations {
		now := time.Now().UTC()
		relation.State = model.RelationStateDiscarded
		relation.DeletedAt = &now
		relation.ConfirmedBy = model.ID{}
		relation.ConfirmedByAgent = model.ID{}
		relation.ConfirmedAt = nil
		if _, err := s.updateRelation(ctx, tx, graphID, relation); err != nil {
			return fmt.Errorf("폐기 사건 관계 상태 전이: %w", err)
		}
		if operation == nil {
			continue
		}
		relationOperation := *operation
		relationOperation.Kind = OperationDiscard
		if err := s.recordAppliedRelationOperation(ctx, tx, &relationOperation, relation.ID); err != nil {
			return fmt.Errorf("폐기 사건 관계 기록: %w", err)
		}
	}
	return nil
}

func normalizeRelation(relation model.Relation) model.Relation {
	if relation.Type == model.RelationTypeRelatesTo && relation.FromContextID.String() > relation.ToContextID.String() {
		relation.FromContextID, relation.ToContextID = relation.ToContextID, relation.FromContextID
	}
	return relation
}

func relationStateValues(states []model.RelationState) string {
	values := make([]string, 0, len(states))
	for _, state := range slices.Compact(slices.Clone(states)) {
		values = append(values, cypherString(string(state)))
	}
	return joinRelationValues(values)
}

func joinRelationValues(values []string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += ", "
		}
		result += value
	}
	return result
}

// relationFromCypher는 사건 관계 간선을 받는 모든 AGE 질의의 응답 조립 지점이다.
func (s *Store) relationFromCypher(ctx context.Context, queryer cypherQueryer, graphID model.ID, query string) (model.Relation, error) {
	var raw, rawSource, rawTarget string
	if err := queryer.QueryRow(ctx, s.cypherSQL(query, "edge agtype, source agtype, target agtype"), pgx.QueryExecModeExec).Scan(&raw, &rawSource, &rawTarget); err != nil {
		return model.Relation{}, storageError("relation_query", connectionError(ctx, "AGE 조회", err))
	}
	return parseStoredRelation(raw, rawSource, rawTarget, graphID)
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
