package store

import (
	"context"
	json "encoding/json/v2"
	"reflect"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestInferenceRelationCodecAndAudit(t *testing.T) {
	graph, from, to, account, decision := newTestID(t), newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	if from.Compare(to) > 0 {
		from, to = to, from
	}
	now := time.Now().UTC()
	relation := model.Relation{ID: newTestID(t), GraphID: graph, Type: model.RelationTypeRelatesTo,
		FromContextID: from, ToContextID: to, State: model.RelationStateConfirmed,
		ProposedBy: model.ProposalSourceInference, ProposedAt: now, ConfirmedBy: account,
		ConfirmedSource: model.ConfirmationSourceInference, ConfirmedAt: &now, InferenceDecisionID: decision}
	raw, err := json.Marshal(map[string]any{"label": "RELATES_TO", "properties": relationProperties(relation)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseRelation(string(raw)+"::edge", graph)
	if err != nil || !reflect.DeepEqual(got, relation) {
		t.Fatalf("추론 관계 왕복: %+v, %v", got, err)
	}
	edge := auditEdge{label: "RELATES_TO", graphID: graph.String(), relationID: relation.ID.String(),
		state: "confirmed", from: from.String(), to: to.String(), proposedBy: "inference",
		confirmedSource: "inference", decisionID: decision.String(), inferenceProperties: true}
	snapshot := auditSnapshot{graphID: graph.String(), edges: []auditEdge{edge}, decisions: map[string]auditInferenceDecision{
		decision.String(): {graphID: graph.String(), target: from.String(), candidate: to.String(), relationType: "relates_to", relationID: relation.ID.String()},
	}}
	violations, err := checkRelationInference(snapshot)
	if err != nil || len(violations) != 0 {
		t.Fatalf("유효한 판정: %v, %v", violations, err)
	}
	wrong := snapshot.decisions[decision.String()]
	wrong.graphID = newTestID(t).String()
	snapshot.decisions[decision.String()] = wrong
	violations, err = checkRelationInference(snapshot)
	if err != nil || len(violations) != 1 {
		t.Fatalf("다른 그래프 판정: %v, %v", violations, err)
	}
}

func TestInferenceDecisionBindingIntegration(t *testing.T) {
	db := newSmallPoolStore(t)
	graph, target, candidate, run, decision, relationID := newTestID(t), newTestID(t), newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	err = insertInferenceRun(t.Context(), tx, inferenceRunRecord{ID: run, GraphID: graph, ContextID: target,
		TargetVersion: 1, ProfileFingerprint: "profile", ProfileVersions: "{}", Candidates: "[]", Result: "applied", Attempts: 1, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.pool.Exec(ctx, `DELETE FROM public.relation_inference_run WHERE run_id=$1`, run.String()); err != nil {
			t.Errorf("판정 표본 정리: %v", err)
		}
	})
	err = insertInferenceDecision(t.Context(), tx, inferenceDecisionRecord{ID: decision, RunID: run, CandidateID: candidate,
		CandidateVersion: 1, RelationType: model.RelationTypeCauses, Direction: "T_to_C", Decision: "relation",
		Evidence: "[]", ValidationResult: "valid", RelationID: relationID})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	relation := model.Relation{ID: relationID, GraphID: graph, Type: model.RelationTypeCauses, FromContextID: target, ToContextID: candidate, InferenceDecisionID: decision}
	if err := validateRelationDecision(t.Context(), db.pool, graph, relation); err != nil {
		t.Fatalf("일치 판정 거부: %v", err)
	}
	for _, scenario := range []string{"graph", "candidate", "type", "relation"} {
		t.Run(scenario, func(t *testing.T) {
			value, graphID := relation, graph
			switch scenario {
			case "graph":
				graphID = newTestID(t)
			case "candidate":
				value.ToContextID = newTestID(t)
			case "type":
				value.Type = model.RelationTypePartOf
			case "relation":
				value.ID = newTestID(t)
			}
			if err := validateRelationDecision(t.Context(), db.pool, graphID, value); err == nil {
				t.Fatal("불일치 판정을 허용했다")
			}
		})
	}
}
