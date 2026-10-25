package main

import (
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestEvaluationPreparationRejectsInvalidScopeBeforeAccess(t *testing.T) {
	for _, limit := range []int{-1, 0} {
		if _, err := prepareEvaluationGraph(t.Context(), nil, nil, contextSet{}, limit); err == nil {
			t.Fatal("잘못된 상한에서 저장소에 접근했다")
		}
	}
	for _, id := range []model.ID{{}, {6: 0x40, 8: 0x80}} {
		if err := drainIndexQueue(t.Context(), nil, nil, id, 1); err == nil || !strings.Contains(err.Error(), "UUIDv7") {
			t.Fatalf("잘못된 그래프 범위를 거부하지 않았다: %v", err)
		}
	}
}
