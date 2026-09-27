package search

import (
	"testing"
	"time"

	"agent_context_sharing/internal/store"
)

func TestNewRequiresSnapshotStore(t *testing.T) {
	if _, err := New(&fakeStore{}, fakeEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: ConsistencySnapshot}, nil); err == nil {
		t.Fatal("스냅숏을 내보내지 못하는 저장소를 받아들였다")
	}
	if _, err := New(&fakeStore{}, fakeEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: "revision"}, nil); err == nil {
		t.Fatal("채택하지 않은 읽기 일관성 방식을 받아들였다")
	}
}

func TestFlowReadCommittedReportsNoSnapshot(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000411")
	value := testContext(t, graphID, "019a0000-0000-7000-8000-000000000412", "시간 후보", 1)
	flow, err := testService(t, &fakeStore{time: []store.SearchCandidate{{Context: value}}}, ExecutionParallel).Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100})
	if err != nil || flow.Read != (store.ReadStats{}) {
		t.Fatalf("Read Committed 검색 측정 = %+v, %v", flow.Read, err)
	}
}
