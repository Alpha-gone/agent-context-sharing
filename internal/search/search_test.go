package search

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
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
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "현재 작업", AsOf: time.Now().UTC(), Budget: 100})
	if !errors.Is(err, ErrAllChannelsFailed) {
		t.Fatalf("모든 채널 실패 오류 = %v", err)
	}
	if flow.Channels["graph"].Failure != "no_entry_points" {
		t.Fatalf("진입점 없는 그래프 채널 상태 = %#v", flow.Channels["graph"])
	}
}

type fakeStore struct {
	semantic    []store.SearchCandidate
	keyword     []store.SearchCandidate
	time        []store.SearchCandidate
	global      []store.SearchCandidate
	hops        store.HopResult
	hopCalls    int
	globalCalls int
	hopStarts   []model.ID
	hopFilters  []string
	// hopDepth에는 마지막 호출이 받은 탐색 깊이를 둔다.
	hopDepth int
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

func (fake *fakeStore) GlobalSummaryCandidates(context.Context, model.ID, time.Time, int) ([]store.SearchCandidate, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.globalCalls++
	return append([]store.SearchCandidate(nil), fake.global...), nil
}

func (fake *fakeStore) HopContextsFrom(_ context.Context, _ model.ID, starts []model.Context, hops int, _ string, filters []string, _ int) (store.HopResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.hopCalls++
	fake.hopDepth = hops
	fake.hopStarts = append(fake.hopStarts, contextIDs(starts)...)
	fake.hopFilters = append([]string(nil), filters...)
	return fake.hops, nil
}

func (fake *fakeStore) ContextOriginKinds(_ context.Context, _ model.ID, contextIDs []model.ID) (map[model.ID][]model.OriginKind, error) {
	origins := make(map[model.ID][]model.OriginKind, len(contextIDs))
	for _, contextID := range contextIDs {
		origins[contextID] = []model.OriginKind{model.OriginKindUserUtterance}
	}
	return origins, nil
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

func (failingStore) GlobalSummaryCandidates(context.Context, model.ID, time.Time, int) ([]store.SearchCandidate, error) {
	return nil, context.DeadlineExceeded
}

func (failingStore) HopContextsFrom(context.Context, model.ID, []model.Context, int, string, []string, int) (store.HopResult, error) {
	return store.HopResult{}, context.DeadlineExceeded
}

func (failingStore) ContextOriginKinds(context.Context, model.ID, []model.ID) (map[model.ID][]model.OriginKind, error) {
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

// TestApplyBudgetTreatsZeroAsNoLimit은 「계정 플랜」이 0을 한도 없음으로 선언한 것을
// 예산 적용이 그대로 지키는지 확인한다. 0을 거부하면 한도를 푸는 설정이 연산을 죽인다.
func TestApplyBudgetTreatsZeroAsNoLimit(t *testing.T) {
	candidates := []combinedCandidate{
		{value: model.Context{ID: newFoldID(t), Layer: model.LayerSource, Body: strings.Repeat("가", 5_000)}},
		{value: model.Context{ID: newFoldID(t), Layer: model.LayerSource, Body: strings.Repeat("나", 5_000)}},
	}
	selected, used, truncation := applyBudget(candidates, 0)
	if len(selected) != 2 || used != 10_000 || truncation != nil {
		t.Fatalf("무제한 예산 결과 = %d개, 사용 %d, 절단 %+v", len(selected), used, truncation)
	}
	if selected, _, truncation := applyBudget(candidates, 6_000); len(selected) != 1 || truncation == nil {
		t.Fatalf("유한 예산이 절단하지 않았다: %d개, 절단 %+v", len(selected), truncation)
	}
}

// TestFlowKeepsResultsWhenOriginTraceFails는 출처 추적 실패가 성공한 검색을 버리지 않는지
// 확인한다. 「정상 응답의 부분 상태」가 internal을 모든 채널 실패에만 쓰기로 확정했다.
func TestFlowKeepsResultsWhenOriginTraceFails(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000001")
	value := testContext(t, graphID, "019a0000-0000-7000-8000-000000000002", "본문", 1)
	backing := &fakeStore{time: []store.SearchCandidate{{Context: value}}}
	service, err := New(&originFailingStore{fakeStore: backing}, fakeEmbedder{},
		Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 10_000})
	if err != nil {
		t.Fatalf("출처 추적 실패가 검색을 중단시켰다: %v", err)
	}
	if len(flow.Contexts) == 0 {
		t.Fatal("출처 추적 실패 뒤 컨텍스트가 비었다")
	}
	if len(flow.Contexts[0].OriginKinds) != 0 {
		t.Fatalf("추적 실패인데 출처가 채워졌다: %v", flow.Contexts[0].OriginKinds)
	}
}

// TestFlowAddsGraphCandidatesAndKeepsOnlyIncludedEdges는 확장 노드가 결합에 들어가고
// 예산에 포함된 컨텍스트를 잇는 간선만 남는지 확인한다. 같은 홉에 이웃을 둘 이상 두어
// entry_distance가 목록 위치가 아니라 저장소가 준 홉 거리인지도 함께 고정한다.
func TestFlowAddsGraphCandidatesAndKeepsOnlyIncludedEdges(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000041")
	entry := testContext(t, graphID, "019a0000-0000-7000-8000-000000000042", "진입점", 1)
	near := testContext(t, graphID, "019a0000-0000-7000-8000-000000000043", "한 홉", 2)
	alsoNear := testContext(t, graphID, "019a0000-0000-7000-8000-000000000044", "같은 한 홉", 3)
	far := testContext(t, graphID, "019a0000-0000-7000-8000-000000000045", "두 홉", 4)
	dropped := testContext(t, graphID, "019a0000-0000-7000-8000-000000000046", "응답 밖", 5)
	database := &fakeStore{
		time: []store.SearchCandidate{{Context: entry}},
		hops: store.HopResult{
			Contexts:  []model.Context{entry, near, alsoNear, far},
			Distances: map[model.ID]int{entry.ID: 0, near.ID: 1, alsoNear.ID: 1, far.ID: 2},
			Edges: []store.HopEdge{
				{FromID: entry.ID, ToID: near.ID, Kind: "derived_from"},
				{FromID: entry.ID, ToID: alsoNear.ID, Kind: "derived_from"},
				{FromID: near.ID, ToID: far.ID, Kind: "supersedes"},
				{FromID: far.ID, ToID: dropped.ID, Kind: "derived_from"},
			},
		},
	}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageReferences}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100, MaxHops: 2, MaxHopNodes: 10})
	if err != nil {
		t.Fatalf("그래프 흐름: %v", err)
	}
	if len(flow.Contexts) != 4 {
		t.Fatalf("그래프 확장 결과 = %v", flowIDs(flow))
	}
	// 응답에 없는 dropped를 한쪽 끝으로 둔 간선은 빠지고 나머지 셋만 남는다.
	if len(flow.Edges) != 3 {
		t.Fatalf("응답에 남은 간선 = %+v", flow.Edges)
	}
	want := map[model.ID]int{entry.ID: 0, near.ID: 1, alsoNear.ID: 1, far.ID: 2}
	for _, item := range flow.Contexts {
		if item.EntryDistance != want[item.Value.ID] {
			t.Errorf("%q entry_distance = %d, want %d", item.Value.Body, item.EntryDistance, want[item.Value.ID])
		}
	}
	for _, item := range flow.Contexts {
		if item.Value.ID != entry.ID {
			continue
		}
		// 진입 채널이 찾은 노드는 그래프 확장이 아니라 진입 채널의 거리 0을 유지한다.
		if got := item.MatchedChannels; !reflect.DeepEqual(got, []string{"time"}) {
			t.Fatalf("진입점 채널 = %v", got)
		}
	}
}

// TestFlowFailsWhenGlobalScopeEntryChannelFails는 전역 범위의 진입 채널이 전역 요약
// 하나뿐이므로 그 채널의 조회 실패가 모든 채널 실패로 올라가는지 확인한다.
func TestFlowFailsWhenGlobalScopeEntryChannelFails(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000051")
	service, err := New(failingStore{}, failingEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageGlobal}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100, Scope: "global", MaxHops: 2, MaxHopNodes: 10})
	if !errors.Is(err, ErrAllChannelsFailed) {
		t.Fatalf("전역 범위 진입 채널 실패 = %v", err)
	}
	if flow.Channels["global_summary"].Failure != "timeout" {
		t.Fatalf("전역 요약 채널 상태 = %#v", flow.Channels["global_summary"])
	}
}

// TestFlowDisablesAutoGlobalFallback은 독립 구성으로 `auto`의 전역 전환을
// 끈 상태가 조회 실패와 다르게 다뤄지는지 확인한다. 국소 결과가 비어도
// 전역 요약을 시작점으로 더하지 않는다.
func TestFlowDisablesAutoGlobalFallback(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000061")
	summary := testContext(t, graphID, "019a0000-0000-7000-8000-000000000062", "전역 요약", 1)
	database := &fakeStore{global: []store.SearchCandidate{{Context: summary}}}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageBaseline}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100, MaxHops: 2, MaxHopNodes: 10})
	if err != nil {
		t.Fatalf("auto 범위 흐름 = %v", err)
	}
	if flow.Channels["global_summary"].Failure != "disabled" || len(flow.Contexts) != 0 {
		t.Fatalf("기준선의 auto 되돌림 = %#v, 컨텍스트 %d개", flow.Channels["global_summary"], len(flow.Contexts))
	}
	// 국소 그래프 단계도 기준선이므로 그래프 경로는 끈 상태로 남아야 한다.
	if flow.Channels["graph"].Failure != "no_entry_points" {
		t.Fatalf("기준선의 그래프 채널 = %#v", flow.Channels["graph"])
	}
}

// TestFlowKeepsExplicitGlobalScopeAtBaseline은 명시한 `scope=global`이 비교 단계와
// 무관하게 속성 필터로 전역 요약을 찾는지 확인한다. 진입점 선택은 클라이언트 계약이다.
func TestFlowKeepsExplicitGlobalScopeAtBaseline(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000071")
	summary := testContext(t, graphID, "019a0000-0000-7000-8000-000000000072", "전역 요약", 1)
	database := &fakeStore{global: []store.SearchCandidate{{Context: summary}}}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageBaseline}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100, Scope: "global", MaxHops: 2, MaxHopNodes: 10})
	if err != nil {
		t.Fatalf("전역 범위 흐름 = %v", err)
	}
	if len(flow.Contexts) != 1 || flow.Contexts[0].Value.ID != summary.ID {
		t.Fatalf("전역 범위 결과 = %v", flowIDs(flow))
	}
	if flow.Channels["global_summary"].Failure != "" {
		t.Fatalf("전역 요약 채널 상태 = %#v", flow.Channels["global_summary"])
	}
	// 기준선은 그래프 경로를 끈 구성이므로 근거를 따라 내려가지 않는다.
	if flow.Channels["graph"].Failure != "disabled" || database.hopCalls != 0 {
		t.Fatalf("기준선의 그래프 확장 = %#v, 호출 %d회", flow.Channels["graph"], database.hopCalls)
	}
}

// TestFlowAutoFallsBackDespiteTimeCandidates는 시간 후보가 있어도 관련 의미·키워드
// 후보가 없으면 전역 요약으로 전환하고, 전역 요약만 그래프 시작점으로 쓰는지 확인한다.
func TestFlowAutoFallsBackDespiteTimeCandidates(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000073")
	timed := testContext(t, graphID, "019a0000-0000-7000-8000-000000000074", "시간 후보", 1)
	summary := testContext(t, graphID, "019a0000-0000-7000-8000-000000000075", "전역 요약", 2)
	database := &fakeStore{time: []store.SearchCandidate{{Context: timed}}, global: []store.SearchCandidate{{Context: summary}}}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, SemanticThreshold: 0.7, FoldThreshold: 0.9, GraphStage: GraphStageBaseline, GlobalFallback: true}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	if _, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100, MaxHops: 2, MaxHopNodes: 10}); err != nil {
		t.Fatalf("auto 전역 되돌림: %v", err)
	}
	if database.globalCalls != 1 {
		t.Fatalf("전역 요약 조회 = %d회, want 1회", database.globalCalls)
	}
	if !slices.Equal(database.hopStarts, []model.ID{summary.ID}) {
		t.Fatalf("전역 전환의 그래프 시작점 = %v, want 전역 요약", database.hopStarts)
	}
	if !slices.Equal(database.hopFilters, []string{"derived_from"}) {
		t.Fatalf("전역 전환의 관계 필터 = %v, want derived_from", database.hopFilters)
	}
}

// TestFlowAutoFallsBackDespiteWeakSemanticCandidates는 의미 후보를 검색 결과에서는
// 보존하되 최상위 유사도가 하한보다 낮으면 전역 요약으로 전환하는지 확인한다.
func TestFlowAutoFallsBackDespiteWeakSemanticCandidates(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000078")
	weak := testContext(t, graphID, "019a0000-0000-7000-8000-000000000079", "약한 의미 후보", 1)
	summary := testContext(t, graphID, "019a0000-0000-7000-8000-00000000007a", "전역 요약", 2)
	database := &fakeStore{
		semantic: []store.SearchCandidate{{Context: weak, Similarity: 0.49}},
		global:   []store.SearchCandidate{{Context: summary}},
	}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, SemanticThreshold: 0.5, FoldThreshold: 0.9, GraphStage: GraphStageBaseline, GlobalFallback: true}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100, MaxHops: 2, MaxHopNodes: 10})
	if err != nil {
		t.Fatalf("auto 전역 되돌림: %v", err)
	}
	if database.globalCalls != 1 || !slices.Contains(flowIDs(flow), weak.ID.String()) {
		t.Fatalf("약한 의미 후보 보존 또는 전역 전환 실패: 호출 %d회, 결과 %v", database.globalCalls, flowIDs(flow))
	}
	if !slices.Equal(database.hopStarts, []model.ID{summary.ID}) {
		t.Fatalf("전역 전환의 그래프 시작점 = %v, want 전역 요약", database.hopStarts)
	}
}

// TestFlowAutoKeepsRelevantLocalCandidates는 키워드 후보가 있으면 global 단계에서도
// 전역 요약 되돌림을 실행하지 않는지 확인한다.
func TestFlowAutoKeepsRelevantLocalCandidates(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000076")
	local := testContext(t, graphID, "019a0000-0000-7000-8000-000000000077", "국소 후보", 1)
	database := &fakeStore{keyword: []store.SearchCandidate{{Context: local}}}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, SemanticThreshold: 0.7, FoldThreshold: 0.9, GraphStage: GraphStageBaseline, GlobalFallback: true}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	if _, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100, MaxHops: 2, MaxHopNodes: 10}); err != nil {
		t.Fatalf("auto 국소 검색: %v", err)
	}
	if database.globalCalls != 0 {
		t.Fatalf("관련 국소 후보가 있는데 전역 요약을 %d회 조회했다", database.globalCalls)
	}
}

func TestHasRelevantSemantic(t *testing.T) {
	candidates := []store.SearchCandidate{{Similarity: 0.49}, {Similarity: 0.5}}
	if !hasRelevantSemantic(candidates, 0.5) {
		t.Fatal("하한과 같은 의미 후보를 관련 후보로 판정하지 않았다")
	}
	if hasRelevantSemantic(candidates[:1], 0.5) {
		t.Fatal("하한보다 낮은 의미 후보를 관련 후보로 판정했다")
	}
}

// TestFlowExpandsAllEntryPointsInOneQuery는 진입점을 모아 한 번에 확장하고 홉 조회의
// 결과 상한이 잘랐을 때 그 경계를 흐름 응답까지 올리는지 확인한다.
func TestFlowExpandsAllEntryPointsInOneQuery(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000081")
	first := testContext(t, graphID, "019a0000-0000-7000-8000-000000000082", "진입점 하나", 1)
	second := testContext(t, graphID, "019a0000-0000-7000-8000-000000000083", "진입점 둘", 2)
	reached := testContext(t, graphID, "019a0000-0000-7000-8000-000000000084", "확장된 노드", 3)
	database := &fakeStore{
		keyword: []store.SearchCandidate{{Context: first}},
		time:    []store.SearchCandidate{{Context: second}},
		hops: store.HopResult{
			Contexts:  []model.Context{first, second, reached},
			Distances: map[model.ID]int{first.ID: 0, second.ID: 0, reached.ID: 1},
			Edges:     []store.HopEdge{{FromID: second.ID, ToID: reached.ID, Kind: "derived_from"}},
			Truncated: true,
			Boundary:  2,
		},
	}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageReferences}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 100, MaxHops: 2, MaxHopNodes: 3})
	if err != nil {
		t.Fatalf("그래프 흐름: %v", err)
	}
	// 진입점이 둘이어도 확장은 한 번이고 두 시작 노드를 함께 받는다.
	if database.hopCalls != 1 {
		t.Fatalf("확장 호출 = %d회, want 1회", database.hopCalls)
	}
	if got := database.hopStarts; len(got) != 2 || !slices.Contains(got, first.ID) || !slices.Contains(got, second.ID) {
		t.Fatalf("확장이 받은 시작 노드 = %v", got)
	}
	if flow.HopBoundary == nil || *flow.HopBoundary != 2 {
		t.Fatalf("잘린 홉 경계 = %v", flow.HopBoundary)
	}
	// 시작 노드는 진입 채널의 결과로 남고 확장분만 그래프 채널의 기여가 된다.
	if flow.Channels["graph"].Candidates != 1 || flow.Channels["graph"].Contribution != 1 {
		t.Fatalf("그래프 채널 측정 = %#v", flow.Channels["graph"])
	}
}

type originFailingStore struct{ *fakeStore }

func (originFailingStore) ContextOriginKinds(context.Context, model.ID, []model.ID) (map[model.ID][]model.OriginKind, error) {
	return nil, errors.New("출처 조회 실패")
}

// TestUnlimitedMaxHopsExpandsGraph는 MaxHops 0이 탐색을 막지 않는지 확인한다.
// 「계정 플랜」이 0을 한도 없음으로 선언했으므로 예산과 MaxHopNodes와 같게 해석해야
// 하며, 그대로 깊이로 넘기면 graph 채널이 실패 표시 없이 항상 0건이 된다.
func TestUnlimitedMaxHopsExpandsGraph(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000011")
	entry := testContext(t, graphID, "019a0000-0000-7000-8000-000000000012", "진입", 1)
	reached := testContext(t, graphID, "019a0000-0000-7000-8000-000000000013", "확장", 2)
	database := &fakeStore{
		keyword: []store.SearchCandidate{{Context: entry}},
		hops: store.HopResult{
			Contexts:  []model.Context{entry, reached},
			Distances: map[model.ID]int{entry.ID: 0, reached.ID: 1},
		},
	}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageRelations}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "현재 작업", AsOf: time.Now().UTC(), Budget: 0, MaxHops: 0, MaxHopNodes: 0})
	if err != nil {
		t.Fatalf("흐름 검색: %v", err)
	}
	if database.hopDepth <= 0 {
		t.Fatalf("탐색 깊이 = %d; MaxHops 0을 그대로 넘겼다", database.hopDepth)
	}
	if flow.Channels["graph"].Failure != "" {
		t.Fatalf("graph 채널 실패 = %q", flow.Channels["graph"].Failure)
	}
	if flow.Channels["graph"].Candidates != 1 {
		t.Fatalf("graph 채널 후보 = %d, want 1", flow.Channels["graph"].Candidates)
	}
}

// TestBoundedMaxHopsPassesDepthThrough는 0이 아닌 값은 그대로 깊이로 넘기는지 확인한다.
func TestBoundedMaxHopsPassesDepthThrough(t *testing.T) {
	graphID := testID(t, "019a0000-0000-7000-8000-000000000021")
	entry := testContext(t, graphID, "019a0000-0000-7000-8000-000000000022", "진입", 1)
	database := &fakeStore{
		keyword: []store.SearchCandidate{{Context: entry}},
		hops:    store.HopResult{Contexts: []model.Context{entry}, Distances: map[model.ID]int{entry.ID: 0}},
	}
	service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageRelations}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	if _, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "현재 작업", AsOf: time.Now().UTC(), Budget: 0, MaxHops: 3, MaxHopNodes: 0}); err != nil {
		t.Fatalf("흐름 검색: %v", err)
	}
	if database.hopDepth != 3 {
		t.Fatalf("탐색 깊이 = %d, want 3", database.hopDepth)
	}
}
