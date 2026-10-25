package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func validateAttackBefore(queryID string, actual, baseline response, sample attackSample, base baseShape) error {
	if actual.signature() != baseline.signature() || !slices.Equal(actual.Entries, baseline.Entries) {
		return fmt.Errorf("공격 전 질의 %q가 기준선과 다르다", queryID)
	}
	for _, criteria := range sample.SuccessCriteria {
		if criteria.QueryID == queryID && meetsCriteria(actual, criteria) {
			return fmt.Errorf("공격 전 성공 조건이 이미 성립한다")
		}
	}
	for _, path := range sample.ExpectedPaths {
		if path.QueryID == queryID {
			if _, warned := diagnosePath(actual, path, base); warned {
				return fmt.Errorf("공격 전 정상 기대 경로가 성립하지 않는다")
			}
		}
	}
	return nil
}

func attackContextReferences(spec contextSpec) []string {
	if spec.Event != nil {
		return spec.Event.Members
	}
	if spec.Derived != nil {
		return spec.Derived.DerivedFrom
	}
	return nil
}

// 입력 파일의 순서와 무관하게 구성원·근거를 사용하는 컨텍스트를 먼저 폐기한다.
func attackRecoveryOrder(specs []contextSpec) ([]contextSpec, error) {
	pending := slices.Clone(specs)
	order := make([]contextSpec, 0, len(specs))
	for len(pending) > 0 {
		index := slices.IndexFunc(pending, func(spec contextSpec) bool {
			return !slices.ContainsFunc(pending, func(other contextSpec) bool { return slices.Contains(attackContextReferences(other), spec.Key) })
		})
		if index < 0 {
			return nil, fmt.Errorf("추가 컨텍스트 참조가 순환한다")
		}
		order = append(order, pending[index])
		pending = slices.Delete(pending, index, index+1)
	}
	return order, nil
}

func attackOperation(graph loadedGraph, kind store.OperationKind, judgment string) *store.OperationRecord {
	return &store.OperationRecord{Kind: kind, GraphID: graph.GraphID, AccountID: graph.AccountID, AgentID: graph.AccountID, JudgmentInput: judgment}
}

func attackRelationJudgment(phase string, relation relationSpec) string {
	relation = relation.normalized()
	return fmt.Sprintf("adversarial:%s:%q:%q:%q", phase, relation.Type, relation.From, relation.To)
}

func confirmAttackRelations(ctx context.Context, database *store.Store, graph loadedGraph, relations []relationSpec, phase string) error {
	for _, spec := range relations {
		id, err := model.NewID()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		relation := model.Relation{ID: id, GraphID: graph.GraphID, Type: model.RelationType(spec.Type), FromContextID: graph.Keys[spec.From], ToContextID: graph.Keys[spec.To], State: model.RelationStateConfirmed, ProposedBy: model.ProposalSourceAgent, ProposedAt: now, ConfirmedBy: graph.AccountID, ConfirmedByAgent: graph.AccountID, ConfirmedAt: &now}
		if _, err := database.ConfirmRelation(ctx, graph.GraphID, relation, attackOperation(graph, store.OperationAdd, attackRelationJudgment(phase, spec))); err != nil {
			return err
		}
	}
	return nil
}
