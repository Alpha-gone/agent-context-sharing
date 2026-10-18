package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent_context_sharing/internal/model"
)

// OperationTarget은 감사에서 되짚을 컨텍스트 또는 사건 관계 중 하나다.
type OperationTarget struct {
	ContextID  model.ID
	RelationID model.ID
}

// OperationEntry는 원래 판단·주체·결과를 보존한 관리 연산 기록이다.
type OperationEntry struct {
	ID model.ID
	model.ManagementOperation
}

// OperationHistory는 소유자에게 대상의 적용·거부 이력을 오래된 순서로 반환한다.
// 판단 입력은 불투명한 텍스트다. 임의로 ID를 추출하거나 외부 위치에 접근하지 않는다.
// 웹 감사 조립용 경계이며 MCP 도구에는 제공하지 않는다.
func (s *Store) OperationHistory(ctx context.Context, graphID, actorID model.ID, target OperationTarget) (result []OperationEntry, err error) {
	if !graphID.IsV7() || !actorID.IsV7() ||
		(!target.ContextID.IsZero() && !target.ContextID.IsV7()) || (!target.RelationID.IsZero() && !target.RelationID.IsV7()) ||
		target.ContextID.IsZero() == target.RelationID.IsZero() {
		return nil, model.FieldError{Field: "target", Message: "감사 조회는 그래프·계정과 대상 하나의 UUIDv7가 필요하다"}
	}
	ctx, release, err := s.enterHopReadScope(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := release(); closeErr != nil {
			result, err = nil, errors.Join(err, closeErr)
		}
		err = connectionError(ctx, "관리 연산 이력 조회", err)
	}()
	grade, found, err := effectiveGrade(ctx, s.reader(ctx), graphID, actorID)
	if err != nil {
		return nil, err
	}
	if !found || grade != model.GraphGradeOwner {
		return nil, ErrNotFound
	}
	column, id := "context_id", target.ContextID
	if !target.RelationID.IsZero() {
		column, id = "relation_id", target.RelationID
	}
	rows, err := s.reader(ctx).Query(ctx, `SELECT operation_id::text, graph_id::text, operation_kind,
		context_id::text, relation_id::text, target_version, judgment_input, actor_account_id::text,
		actor_agent_id::text, applied_at, result, COALESCE(reject_reason,'')
		FROM public.operation_log WHERE graph_id=$1 AND `+column+`=$2 ORDER BY applied_at, operation_id`, graphID.String(), id.String())
	if err != nil {
		return nil, storageError("operation_history_query", err)
	}
	defer rows.Close()
	result = make([]OperationEntry, 0)
	for rows.Next() {
		var entry OperationEntry
		var rawID, rawGraph, rawAccount, rawAgent string
		var rawContext, rawRelation *string
		var version *int64
		if err := rows.Scan(&rawID, &rawGraph, &entry.Kind, &rawContext, &rawRelation, &version, &entry.JudgmentInput,
			&rawAccount, &rawAgent, &entry.AppliedAt, &entry.Result, &entry.RejectionReason); err != nil {
			return nil, storageError("operation_history_scan", err)
		}
		for _, field := range []struct {
			raw *string
			id  *model.ID
		}{{&rawID, &entry.ID}, {&rawGraph, &entry.GraphID}, {&rawAccount, &entry.AccountID}, {&rawAgent, &entry.AgentID}, {rawContext, &entry.ContextID}, {rawRelation, &entry.RelationID}} {
			if field.raw != nil {
				*field.id, err = model.ParseID(*field.raw)
				if err != nil {
					return nil, storageError("operation_history_id", err)
				}
			}
		}
		if version != nil {
			entry.TargetVersion = *version
		}
		entry.AppliedAt = entry.AppliedAt.UTC()
		if err := entry.Validate(); err != nil {
			return nil, storageError("operation_history_value", err)
		}
		if entry.GraphID != graphID || entry.ContextID != target.ContextID || entry.RelationID != target.RelationID {
			return nil, storageError("operation_history_scope", ErrInvariantViolation)
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, storageError("operation_history_rows", err)
	}
	return result, nil
}

// ContextImpact는 소유자의 감사에서 근거의 역방향·구성원 역방향과 확정 관계를 따른다.
// 폐기된 시작점도 조회하되 이웃의 삭제 판정은 홉 탐색과 같다. 깊이·플랜·결과 수로
// 자르지 않으며 취소·마감·저장 오류는 부분 결과 없이 반환한다. 내용과 색인을 변경하지 않는다.
func (s *Store) ContextImpact(ctx context.Context, graphID, contextID, actorID model.ID) (result HopResult, err error) {
	if !graphID.IsV7() || !contextID.IsV7() || !actorID.IsV7() {
		return HopResult{}, model.FieldError{Field: "context_id", Message: "영향 조회 식별자는 UUIDv7이어야 한다"}
	}
	ctx, cancel := context.WithTimeout(ctx, hopWorkTimeout)
	defer cancel()
	ctx, release, err := s.enterHopReadScope(ctx)
	if err != nil {
		return HopResult{}, err
	}
	defer func() {
		if closeErr := release(); closeErr != nil {
			result, err = HopResult{}, errors.Join(err, closeErr)
		}
		err = connectionError(ctx, "관리 영향 조회", err)
	}()
	grade, found, err := effectiveGrade(ctx, s.reader(ctx), graphID, actorID)
	if err != nil {
		return HopResult{}, err
	}
	if !found || grade != model.GraphGradeOwner {
		return HopResult{}, ErrNotFound
	}
	start, err := s.context(ctx, s.reader(ctx), graphID, contextID)
	if err != nil {
		return HopResult{}, err
	}
	labels, err := traversalLabels([]string{"derived_from", "has_member", "precedes", "causes", "part_of", "relates_to"})
	if err != nil {
		return HopResult{}, err
	}
	result = HopResult{Contexts: []model.Context{start}, Distances: map[model.ID]int{contextID: 0}}
	frontier := []model.ID{contextID}
	edges := make(map[string]HopEdge)
	for depth := 1; len(frontier) > 0; depth++ {
		next := make([]model.ID, 0)
		for _, label := range labels {
			direction := "in"
			if label.confirmed {
				direction = "both"
			}
			neighbors, err := s.hopNeighbors(ctx, graphID, frontier, label, direction)
			if err != nil {
				return HopResult{}, err
			}
			for _, neighbor := range neighbors {
				if neighbor.context.DeletedAt != nil {
					continue
				}
				key := fmt.Sprintf("%s|%s|%s", neighbor.fromID, neighbor.toID, label.kind)
				edges[key] = HopEdge{FromID: neighbor.fromID, ToID: neighbor.toID, Kind: label.kind}
				if _, seen := result.Distances[neighbor.context.ID]; seen {
					continue
				}
				result.Distances[neighbor.context.ID] = depth
				result.Contexts = append(result.Contexts, neighbor.context)
				next = append(next, neighbor.context.ID)
			}
		}
		frontier = next
	}
	result.Edges = slices.SortedFunc(edgesValues(edges, result.Distances), compareHopEdges)
	if _, err := s.fillHopReferences(ctx, graphID, result.Contexts); err != nil {
		return HopResult{}, err
	}
	return result, nil
}
