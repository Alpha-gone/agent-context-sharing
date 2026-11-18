package store

import (
	"testing"

	"agent_context_sharing/internal/model"
)

func TestIngestPreviousRelationIntegration(t *testing.T) {
	database, _, pool := newAgentContractDatabase(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	source, err := database.CreateContext(t.Context(), graph, conversationSource(t, graph, actor, "synthetic", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := source.Source.OccurredAt
	first := conversationEvent(t, graph, actor, source.ID, "synthetic", now, nil)
	first, err = database.CreateContext(t.Context(), graph, first, nil)
	if err != nil {
		t.Fatal(err)
	}
	second := conversationEvent(t, graph, actor, source.ID, "synthetic", now, nil)
	second.Event.PreviousEventID = new(first.ID)
	op := &OperationRecord{Kind: OperationAdd, GraphID: graph, ContextID: second.ID, AccountID: actor, AgentID: actor, JudgmentInput: "segment", JudgmentSource: model.ConfirmationSourceIngest}
	if _, err = database.CreateContextWithOperation(t.Context(), graph, second, nil, op, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	relation := lifeProposalIdentity(t, database, graph, model.RelationTypePrecedes, first.ID, second.ID)
	if relation.ConfirmedSource != model.ConfirmationSourceIngest || relation.ProposedBy != model.ProposalSourceSystem {
		t.Fatalf("수집 관계 출처: %+v", relation)
	}
	var sourceName string
	if err = pool.QueryRow(t.Context(), `SELECT judgment_source FROM public.operation_log WHERE graph_id=$1 AND relation_id=$2`, graph.String(), relation.ID.String()).Scan(&sourceName); err != nil || sourceName != "ingest" {
		t.Fatalf("관계 감사 출처: %s %v", sourceName, err)
	}
}
