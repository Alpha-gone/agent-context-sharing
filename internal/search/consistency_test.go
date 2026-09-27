package search

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// revisionStore는 ContentRevision이 불릴 때마다 revisions의 다음 값을 돌려준다.
type revisionStore struct {
	*fakeStore
	revisions []int64
	calls     int
}

func (revision *revisionStore) BeginReadSnapshot(context.Context) (*store.ReadSnapshot, error) {
	return nil, errors.New("단위 테스트는 스냅숏을 쓰지 않는다")
}

func (revision *revisionStore) ContentRevision(context.Context, model.ID) (int64, error) {
	value := revision.revisions[min(revision.calls, len(revision.revisions)-1)]
	revision.calls++
	return value, nil
}

// countingEmbedder는 임베딩 호출 수를 센다.
type countingEmbedder struct{ calls atomic.Int32 }

func (embedder *countingEmbedder) Embed(context.Context, string) ([]float64, error) {
	embedder.calls.Add(1)
	return []float64{1}, nil
}

func (*countingEmbedder) ModelID() string { return "test:vector:1" }

func revisionFlow(t *testing.T, revisions ...int64) (Flow, *revisionStore, *countingEmbedder, error) {
	t.Helper()
	graphID := testID(t, "019a0000-0000-7000-8000-000000000401")
	value := testContext(t, graphID, "019a0000-0000-7000-8000-000000000402", "시간 후보", 1)
	database := &revisionStore{fakeStore: &fakeStore{time: []store.SearchCandidate{{Context: value}}}, revisions: revisions}
	embedder := &countingEmbedder{}
	service, err := New(database, embedder, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: ConsistencyRevision}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100})
	return flow, database, embedder, err
}

func TestFlowRevisionRetriesUntilRevisionIsStable(t *testing.T) {
	// 첫 시도는 전후 판이 1→2로 달라 버리고, 두 번째 시도는 2→2라 받아들인다.
	flow, database, embedder, err := revisionFlow(t, 1, 2, 2, 2)
	if err != nil {
		t.Fatalf("내용 판 재시도: %v", err)
	}
	if flow.Read.Attempts != 2 || database.calls != 4 || len(flow.Contexts) != 1 {
		t.Fatalf("재시도 = %d회, 판 조회 %d회, 결과 %d개", flow.Read.Attempts, database.calls, len(flow.Contexts))
	}
	// 외부 호출은 재시도마다 반복하지 않는다.
	if embedder.calls.Load() != 1 {
		t.Fatalf("임베딩 호출 = %d, want 1", embedder.calls.Load())
	}
}

func TestFlowRevisionStopsAtAttemptLimit(t *testing.T) {
	revisions := make([]int64, 0, 2*maxReadAttempts)
	for index := range 2 * maxReadAttempts {
		revisions = append(revisions, int64(index))
	}
	_, database, _, err := revisionFlow(t, revisions...)
	if !errors.Is(err, ErrReadNotSettled) || database.calls != 2*maxReadAttempts {
		t.Fatalf("상한 도달 = %v, 판 조회 %d회", err, database.calls)
	}
}

func TestNewRequiresConsistentStoreForCandidates(t *testing.T) {
	for _, consistency := range []Consistency{ConsistencySnapshot, ConsistencyRevision} {
		if _, err := New(&fakeStore{}, fakeEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: consistency}, nil); err == nil {
			t.Fatalf("%s 후보가 스냅숏·내용 판 없는 저장소를 받아들였다", consistency)
		}
	}
	if _, err := New(&fakeStore{}, fakeEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: "other"}, nil); err == nil {
		t.Fatal("알 수 없는 읽기 일관성 방식을 받아들였다")
	}
}

func TestFlowReadCommittedReportsSingleAttempt(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000411")
	value := testContext(t, graphID, "019a0000-0000-7000-8000-000000000412", "시간 후보", 1)
	flow, err := testService(t, &fakeStore{time: []store.SearchCandidate{{Context: value}}}, ExecutionParallel).Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100})
	if err != nil || flow.Read.Attempts != 1 || flow.Read.Connections != 0 {
		t.Fatalf("현행 검색 측정 = %+v, %v", flow.Read, err)
	}
}
