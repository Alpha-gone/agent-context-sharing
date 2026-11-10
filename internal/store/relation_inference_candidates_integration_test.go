package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func TestInferenceCandidatesIndexWaitAndGraphScope(t *testing.T) {
	db := newIsolatedIntegrationStore(t)
	graph, actor, member := lifeProposalFixture(t, db)
	targetInput := testEventContext(t, graph, actor, member.ID)
	targetInput.Body = "shared keyword target"
	target, err := db.CreateContext(t.Context(), graph, targetInput, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidateInput := testEventContext(t, graph, actor, member.ID)
	candidateInput.Body = "shared keyword candidate"
	candidate, err := db.CreateContext(t.Context(), graph, candidateInput, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherGraph, otherActor, otherMember := lifeProposalFixture(t, db)
	other := testEventContext(t, otherGraph, otherActor, otherMember.ID)
	other.Body = "shared keyword target"
	if _, err := db.CreateContext(t.Context(), otherGraph, other, nil); err != nil {
		t.Fatal(err)
	}
	request := model.InferenceCandidateRequest{Task: model.RelationInferenceTask{GraphID: graph, ContextID: target.ID, TargetVersion: target.Version, Generation: time.Now().UTC()}, ProfileFingerprint: "synthetic", EmbeddingModel: "test", Channels: []string{"semantic", "keyword", "member", "rule", "conversation"}, Limit: 10, IndexWait: time.Hour, AdjacencyWindow: time.Hour}
	snapshot, err := db.InferenceCandidates(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.IndexWaitUntil == nil {
		t.Fatal("색인 대기를 반환하지 않았다")
	}
	request.Channels = []string{"member"}
	snapshot, err = db.InferenceCandidates(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.IndexWaitUntil != nil || snapshot.SemanticUnavailable || len(snapshot.Channels) != 1 || len(snapshot.Channels[0].Candidates) != 1 || snapshot.Channels[0].Candidates[0].Event.ID != candidate.ID {
		t.Fatalf("구성원 후보: %+v", snapshot)
	}
	request.Channels = []string{"semantic", "keyword", "member", "rule", "conversation"}
	request.Task.Generation = time.Now().Add(-2 * time.Hour).UTC()
	snapshot, err = db.InferenceCandidates(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.SemanticUnavailable || snapshot.IndexWaitUntil != nil {
		t.Fatalf("기한 만료: %+v", snapshot)
	}
	found := false
	for _, channel := range snapshot.Channels {
		if channel.Name == "semantic" {
			t.Fatal("의미 채널을 생략하지 않았다")
		}
		for _, value := range channel.Candidates {
			if value.Event.GraphID != graph || value.Event.ID == target.ID || value.Event.ID == other.ID {
				t.Fatal("그래프 경계 위반")
			}
			if value.Event.ID == candidate.ID && slices.Contains(value.Allowed, model.RelationTypeRelatesTo) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("같은 그래프 후보가 없다")
	}
	if _, err := db.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypeRelatesTo, target.ID, candidate.ID), nil); err != nil {
		t.Fatal(err)
	}
	request.Channels = []string{"member"}
	snapshot, err = db.InferenceCandidates(t.Context(), request)
	if err != nil || len(snapshot.Channels) != 1 || len(snapshot.Channels[0].Candidates) != 1 {
		t.Fatalf("확정 관계 후보: %+v %v", snapshot, err)
	}
	allowed := snapshot.Channels[0].Candidates[0].Allowed
	if slices.Contains(allowed, model.RelationTypeRelatesTo) || !slices.Contains(allowed, model.RelationTypeCauses) {
		t.Fatalf("확정 유형 제외: %v", allowed)
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE public.index_task SET state='failed' WHERE context_id=$1`, target.ID.String()); err != nil {
		t.Fatal(err)
	}
	request.Task.Generation = time.Now().UTC()
	request.Channels = []string{"semantic", "member"}
	snapshot, err = db.InferenceCandidates(t.Context(), request)
	if err != nil || !snapshot.SemanticUnavailable || snapshot.IndexWaitUntil != nil {
		t.Fatalf("색인 실패 처리: %+v %v", snapshot, err)
	}
}

func TestInferenceCandidatesSemanticAndJudgedPair(t *testing.T) {
	db := newIsolatedIntegrationStore(t)
	graph, actor, member := lifeProposalFixture(t, db)
	oldInput := testEventContext(t, graph, actor, member.ID)
	oldInput.Body = "old event"
	old, err := db.CreateContext(t.Context(), graph, oldInput, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 의미 채널은 규칙 채널의 최근 1,000건 제한 밖에서도 후보를 찾는다.
	for offset := 0; offset < 1001; offset += 100 {
		var properties []string
		for range min(100, 1001-offset) {
			node := old
			node.ID = newTestID(t)
			encoded, err := encodeProperties(contextProperties(node))
			if err != nil {
				t.Fatal(err)
			}
			properties = append(properties, encoded)
		}
		query := "MATCH (member:Context) WHERE member.context_id = " + cypherString(member.ID.String()) +
			" WITH member UNWIND [" + strings.Join(properties, ",") + "] AS props CREATE (event:Context) SET event = props CREATE (event)-[:HAS_MEMBER {graph_id: " + cypherString(graph.String()) + "}]->(member) RETURN count(event)"
		var count string
		if err := db.pool.QueryRow(t.Context(), db.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
			t.Fatal(err)
		}
	}
	var newer int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+db.contextTable()+` WHERE properties ->> 'graph_id'::text=$1 AND properties ->> 'layer'::text='event' AND (properties ->> 'context_id'::text) COLLATE "C" > $2 COLLATE "C"`, graph.String(), old.ID.String()).Scan(&newer); err != nil || newer < 1001 {
		t.Fatalf("최근 집합 밖 표본 준비: %d %v", newer, err)
	}
	targetInput := testEventContext(t, graph, actor, member.ID)
	targetInput.Body = "target event"
	target, err := db.CreateContext(t.Context(), graph, targetInput, nil)
	if err != nil {
		t.Fatal(err)
	}
	lifeSeedProposalEmbedding(t, db, target, "test", 1)
	lifeSeedProposalEmbedding(t, db, old, "test", .9)
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM public.index_task WHERE context_id=$1`, target.ID.String()); err != nil {
		t.Fatal(err)
	}
	request := model.InferenceCandidateRequest{Task: model.RelationInferenceTask{GraphID: graph, ContextID: target.ID, TargetVersion: target.Version, Generation: time.Now().UTC()}, ProfileFingerprint: "synthetic", EmbeddingModel: "test", Channels: []string{"semantic"}, Limit: 10, IndexWait: time.Hour, AdjacencyWindow: time.Hour}
	result, err := db.InferenceCandidates(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.SemanticUnavailable || len(result.Channels) != 1 || len(result.Channels[0].Candidates) != 1 || result.Channels[0].Candidates[0].Event.ID != old.ID {
		t.Fatalf("의미 후보: %+v", result)
	}
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run := newTestID(t)
	if err := insertInferenceRun(t.Context(), tx, inferenceRunRecord{ID: run, GraphID: graph, ContextID: target.ID, TargetVersion: target.Version, ProfileFingerprint: request.ProfileFingerprint, ProfileVersions: "{}", Candidates: "[]", Result: "none", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := insertInferenceDecision(t.Context(), tx, inferenceDecisionRecord{ID: newTestID(t), RunID: run, CandidateID: old.ID, CandidateVersion: old.Version, RelationType: model.RelationTypeRelatesTo, Decision: "none", Evidence: "{}", ValidationResult: "valid"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err = db.InferenceCandidates(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Channels[0].Candidates) != 0 {
		t.Fatal("이미 판정한 쌍을 다시 반환했다")
	}
}
