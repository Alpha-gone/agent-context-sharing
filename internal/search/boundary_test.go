package search

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type snapshotMarker struct{}

type snapshotBoundaryStore struct {
	Store
	opened, closed int
	readContext    context.Context
}

func (database *snapshotBoundaryStore) WithReadSnapshot(ctx context.Context, read func(context.Context) error) (model.ReadStats, error) {
	database.opened++
	defer func() { database.closed++ }()
	err := read(context.WithValue(ctx, snapshotMarker{}, true))
	return model.ReadStats{Connections: 4, PeakConnections: 4}, err
}

func (database *snapshotBoundaryStore) KeywordCandidates(ctx context.Context, graph model.ID, query string, at time.Time, limit int) ([]model.SearchCandidate, error) {
	database.readContext = ctx
	return database.Store.KeywordCandidates(ctx, graph, query, at, limit)
}

type droppingDiagnostics struct {
	calls                        int
	level, stage, result, reason string
}

func (diagnostics *droppingDiagnostics) RecordDiagnostic(_ context.Context, level, stage, result, reason string) bool {
	diagnostics.calls++
	diagnostics.level, diagnostics.stage, diagnostics.result, diagnostics.reason = level, stage, result, reason
	return false
}

func TestInjectedSnapshotBoundaryPreservesResultAndError(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000411")
	value := testContext(t, graph, "019a0000-0000-7000-8000-000000000412", "후보", 1)
	for _, sample := range []struct {
		name   string
		source Store
		failed bool
	}{
		{"success", &fakeStore{time: []model.SearchCandidate{{Context: value}}}, false},
		{"failure", failingStore{}, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			database := &snapshotBoundaryStore{Store: sample.source}
			service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: ConsistencySnapshot}, nil)
			if err != nil {
				t.Fatal(err)
			}
			diagnostics := &droppingDiagnostics{}
			service.SetDiagnostics(diagnostics)
			flow, err := service.Flow(t.Context(), Input{GraphID: graph, WorkContext: "질의 원문", Budget: 100})
			if sample.failed != errors.Is(err, ErrAllChannelsFailed) || database.opened != 1 || database.closed != 1 || database.readContext.Value(snapshotMarker{}) != true {
				t.Fatalf("스냅숏 경계: opened=%d closed=%d error=%v", database.opened, database.closed, err)
			}
			if flow.Read.PeakConnections != 4 || diagnostics.calls != 1 || diagnostics.stage != "flow" {
				t.Fatalf("자원·진단 집계: %+v %+v", flow.Read, diagnostics)
			}
			if !sample.failed && (err != nil || len(flow.Contexts) != 1 || flow.Contexts[0].Value.ID != value.ID || diagnostics.result != "completed") {
				t.Fatalf("진단 누락이 성공 결과를 바꿨다: %+v %v", flow, err)
			}
			if sample.failed && diagnostics.reason != "search_failed" {
				t.Fatal("오류 원문을 전달했다")
			}
		})
	}
}
