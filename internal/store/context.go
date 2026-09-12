package store

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// CreateContext은 정점과 계층별 참조 간선을 같은 Read Committed 트랜잭션에서 만든다.
// 원천은 같은 source_ref가 있으면 기존 정점을 반환한다.
func (s *Store) CreateContext(ctx context.Context, graphID model.ID, value model.Context, derivedFrom []model.ID) (model.Context, error) {
	return s.CreateContextWithOperation(ctx, graphID, value, derivedFrom, nil)
}

// CreateContextWithOperation은 새 컨텍스트와 적용 기록을 같은 트랜잭션에 남긴다.
func (s *Store) CreateContextWithOperation(ctx context.Context, graphID model.ID, value model.Context, derivedFrom []model.ID, operation *OperationRecord) (model.Context, error) {
	if err := validateContextInput(graphID, value, derivedFrom); err != nil {
		return model.Context{}, err
	}
	created, err := s.createContext(ctx, graphID, value, derivedFrom, operation)
	if err == nil {
		return created, nil
	}
	if value.Layer != model.LayerSource || !isUniqueViolation(err) {
		return model.Context{}, err
	}
	// 동시 요청이 부분 유일 인덱스에 먼저 도달한 경우에는 새 트랜잭션으로 기존 원천을 읽는다.
	existing, lookupErr := s.findSource(ctx, graphID, value.Source.Reference.Locator)
	if lookupErr != nil {
		return model.Context{}, fmt.Errorf("원천 중복 뒤 기존 정점 조회: %w", lookupErr)
	}
	return existing, nil
}

// Context는 graph_id 안에서 context_id에 해당하는 정점을 읽고 응답 격리를 재검사한다.
func (s *Store) Context(ctx context.Context, graphID, contextID model.ID) (model.Context, error) {
	if !graphID.IsV7() || !contextID.IsV7() {
		return model.Context{}, fmt.Errorf("그래프와 컨텍스트 식별자는 UUIDv7이어야 한다")
	}
	return s.context(ctx, s.pool, graphID, contextID)
}

// UpdateContext는 조건부 openCypher 갱신으로 판 번호의 비교와 증가를 같은 트랜잭션에 둔다.
func (s *Store) UpdateContext(ctx context.Context, graphID model.ID, expectedVersion int64, value model.Context) (model.Context, error) {
	return s.UpdateContextWithOperation(ctx, graphID, expectedVersion, value, nil)
}

// UpdateContextWithOperation은 갱신과 적용 기록을 같은 트랜잭션에 남긴다.
func (s *Store) UpdateContextWithOperation(ctx context.Context, graphID model.ID, expectedVersion int64, value model.Context, operation *OperationRecord) (model.Context, error) {
	if !graphID.IsV7() || value.GraphID != graphID || expectedVersion < 1 {
		return model.Context{}, fmt.Errorf("컨텍스트 갱신 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 갱신 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	previous, err := s.context(ctx, tx, graphID, value.ID)
	if err != nil {
		return model.Context{}, fmt.Errorf("갱신 전 컨텍스트 조회: %w", err)
	}
	if previous.Version != expectedVersion {
		return model.Context{}, VersionConflictError{Current: previous.Version}
	}
	if err := model.ValidateUpdate(previous, value); err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 갱신 검증: %w", err)
	}
	if value.Layer == model.LayerEvent {
		if err := s.validateEventMembers(ctx, tx, graphID, value); err != nil {
			return model.Context{}, err
		}
	}
	properties, err := encodeProperties(contextProperties(value))
	if err != nil {
		return model.Context{}, err
	}
	query := "MATCH (node:Context) WHERE node.context_id = " + cypherString(value.ID.String()) +
		" AND node.graph_id = " + cypherString(graphID.String()) +
		" AND node.version = " + fmt.Sprint(expectedVersion) +
		" SET node = " + properties + " RETURN node"
	stored, err := s.contextFromCypher(ctx, tx, graphID, query)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return model.Context{}, fmt.Errorf("컨텍스트 정점 갱신: %w", err)
		}
		if err := s.contextVersionConflict(ctx, tx, graphID, value.ID); err != nil {
			return model.Context{}, err
		}
		return model.Context{}, fmt.Errorf("컨텍스트 정점 갱신: %w", err)
	}
	if value.Layer == model.LayerEvent {
		if err := s.replaceEventMembers(ctx, tx, graphID, value); err != nil {
			return model.Context{}, err
		}
	}
	if previous.Body != value.Body {
		if err := s.enqueueIndexTask(ctx, tx, graphID, value.ID); err != nil {
			return model.Context{}, err
		}
	}
	if err := s.updateGraphActivity(ctx, tx, graphID, utf8.RuneCountInString(value.Body)-utf8.RuneCountInString(previous.Body)); err != nil {
		return model.Context{}, err
	}
	if err := s.recordAppliedOperation(ctx, tx, operation, stored.Version); err != nil {
		return model.Context{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 갱신 커밋: %w", err)
	}
	if value.Layer == model.LayerEvent {
		stored, err := s.context(ctx, s.pool, graphID, value.ID)
		if err != nil {
			return model.Context{}, fmt.Errorf("갱신한 사건 조립: %w", err)
		}
		return stored, nil
	}
	return stored, nil
}

// DiscardContext는 활성 컨텍스트에 폐기 시각을 표시하고 적용 기록을 함께 남긴다.
func (s *Store) DiscardContext(ctx context.Context, graphID, contextID model.ID, operation *OperationRecord) (model.Context, error) {
	return s.changeContextDeletion(ctx, graphID, contextID, operation, true)
}

// RestoreContext는 연산으로 폐기한 컨텍스트의 폐기 시각을 지우고 적용 기록을 함께 남긴다.
func (s *Store) RestoreContext(ctx context.Context, graphID, contextID model.ID, operation *OperationRecord) (model.Context, error) {
	return s.changeContextDeletion(ctx, graphID, contextID, operation, false)
}

func (s *Store) changeContextDeletion(ctx context.Context, graphID, contextID model.ID, operation *OperationRecord, discard bool) (model.Context, error) {
	if !graphID.IsV7() || !contextID.IsV7() {
		return model.Context{}, fmt.Errorf("그래프와 컨텍스트 식별자는 UUIDv7이어야 한다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 상태 전이 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	previous, err := s.context(ctx, tx, graphID, contextID)
	if err != nil {
		return model.Context{}, err
	}
	if discard && previous.DeletedAt != nil {
		return model.Context{}, fmt.Errorf("컨텍스트가 이미 폐기됐다")
	}
	if !discard && previous.DeletedAt == nil {
		return model.Context{}, fmt.Errorf("컨텍스트가 활성 상태다")
	}
	if discard {
		if err := s.invalidateDerivedEvidence(ctx, tx, graphID, contextID); err != nil {
			return model.Context{}, err
		}
	}
	next := previous
	if discard {
		now := nowUTC()
		next.DeletedAt = &now
	} else {
		next.DeletedAt = nil
	}
	if next.Layer != model.LayerSource {
		next.Version++
	}
	if err := model.ValidateUpdate(previous, next); err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 상태 전이 검증: %w", err)
	}
	properties, err := encodeProperties(contextProperties(next))
	if err != nil {
		return model.Context{}, err
	}
	query := "MATCH (node:Context) WHERE node.context_id = " + cypherString(contextID.String()) +
		" AND node.graph_id = " + cypherString(graphID.String()) +
		" SET node = " + properties + " RETURN node"
	stored, err := s.contextFromCypher(ctx, tx, graphID, query)
	if err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 상태 전이: %w", err)
	}
	delta := -utf8.RuneCountInString(previous.Body)
	if !discard {
		delta = -delta
	}
	if err := s.updateGraphActivity(ctx, tx, graphID, delta); err != nil {
		return model.Context{}, err
	}
	if err := s.recordAppliedOperation(ctx, tx, operation, stored.Version); err != nil {
		return model.Context{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 상태 전이 커밋: %w", err)
	}
	return stored, nil
}

// invalidateDerivedEvidence는 폐기한 컨텍스트를 근거로 가리키는 파생에
// 근거 무효 표시를 한 단계만 전파하고 연쇄로 퍼지지 않게 한다.
func (s *Store) invalidateDerivedEvidence(ctx context.Context, tx pgx.Tx, graphID, evidenceID model.ID) error {
	query := "MATCH (derived:Context)-[edge:DERIVED_FROM]->(evidence:Context) WHERE evidence.context_id = " + cypherString(evidenceID.String()) +
		" AND evidence.graph_id = " + cypherString(graphID.String()) +
		" AND derived.graph_id = " + cypherString(graphID.String()) +
		" AND edge.graph_id = " + cypherString(graphID.String()) + " RETURN derived"
	rows, err := tx.Query(ctx, s.cypherSQL(query, "derived agtype"))
	if err != nil {
		return fmt.Errorf("근거 무효 전파 대상 조회: %w", err)
	}
	defer rows.Close()
	derived := make([]model.Context, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("근거 무효 전파 대상 행 해석: %w", err)
		}
		value, err := parseContext(raw, graphID)
		if err != nil {
			return err
		}
		derived = append(derived, value)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("근거 무효 전파 대상 행 읽기: %w", err)
	}
	for _, previous := range derived {
		if previous.Derived == nil || previous.Derived.EvidenceInvalidated {
			continue
		}
		next := previous
		derived := *previous.Derived
		next.Derived = &derived
		next.Version++
		next.Derived.EvidenceInvalidated = true
		if err := model.ValidateEvidenceInvalidation(previous, next); err != nil {
			return fmt.Errorf("근거 무효 표시 검증: %w", err)
		}
		properties, err := encodeProperties(contextProperties(next))
		if err != nil {
			return err
		}
		update := "MATCH (node:Context) WHERE node.context_id = " + cypherString(previous.ID.String()) +
			" AND node.graph_id = " + cypherString(graphID.String()) +
			" AND node.version = " + fmt.Sprint(previous.Version) +
			" AND node.evidence_invalidated <> true SET node = " + properties
		if _, err := tx.Exec(ctx, s.cypherSQL(update, "updated agtype")); err != nil {
			return fmt.Errorf("근거 무효 표시 전파: %w", err)
		}
	}
	return nil
}

// createContext은 원천 중복 확인과 그래프 활동 갱신을 포함한 실제 쓰기 트랜잭션이다.
func (s *Store) createContext(ctx context.Context, graphID model.ID, value model.Context, derivedFrom []model.ID, operation *OperationRecord) (model.Context, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 생성 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	if value.Layer == model.LayerSource {
		existing, err := s.findSourceWith(ctx, tx, graphID, value.Source.Reference.Locator)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return model.Context{}, fmt.Errorf("원천 중복 조회: %w", err)
		}
	}
	if value.Layer == model.LayerDerived {
		if err := s.validateReferences(ctx, tx, graphID, derivedFrom); err != nil {
			return model.Context{}, err
		}
	}
	if value.Layer == model.LayerEvent {
		if err := s.validateEventMembers(ctx, tx, graphID, value); err != nil {
			return model.Context{}, err
		}
	}
	properties, err := encodeProperties(contextProperties(value))
	if err != nil {
		return model.Context{}, err
	}
	stored, err := s.contextFromCypher(ctx, tx, graphID, "CREATE (node:Context "+properties+") RETURN node")
	if err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 정점 생성: %w", err)
	}
	if err := s.createReferenceEdges(ctx, tx, graphID, value, derivedFrom); err != nil {
		return model.Context{}, err
	}
	if err := s.enqueueIndexTask(ctx, tx, graphID, value.ID); err != nil {
		return model.Context{}, err
	}
	if err := s.updateGraphActivity(ctx, tx, graphID, utf8.RuneCountInString(value.Body)); err != nil {
		return model.Context{}, err
	}
	if err := s.recordAppliedOperation(ctx, tx, operation, stored.Version); err != nil {
		return model.Context{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Context{}, fmt.Errorf("컨텍스트 생성 커밋: %w", err)
	}
	if value.Layer == model.LayerEvent {
		stored, err = s.context(ctx, s.pool, graphID, value.ID)
		if err != nil {
			return model.Context{}, fmt.Errorf("생성한 사건 조립: %w", err)
		}
	}
	return stored, nil
}

// validateContextInput은 도메인 규칙과 접근 계층의 graph_id 일치를 먼저 확인한다.
func validateContextInput(graphID model.ID, value model.Context, derivedFrom []model.ID) error {
	if !graphID.IsV7() || value.GraphID != graphID {
		return fmt.Errorf("요청 graph_id와 컨텍스트 graph_id가 일치하지 않는다")
	}
	if err := value.Validate(); err != nil {
		return fmt.Errorf("컨텍스트 검증: %w", err)
	}
	if value.Layer == model.LayerDerived {
		if err := model.ValidateDerivedReferences(derivedFrom); err != nil {
			return fmt.Errorf("파생 근거 검증: %w", err)
		}
	} else if len(derivedFrom) > 0 {
		return fmt.Errorf("파생이 아닌 컨텍스트에는 derived_from을 둘 수 없다")
	}
	return nil
}

// validateReferences는 모든 파생 근거가 요청 그래프에 속하는지 확인한다.
func (s *Store) validateReferences(ctx context.Context, tx pgx.Tx, graphID model.ID, referenceIDs []model.ID) error {
	for _, referenceID := range referenceIDs {
		if _, err := s.context(ctx, tx, graphID, referenceID); err != nil {
			return fmt.Errorf("파생 근거 %s 조회: %w", referenceID, err)
		}
	}
	return nil
}

// validateEventMembers는 사건 구성원이 요청 그래프의 원천 또는 파생인지 검증한다.
func (s *Store) validateEventMembers(ctx context.Context, tx pgx.Tx, graphID model.ID, event model.Context) error {
	members := make([]model.Context, 0, len(event.Event.MemberIDs))
	for _, memberID := range event.Event.MemberIDs {
		member, err := s.context(ctx, tx, graphID, memberID)
		if err != nil {
			return fmt.Errorf("사건 구성원 %s 조회: %w", memberID, err)
		}
		members = append(members, member)
	}
	if err := model.ValidateEventMembers(event, members); err != nil {
		return fmt.Errorf("사건 구성원 검증: %w", err)
	}
	return nil
}

// createReferenceEdges는 파생 근거 또는 사건 구성원 edge를 정점과 같은 트랜잭션에서 만든다.
func (s *Store) createReferenceEdges(ctx context.Context, tx pgx.Tx, graphID model.ID, value model.Context, derivedFrom []model.ID) error {
	var label string
	var targetIDs []model.ID
	switch value.Layer {
	case model.LayerDerived:
		label, targetIDs = "DERIVED_FROM", derivedFrom
	case model.LayerEvent:
		label, targetIDs = "HAS_MEMBER", value.Event.MemberIDs
	default:
		return nil
	}
	for _, targetID := range targetIDs {
		query := "MATCH (from:Context), (to:Context) WHERE from.context_id = " + cypherString(value.ID.String()) +
			" AND from.graph_id = " + cypherString(graphID.String()) +
			" AND to.context_id = " + cypherString(targetID.String()) +
			" AND to.graph_id = " + cypherString(graphID.String()) +
			" CREATE (from)-[:" + label + " {graph_id: " + cypherString(graphID.String()) + "}]->(to)"
		if _, err := tx.Exec(ctx, s.cypherSQL(query, "created agtype")); err != nil {
			return fmt.Errorf("%s 참조 간선 생성: %w", label, err)
		}
	}
	return nil
}

// replaceEventMembers는 사건 갱신 때만 기존 구성원 간선을 새 구성원 집합으로 교체한다.
func (s *Store) replaceEventMembers(ctx context.Context, tx pgx.Tx, graphID model.ID, event model.Context) error {
	deleteQuery := "MATCH (event:Context)-[edge:HAS_MEMBER]->() WHERE event.context_id = " + cypherString(event.ID.String()) +
		" AND event.graph_id = " + cypherString(graphID.String()) +
		" AND edge.graph_id = " + cypherString(graphID.String()) + " DELETE edge"
	if _, err := tx.Exec(ctx, s.cypherSQL(deleteQuery, "deleted agtype")); err != nil {
		return fmt.Errorf("사건 구성원 간선 삭제: %w", err)
	}
	return s.createReferenceEdges(ctx, tx, graphID, event, nil)
}

// context는 AGE 조회 결과를 한곳에서 model.Context로 조립해 graph_id를 재검사한다.
func (s *Store) context(ctx context.Context, queryer cypherQueryer, graphID, contextID model.ID) (model.Context, error) {
	query := "MATCH (node:Context) WHERE node.context_id = " + cypherString(contextID.String()) +
		" AND node.graph_id = " + cypherString(graphID.String()) + " RETURN node"
	stored, err := s.contextFromCypher(ctx, queryer, graphID, query)
	if err != nil || stored.Layer != model.LayerEvent {
		return stored, err
	}
	memberIDs, err := s.eventMemberIDs(ctx, queryer, graphID, contextID)
	if err != nil {
		return model.Context{}, err
	}
	stored.Event.MemberIDs = memberIDs
	if err := stored.Validate(); err != nil {
		return model.Context{}, fmt.Errorf("저장된 사건 컨텍스트 검증: %w", err)
	}
	return stored, nil
}

// eventMemberIDs는 HAS_MEMBER 간선에서 사건 구성원 식별자를 다시 조립한다.
func (s *Store) eventMemberIDs(ctx context.Context, queryer cypherQueryer, graphID, eventID model.ID) ([]model.ID, error) {
	query := "MATCH (event:Context)-[edge:HAS_MEMBER]->(member:Context) WHERE event.context_id = " + cypherString(eventID.String()) +
		" AND event.graph_id = " + cypherString(graphID.String()) +
		" AND edge.graph_id = " + cypherString(graphID.String()) +
		" AND member.graph_id = " + cypherString(graphID.String()) + " RETURN member"
	rows, err := queryer.Query(ctx, s.cypherSQL(query, "member agtype"))
	if err != nil {
		return nil, fmt.Errorf("사건 구성원 조회: %w", err)
	}
	defer rows.Close()
	memberIDs := make([]model.ID, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("사건 구성원 행 해석: %w", err)
		}
		member, err := parseContext(raw, graphID)
		if err != nil {
			return nil, err
		}
		memberIDs = append(memberIDs, member.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("사건 구성원 행 읽기: %w", err)
	}
	return memberIDs, nil
}

// contextFromCypher는 정점 하나를 받는 모든 AGE 질의의 응답 조립 지점이다.
func (s *Store) contextFromCypher(ctx context.Context, queryer cypherQueryer, graphID model.ID, query string) (model.Context, error) {
	var raw string
	if err := queryer.QueryRow(ctx, s.cypherSQL(query, "node agtype")).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Context{}, ErrNotFound
		}
		return model.Context{}, err
	}
	return parseContext(raw, graphID)
}

// findSource는 원천 중복 재시도 경로가 새 트랜잭션에서 기존 정점을 읽게 한다.
func (s *Store) findSource(ctx context.Context, graphID model.ID, locator string) (model.Context, error) {
	return s.findSourceWith(ctx, s.pool, graphID, locator)
}

// findSourceWith는 같은 그래프와 locator를 모두 대조해 원천 중복을 찾는다.
func (s *Store) findSourceWith(ctx context.Context, queryer cypherQueryer, graphID model.ID, locator string) (model.Context, error) {
	query := "MATCH (node:Context) WHERE node.graph_id = " + cypherString(graphID.String()) +
		" AND node.layer = 'source' AND node.source_ref_locator = " + cypherString(locator) + " RETURN node"
	return s.contextFromCypher(ctx, queryer, graphID, query)
}

// updateGraphActivity는 컨텍스트 변경과 같은 트랜잭션에서 저장 문자 수와 최근 활동을 갱신한다.
func (s *Store) updateGraphActivity(ctx context.Context, tx pgx.Tx, graphID model.ID, characterDelta int) error {
	command, err := tx.Exec(ctx, `
		UPDATE public.context_graph
		SET last_activity_at = $1, stored_chars = stored_chars + $2
		WHERE graph_id = $3 AND stored_chars + $2 >= 0`, nowUTC(), characterDelta, graphID.String())
	if err != nil {
		return fmt.Errorf("그래프 활동 갱신: %w", err)
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// contextVersionConflict은 같은 트랜잭션에서 현재 판 번호를 읽어 충돌 또는 부재를 구분한다.
func (s *Store) contextVersionConflict(ctx context.Context, tx pgx.Tx, graphID, contextID model.ID) error {
	current, err := s.context(ctx, tx, graphID, contextID)
	if err != nil {
		return err
	}
	return VersionConflictError{Current: current.Version}
}

// cypherQueryer는 연결 풀과 트랜잭션이 공유하는 AGE 단일 행 질의 기능만 노출한다.
type cypherQueryer interface {
	// Query는 여러 AGE 결과 행을 스트리밍으로 읽는다.
	Query(context.Context, string, ...any) (pgx.Rows, error)
	// QueryRow는 AGE 결과 한 행을 읽는다.
	QueryRow(context.Context, string, ...any) pgx.Row
}

// cypherSQL은 검증된 graphName과 이스케이프한 openCypher 문자열로 AGE 호출 SQL을 만든다.
func (s *Store) cypherSQL(query, columns string) string {
	return "SELECT * FROM ag_catalog.cypher(" + sqlString(s.graphName) + ", " + dollarString(query) + ") AS (" + columns + ")"
}

// sqlString은 AGE 호출 안에 넣을 PostgreSQL 문자열 리터럴을 만든다.
func sqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// dollarString은 AGE가 요구하는 dollar-quoted openCypher 문자열을 만든다.
// 본문에 같은 구분자가 있으면 다른 태그를 찾아 SQL 문자열 경계가 깨지지 않게 한다.
func dollarString(value string) string {
	for index := range 1_000_000 {
		tag := fmt.Sprintf("$store%d$", index)
		if !strings.Contains(value, tag) {
			return tag + value + tag
		}
	}
	panic("openCypher dollar quote 태그를 만들 수 없다")
}

// cypherString은 openCypher 문자열 리터럴을 JSON 방식으로 이스케이프한다.
func cypherString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// encodeProperties는 값은 JSON으로 이스케이프하고 고정 property 이름은 Cypher map key로 조립한다.
func encodeProperties(properties map[string]any) (string, error) {
	keys := slices.Sorted(maps.Keys(properties))
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		encoded, err := json.Marshal(properties[key])
		if err != nil {
			return "", fmt.Errorf("AGE property %q 인코딩: %w", key, err)
		}
		parts = append(parts, key+": "+string(encoded))
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

// isUniqueViolation은 부분 유일 인덱스를 포함한 PostgreSQL 유일성 위반을 판별한다.
func isUniqueViolation(err error) bool {
	pgError, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgError.Code == "23505"
}
