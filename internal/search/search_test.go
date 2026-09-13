package search

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func TestFlowCombinesChannelsSameInParallelAndSequential(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000001")
	first := testContext(t, graphID, "019a0000-0000-7000-8000-000000000002", "첫 번째", 1)
	second := testContext(t, graphID, "019a0000-0000-7000-8000-000000000003", "두 번째", 2)
	third := testContext(t, graphID, "019a0000-0000-7000-8000-000000000004", "세 번째", 3)
	database := &fakeStore{
		semantic: []store.SearchCandidate{{Context: first}, {Context: second}},
		keyword:  []store.SearchCandidate{{Context: second}},
		time:     []store.SearchCandidate{{Context: third}},
	}
	input := Input{GraphID: graphID, WorkContext: "현재 작업", AsOf: time.Now().UTC(), Budget: 100}
	parallel := testService(t, database, ExecutionParallel)
	sequential := testService(t, database, ExecutionSequential)
	parallelFlow, err := parallel.Flow(t.Context(), input)
	if err != nil {
		t.Fatalf("병렬 검색: %v", err)
	}
	sequentialFlow, err := sequential.Flow(t.Context(), input)
	if err != nil {
		t.Fatalf("순차 검색: %v", err)
	}
	if got, want := flowIDs(parallelFlow), flowIDs(sequentialFlow); !reflect.DeepEqual(got, want) {
		t.Fatalf("병렬과 순차 결과가 다르다: %v != %v", got, want)
	}
	if got, want := flowIDs(parallelFlow), []string{second.ID.String(), third.ID.String(), first.ID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("결합 순서 = %v, want %v", got, want)
	}
	if channels := parallelFlow.Contexts[0].MatchedChannels; !reflect.DeepEqual(channels, []string{"keyword", "semantic"}) {
		t.Fatalf("중복 후보 기여 채널 = %v", channels)
	}
	if len(parallelFlow.Edges) != 0 {
		t.Fatalf("비그래프 기준선이 간선을 반환했다: %+v", parallelFlow.Edges)
	}
	if parallelFlow.Channels["keyword"].Contribution != 1 || parallelFlow.Channels["semantic"].Contribution != 2 || parallelFlow.Channels["time"].Contribution != 1 {
		t.Fatalf("채널 기여 = %#v", parallelFlow.Channels)
	}
}

func TestFlowKeepsOtherChannelsWhenEmbeddingFails(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000011")
	value := testContext(t, graphID, "019a0000-0000-7000-8000-000000000012", "시간 후보", 1)
	database := &fakeStore{time: []store.SearchCandidate{{Context: value}}}
	service, err := New(database, failingEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "현재 작업", AsOf: time.Now().UTC(), Budget: 100})
	if err != nil {
		t.Fatalf("부분 상태 검색: %v", err)
	}
	if flow.Channels["semantic"].Failure == "" || len(flow.Contexts) != 1 {
		t.Fatalf("의미 채널 실패의 부분 상태가 유지되지 않았다: %+v", flow)
	}
}

func TestApplyBudgetDoesNotFillWithLowerRankedShortContext(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000021")
	first := combinedCandidate{value: testContext(t, graphID, "019a0000-0000-7000-8000-000000000022", "가나다라마바사", 1)}
	second := combinedCandidate{value: testContext(t, graphID, "019a0000-0000-7000-8000-000000000023", "짧음", 2)}
	selected, used, truncated := applyBudget([]combinedCandidate{first, second}, 5)
	if len(selected) != 0 || used != 0 || truncated == nil || truncated.Excluded != 2 {
		t.Fatalf("예산 절단 = selected:%d used:%d truncated:%+v", len(selected), used, truncated)
	}
}

func TestFlowReturnsInternalErrorOnlyWhenAllChannelsFail(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000031")
	service, err := New(failingStore{}, failingEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	_, err = service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "현재 작업", AsOf: time.Now().UTC(), Budget: 100})
	if !errors.Is(err, ErrAllChannelsFailed) {
		t.Fatalf("모든 채널 실패 오류 = %v", err)
	}
}

type fakeStore struct {
	semantic []store.SearchCandidate
	keyword  []store.SearchCandidate
	time     []store.SearchCandidate
	mu       sync.Mutex
}

func (fake *fakeStore) KeywordCandidates(context.Context, model.ID, string, time.Time, int) ([]store.SearchCandidate, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]store.SearchCandidate(nil), fake.keyword...), nil
}

func (fake *fakeStore) TimeCandidates(context.Context, model.ID, time.Time, int) ([]store.SearchCandidate, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]store.SearchCandidate(nil), fake.time...), nil
}

func (fake *fakeStore) SemanticCandidates(context.Context, model.ID, string, []float64, time.Time, int) ([]store.SearchCandidate, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]store.SearchCandidate(nil), fake.semantic...), nil
}

func (fake *fakeStore) ContextOriginKinds(context.Context, model.ID, model.ID) ([]model.OriginKind, error) {
	return []model.OriginKind{model.OriginKindUserUtterance}, nil
}

type failingStore struct{}

func (failingStore) KeywordCandidates(context.Context, model.ID, string, time.Time, int) ([]store.SearchCandidate, error) {
	return nil, context.DeadlineExceeded
}

func (failingStore) TimeCandidates(context.Context, model.ID, time.Time, int) ([]store.SearchCandidate, error) {
	return nil, context.DeadlineExceeded
}

func (failingStore) SemanticCandidates(context.Context, model.ID, string, []float64, time.Time, int) ([]store.SearchCandidate, error) {
	return nil, context.DeadlineExceeded
}

func (failingStore) ContextOriginKinds(context.Context, model.ID, model.ID) ([]model.OriginKind, error) {
	return nil, nil
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(context.Context, string) ([]float64, error) { return []float64{1}, nil }
func (fakeEmbedder) ModelID() string                                  { return "test:vector:1" }

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, string) ([]float64, error) {
	return nil, context.DeadlineExceeded
}
func (failingEmbedder) ModelID() string { return "test:vector:1" }

func testService(t *testing.T, database Store, execution Execution) *Service {
	t.Helper()
	service, err := New(database, fakeEmbedder{}, Config{Execution: execution, CandidateLimit: 5, FoldThreshold: 0.9}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	return service
}

func testID(t *testing.T, value string) model.ID {
	t.Helper()
	id, err := model.ParseID(value)
	if err != nil {
		t.Fatalf("테스트 식별자 해석: %v", err)
	}
	return id
}

func testContext(t *testing.T, graphID model.ID, rawID, body string, seconds int) model.Context {
	t.Helper()
	return model.Context{ID: testID(t, rawID), GraphID: graphID, Layer: model.LayerSource, Body: body, RecordedAt: time.Date(2026, 1, 1, 0, 0, seconds, 0, time.UTC)}
}

func flowIDs(flow Flow) []string {
	ids := make([]string, 0, len(flow.Contexts))
	for _, value := range flow.Contexts {
		ids = append(ids, value.Value.ID.String())
	}
	return ids
}

// TestFoldSimilarDerivedGroupsByEvidenceAndSimilarity는 접기 범위를 확인한다.
// 근거가 다르면 같은 본문이라도 접지 않고, 근거가 같으면 임계값 이상일 때만 접는다.
func TestFoldSimilarDerivedGroupsByEvidenceAndSimilarity(t *testing.T) {
	evidence, other := newFoldID(t), newFoldID(t)
	derived := func(body string, referenceID model.ID) combinedCandidate {
		return combinedCandidate{value: model.Context{
			ID: newFoldID(t), Layer: model.LayerDerived, Body: body,
			Derived: &model.DerivedAttributes{
				Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateObservation,
				DerivedFrom: []model.ID{referenceID},
			},
		}}
	}

	t.Run("근거가 같고 본문이 거의 같으면 접는다", func(t *testing.T) {
		folded := foldSimilarDerived([]combinedCandidate{
			derived("배포 파이프라인이 실패한 원인은 캐시 키 충돌이다", evidence),
			derived("배포 파이프라인이 실패한 원인은 캐시 키 충돌이다.", evidence),
		}, 0.9)
		if len(folded) != 1 || folded[0].folded != 1 {
			t.Fatalf("접기 결과 = %d개, 접힌 수 %d", len(folded), folded[0].folded)
		}
	})

	t.Run("근거가 다르면 본문이 같아도 접지 않는다", func(t *testing.T) {
		body := "배포 파이프라인이 실패한 원인은 캐시 키 충돌이다"
		folded := foldSimilarDerived([]combinedCandidate{derived(body, evidence), derived(body, other)}, 0.9)
		if len(folded) != 2 {
			t.Fatalf("근거가 다른 파생이 접혔다: %d개", len(folded))
		}
	})

	t.Run("임계값에 못 미치면 접지 않는다", func(t *testing.T) {
		folded := foldSimilarDerived([]combinedCandidate{
			derived("배포 파이프라인이 실패한 원인은 캐시 키 충돌이다", evidence),
			derived("스테이징 데이터베이스 마이그레이션이 지연되고 있다", evidence),
		}, 0.9)
		if len(folded) != 2 {
			t.Fatalf("서로 다른 파생이 접혔다: %d개", len(folded))
		}
	})

	t.Run("원천과 사건은 접기 대상이 아니다", func(t *testing.T) {
		source := combinedCandidate{value: model.Context{ID: newFoldID(t), Layer: model.LayerSource, Body: "같은 본문"}}
		event := combinedCandidate{value: model.Context{ID: newFoldID(t), Layer: model.LayerEvent, Body: "같은 본문"}}
		if folded := foldSimilarDerived([]combinedCandidate{source, event}, 0.9); len(folded) != 2 {
			t.Fatalf("파생이 아닌 컨텍스트가 접혔다: %d개", len(folded))
		}
	})
}

func newFoldID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("식별자 생성: %v", err)
	}
	return id
}

// TestChannelFailureReasonSeparatesUpstreamFromStore는 실패 사유가 상류 장애와 저장소 장애를
// 구분하는지 확인한다. 「측정」이 채널별 실패 사유로 둘을 나눠 재기 때문이다.
func TestChannelFailureReasonSeparatesUpstreamFromStore(t *testing.T) {
	tests := map[string]struct {
		err    error
		reason string
	}{
		"질의 임베딩 실패": {err: fmt.Errorf("%w: %w", ErrEmbeddingUnavailable, errors.New("제공자 응답 500")), reason: "embedding_unavailable"},
		"저장소 실패":    {err: errors.New("키워드 검색: 연결 없음"), reason: "store_unavailable"},
		"시간 초과":     {err: fmt.Errorf("의미 유사도 검색: %w", context.DeadlineExceeded), reason: "timeout"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if reason := failureReason(test.err); reason != test.reason {
				t.Fatalf("실패 사유 = %q, want %q", reason, test.reason)
			}
		})
	}
}
