package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
)

func TestOBS07DiagnosticFileFailurePreservesCommitIntegration(t *testing.T) {
	database, _, _ := newAgentContractDatabase(t)
	graph, first, second := relationTestEvents(t, database)
	grantAccount(t, database, graph, first.CreatedBy, model.GraphGradeOwner)
	base, err := os.MkdirTemp(".", ".obs07-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	root, err := filepath.Abs(filepath.Join(base, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := diagnostic.NewSource("obs07_commit")
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := diagnostic.NewRecorder(t.Context(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close(context.Background())
	reporter, err := diagnostic.NewReporter(diagnostic.Management, recorder, source)
	if err != nil {
		t.Fatal(err)
	}
	database.SetOperationDiagnostics(reporter, nil)
	// 열린 fd가 있어도 경로/권한 검사가 실행 중 장애를 검출해야 한다.
	if err := os.Chmod(filepath.Join(root, source.InstanceID(), "management.jsonl"), 0o400); err != nil {
		t.Fatal(err)
	}
	relation := confirmable(t, graph, model.RelationTypePrecedes, first.ID, second.ID)
	operation := &OperationRecord{Kind: OperationAdd, GraphID: graph, RelationID: relation.ID, JudgmentInput: "audit-only judgment", AccountID: relation.ConfirmedBy, AgentID: relation.ConfirmedByAgent}
	stored, err := database.ConfirmRelation(obs05Context(t), graph, relation, operation)
	if err != nil {
		t.Fatal("진단 장애가 업무 성공을 바꿨다", err)
	}
	if err := recorder.Close(context.Background()); err == nil {
		t.Fatal("실제 파일 장애가 검출되지 않았다")
	}
	stats := recorder.Stats()
	if stats.Accepted != 1 || stats.Written != 0 || stats.Dropped != 1 {
		t.Fatal(stats)
	}
	relations, _, err := database.ListRelations(t.Context(), graph, "", 100)
	if err != nil || len(relations) != 1 || relations[0].ID != stored.ID {
		t.Fatal("진단 장애 뒤 관계가 보존되지 않았다", err)
	}
	history, err := database.OperationHistory(t.Context(), graph, first.CreatedBy, OperationTarget{RelationID: stored.ID})
	if err != nil || len(history) != 1 || history[0].Result != "applied" || history[0].JudgmentInput != "audit-only judgment" {
		t.Fatal("진단 장애 뒤 업무 감사가 보존되지 않았다", err)
	}
}
