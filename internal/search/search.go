// Package search는 비그래프 검색 채널 실행, 결합과 컨텍스트 예산 적용을 소유한다.
package search

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// Execution은 서로 독립인 진입 채널을 실행하는 방식이다.
type Execution string

const (
	// ExecutionParallel은 세 진입 채널을 동시에 실행한다.
	ExecutionParallel Execution = "parallel"
	// ExecutionSequential은 결과 재현을 위해 고정 순서로 실행한다.
	ExecutionSequential Execution = "sequential"
)

// GraphStage는 그래프 효과 비교에서 누적해 활성화할 범위다.
type GraphStage string

const (
	GraphStageBaseline   GraphStage = "baseline"
	GraphStageReferences GraphStage = "references"
	GraphStageRelations  GraphStage = "relations"
	GraphStageGlobal     GraphStage = "global"
)

// Config는 검색 결합의 배포 구성을 담는다.
type Config struct {
	Execution         Execution
	CandidateLimit    int
	SemanticThreshold float64
	FoldThreshold     float64
	GraphStage        GraphStage
	GlobalFallback    bool
	// EvidencePathSelection 필드에는 근거 경로 보존 선택 활성 여부를 둔다. 끄면 기존 통합
	// 순위 절단을 수행한다.
	EvidencePathSelection bool
	// AdaptiveRouting 필드에는 scope=auto의 질의 적응형 라우팅 활성 여부를 둔다. 끄면 기존
	// auto 전환 규칙을 수행한다.
	AdaptiveRouting bool
	// AdaptiveDirectThreshold와 AdaptiveMarginThreshold 필드에는 직접 경로를 고르는 의미
	// 후보 1위의 유사도 하한과 2위와의 분리 폭을 둔다.
	AdaptiveDirectThreshold float64
	AdaptiveMarginThreshold float64
	// Route 필드는 평가 실행기만 쓴다. 적응형 라우팅이 켜진 scope=auto에서 라우터 대신 이
	// 경로를 실행해 세 경로의 결과를 같은 입력으로 견준다. 비어 있으면 라우터가 고른다.
	Route Route
	// Consistency 필드에는 한 흐름 응답의 읽기 일관성 방식을 둔다. 「요청 단위 일관 읽기 구현
	// 비교」가 동기화 스냅숏을 운영 방식으로 채택해 서버는 스냅숏을 쓴다. 비어 있으면 비교
	// 기준인 Read Committed 검색이고, 내용 판 재시도는 평가 실행기의 비교에서만 쓴다. 배포
	// 구성으로 열지 않는다.
	Consistency Consistency
}

// Consistency는 한 흐름 응답을 구성하는 읽기의 일관성 방식이다.
type Consistency string

const (
	// ConsistencySnapshot은 조정 연결이 내보낸 스냅숏을 모든 읽기가 가져오는 방식이다.
	ConsistencySnapshot Consistency = "snapshot"
	// ConsistencyRevision은 요청 전후 내용 판이 다르면 흐름 전체를 다시 실행하는 방식이다.
	ConsistencyRevision Consistency = "revision"
)

// maxReadAttempts는 내용 판 재시도의 상한이다. 쓰기가 계속 겹치면 무한히 돌지 않고
// ErrReadNotSettled로 끝내며, 평가 실행기는 이를 지연 상한 위반으로 센다.
const maxReadAttempts = 5

// ErrReadNotSettled는 내용 판 재시도가 상한 안에 한 판의 결과를 얻지 못했음을 나타낸다.
var ErrReadNotSettled = errors.New("요청 동안 그래프 내용 판이 계속 바뀌었다")

// ConsistentStore는 일관 읽기 후보가 추가로 쓰는 저장소 기능이다. 현행 검색은 쓰지 않으므로
// Store와 나눠 두고 후보를 고른 구성에서만 요구한다.
type ConsistentStore interface {
	BeginReadSnapshot(context.Context) (*store.ReadSnapshot, error)
	ContentRevision(context.Context, model.ID) (int64, error)
}

// ReadStats는 한 흐름이 읽기 경계에서 쓴 자원과 시도 횟수다.
type ReadStats struct {
	// Attempts 필드에는 흐름을 실행한 횟수를 둔다. 내용 판 재시도가 아니면 1이다.
	Attempts int
	store.ReadStats
}

// Embedder는 질의 텍스트를 현재 색인 모델의 벡터로 바꾸는 index 경계다.
type Embedder interface {
	Embed(context.Context, string) ([]float64, error)
	ModelID() string
}

// Store는 search가 필요한 데이터 접근 기능만 노출한다.
type Store interface {
	KeywordCandidates(context.Context, model.ID, string, time.Time, int) ([]store.SearchCandidate, error)
	TimeCandidates(context.Context, model.ID, time.Time, int) ([]store.SearchCandidate, error)
	SemanticCandidates(context.Context, model.ID, string, []float64, time.Time, int) ([]store.SearchCandidate, error)
	GlobalSummaryCandidates(context.Context, model.ID, time.Time, int) ([]store.SearchCandidate, error)
	HopContextsFrom(context.Context, model.ID, []model.Context, int, string, []string, int) (store.HopResult, error)
	ContextOriginKinds(context.Context, model.ID, []model.ID) (map[model.ID][]model.OriginKind, error)
}

// Service는 비그래프 기준선의 세 진입 채널을 실행한다.
type Service struct {
	store      Store
	consistent ConsistentStore
	embedder   Embedder
	config     Config
	logger     *slog.Logger
}

// Input은 context_flow_get이 내부 검색기에 넘기는 확정된 입력이다.
type Input struct {
	GraphID     model.ID
	WorkContext string
	AsOf        time.Time
	Budget      int
	Scope       string
	MaxHops     int
	MaxHopNodes int
}

// Context는 흐름 응답을 만들기 위해 컨텍스트에 검색 메타데이터를 붙인 값이다.
type Context struct {
	Value           model.Context
	OriginKinds     []model.OriginKind
	MatchedChannels []string
	Rank            int
	FoldedCount     int
	EntryDistance   int
	// CandidateDegree 필드에는 후보 부분 그래프에서의 차수를 둔다. 응답에는 싣지 않으며
	// 「검색 품질 평가」의 허브 편중을 계산하는 원자료다.
	CandidateDegree int
}

// Channel은 채널별 후보 수·지연·실패와 응답 기여를 담는다.
type Channel struct {
	Candidates   int
	Latency      time.Duration
	Contribution int
	Failure      string
}

// Truncation은 예산으로 제외된 항목의 수를 나타낸다.
type Truncation struct {
	Reason   string
	Excluded int
}

// Flow는 MCP가 그대로 직렬화할 수 있는 비그래프 검색 결과다.
type Flow struct {
	Contexts    []Context
	Edges       []store.HopEdge
	EntryPoints []model.ID
	BudgetUsed  int
	Budget      int
	Channels    map[string]Channel
	Truncation  *Truncation
	// HopBoundary 필드에는 그래프 확장이 결과 상한으로 잘린 홉 경계를 둔다. 예산 절단과
	// 다른 원인이므로 따로 싣고, 경계 0과 절단 없음을 구분하려고 포인터로 둔다.
	HopBoundary *int
	Selection   Selection
	// Route와 RouteReason 필드에는 실제 실행한 검색 경로와 그 이유 코드를 둔다.
	Route       Route
	RouteReason string
	// RouteSignals 필드에는 적응형 라우팅이 판정에 쓴 진입 채널 요약을 둔다. 라우터를
	// 실행하지 않았으면 0 값이다.
	RouteSignals RouteSignals
	// Read 필드에는 요청 단위 일관 읽기 비교의 측정값을 둔다.
	Read ReadStats
}

// ErrAllChannelsFailed는 부분 상태로 복구할 채널도 남지 않았음을 나타낸다.
var ErrAllChannelsFailed = errors.New("모든 검색 채널이 실패했다")

// New는 외부 임베딩 호출 소유자를 주입받아 검색 실행기를 만든다.
func New(database Store, embedder Embedder, config Config, logger *slog.Logger) (*Service, error) {
	if database == nil || embedder == nil {
		return nil, fmt.Errorf("검색 저장소와 임베딩 제공자가 필요하다")
	}
	if config.Execution != ExecutionParallel && config.Execution != ExecutionSequential {
		return nil, fmt.Errorf("검색 채널 실행 방식이 올바르지 않다")
	}
	if config.CandidateLimit < 1 {
		return nil, fmt.Errorf("검색 채널 후보 수 상한은 양수여야 한다")
	}
	if config.SemanticThreshold < 0 || config.SemanticThreshold > 1 {
		return nil, fmt.Errorf("의미 유사도 하한은 0 이상 1 이하여야 한다")
	}
	if config.FoldThreshold <= 0 || config.FoldThreshold > 1 {
		return nil, fmt.Errorf("중복 파생 접기 임계값은 0 초과 1 이하여야 한다")
	}
	if config.GraphStage == "" {
		config.GraphStage = GraphStageBaseline
	}
	if !slices.Contains([]GraphStage{GraphStageBaseline, GraphStageReferences, GraphStageRelations, GraphStageGlobal}, config.GraphStage) {
		return nil, fmt.Errorf("그래프 검색 비교 단계가 올바르지 않다")
	}
	if config.AdaptiveRouting {
		// 국소 확장이 확장할 관계가 없으면 직접 경로와 같아져 비교가 성립하지 않는다.
		if config.GraphStage == GraphStageBaseline {
			return nil, fmt.Errorf("질의 적응형 라우팅은 그래프 검색 비교 단계 baseline과 함께 쓸 수 없다")
		}
		if config.AdaptiveDirectThreshold < 0 || config.AdaptiveDirectThreshold > 1 || config.AdaptiveMarginThreshold < 0 || config.AdaptiveMarginThreshold > 1 {
			return nil, fmt.Errorf("직접 충분성 하한과 분리 폭은 0 이상 1 이하여야 한다")
		}
	}
	if !slices.Contains([]Route{"", RouteDirect, RouteLocal, RouteGlobal}, config.Route) || (config.Route != "" && !config.AdaptiveRouting) {
		return nil, fmt.Errorf("강제 검색 경로는 질의 적응형 라우팅과 함께 direct, local, global 중 하나여야 한다")
	}
	var consistent ConsistentStore
	switch config.Consistency {
	case "":
	case ConsistencySnapshot, ConsistencyRevision:
		var ok bool
		if consistent, ok = database.(ConsistentStore); !ok {
			return nil, fmt.Errorf("일관 읽기 비교에는 스냅숏과 내용 판을 제공하는 저장소가 필요하다")
		}
	default:
		return nil, fmt.Errorf("읽기 일관성 방식이 올바르지 않다")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: database, consistent: consistent, embedder: embedder, config: config, logger: logger}, nil
}

// Flow는 진입 채널을 결합하고 구성에 따라 그래프 경로와 전역 요약을 추가한다.
func (service *Service) Flow(ctx context.Context, input Input) (Flow, error) {
	// Budget 0은 「계정 플랜」이 선언한 대로 한도 없음이다.
	if !input.GraphID.IsV7() || input.WorkContext == "" || input.Budget < 0 || input.MaxHops < 0 || input.MaxHopNodes < 0 {
		return Flow{}, fmt.Errorf("검색 입력이 올바르지 않다")
	}
	if input.Scope == "" {
		input.Scope = "auto"
	}
	if !slices.Contains([]string{"auto", "local", "global"}, input.Scope) {
		return Flow{}, fmt.Errorf("검색 범위가 올바르지 않다")
	}
	if input.AsOf.IsZero() {
		input.AsOf = time.Now().UTC()
	}
	if !input.AsOf.UTC().Equal(input.AsOf) {
		return Flow{}, fmt.Errorf("검색 기준 시각은 UTC여야 한다")
	}
	if service.config.Consistency == "" {
		flow, err := service.flow(ctx, input, func() ([]float64, error) { return service.embedder.Embed(ctx, input.WorkContext) })
		flow.Read.Attempts = 1
		return flow, err
	}
	// 질의 임베딩은 외부 호출이므로 읽기 경계를 열기 전에 끝낸다. 경계 안에서 기다리면
	// 스냅숏과 연결을 제공자 지연만큼 붙잡는다. 전역 범위는 의미 채널을 돌리지 않는다.
	var embedding []float64
	var embedErr error
	if input.Scope != "global" {
		embedding, embedErr = service.embedder.Embed(ctx, input.WorkContext)
	}
	embed := func() ([]float64, error) { return embedding, embedErr }
	if service.config.Consistency == ConsistencySnapshot {
		snapshot, err := service.consistent.BeginReadSnapshot(ctx)
		if err != nil {
			return Flow{}, err
		}
		flow, err := service.flow(snapshot.Context(ctx), input, embed)
		flow.Read = ReadStats{Attempts: 1, ReadStats: snapshot.Close(ctx)}
		return flow, err
	}
	for attempt := 1; attempt <= maxReadAttempts; attempt++ {
		before, err := service.consistent.ContentRevision(ctx, input.GraphID)
		if err != nil {
			return Flow{}, err
		}
		flow, err := service.flow(ctx, input, embed)
		if err != nil {
			return flow, err
		}
		after, err := service.consistent.ContentRevision(ctx, input.GraphID)
		if err != nil {
			return Flow{}, err
		}
		if before == after {
			flow.Read.Attempts = attempt
			return flow, nil
		}
	}
	return Flow{}, ErrReadNotSettled
}

// flow는 한 번의 검색 흐름이다. embed는 의미 채널이 쓸 질의 벡터를 돌려준다.
func (service *Service) flow(ctx context.Context, input Input, embed func() ([]float64, error)) (Flow, error) {
	current := time.Now().UTC()
	channels := map[string]Channel{}
	results := make([]channelResult, 3)
	run := func(index int, name string, execute func() ([]store.SearchCandidate, error)) {
		started := time.Now()
		candidates, err := execute()
		metric := Channel{Latency: time.Since(started)}
		if err != nil {
			// 응답에는 분류만 싣고 원인은 로그에 남긴다. 「오류의 생성과 전달」이 내부 원인을
			// 응답에 넣지 못하게 했고, 「측정」은 실패 채널의 사유를 남기라고 요구한다.
			metric.Failure = failureReason(err)
			service.logger.ErrorContext(ctx, "검색 채널 실패", "channel", name, "reason", metric.Failure, "error", err.Error())
		} else {
			metric.Candidates = len(candidates)
		}
		results[index] = channelResult{name: name, candidates: candidates, metric: metric}
	}
	semantic := func() ([]store.SearchCandidate, error) {
		embedding, err := embed()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrEmbeddingUnavailable, err)
		}
		return service.store.SemanticCandidates(ctx, input.GraphID, service.embedder.ModelID(), embedding, current, service.config.CandidateLimit)
	}
	keyword := func() ([]store.SearchCandidate, error) {
		return service.store.KeywordCandidates(ctx, input.GraphID, input.WorkContext, current, service.config.CandidateLimit)
	}
	timeFilter := func() ([]store.SearchCandidate, error) {
		return service.store.TimeCandidates(ctx, input.GraphID, input.AsOf, service.config.CandidateLimit)
	}
	if input.Scope != "global" && service.config.Execution == ExecutionParallel {
		var group sync.WaitGroup
		group.Go(func() { run(0, "semantic", semantic) })
		group.Go(func() { run(1, "keyword", keyword) })
		group.Go(func() { run(2, "time", timeFilter) })
		group.Wait()
	} else if input.Scope != "global" {
		run(0, "semantic", semantic)
		run(1, "keyword", keyword)
		run(2, "time", timeFilter)
	}
	for _, result := range results {
		if result.name != "" {
			channels[result.name] = result.metric
		}
	}
	// 진입점이 없으면 확장할 것도 없으므로 그래프 채널의 상태까지 남기고 끝낸다.
	allEntryChannelsFailed := func() (Flow, error) {
		graph := service.graphCandidates(ctx, input, nil, false, 0)
		channels[graph.name] = graph.metric
		return Flow{Channels: channels}, ErrAllChannelsFailed
	}
	if input.Scope != "global" && channels["semantic"].Failure != "" && channels["keyword"].Failure != "" && channels["time"].Failure != "" {
		return allEntryChannelsFailed()
	}
	combined := combine(results)
	var entryPoints []model.Context
	var graph channelResult
	var route Route
	var reason string
	var signals RouteSignals
	globalFallbackTriggered, globalFallbackApplied := false, false
	if input.Scope == "auto" && service.config.AdaptiveRouting {
		signals = routeSignals(results, combined, service.config.SemanticThreshold)
		route, reason = signals.Decide(service.config.AdaptiveDirectThreshold, service.config.AdaptiveMarginThreshold)
		if service.config.Route != "" {
			route, reason = service.config.Route, ReasonForced
		}
		var global channelResult
		if route == RouteGlobal {
			global = service.globalSummaries(ctx, input)
			channels[global.name] = global.metric
			// 요약이 없거나 조회가 실패하면 직접 경로로 되돌린다. 조회 실패는 채널 실패로 남는다.
			if len(global.candidates) == 0 {
				route, reason = RouteDirect, ReasonGlobalSummaryAbsent
			} else {
				results = append(results, global)
			}
		}
		switch route {
		case RouteDirect:
			entryPoints = candidateContexts(combined)
			// 직접 경로는 확장 채널을 실행하지 않는다. 조회 실패가 아니므로 구성으로 끈 상태와
			// 같이 표시해 모든 채널 실패 판정과 실패 집계에서 뺀다.
			graph = channelResult{name: "graph", metric: Channel{Failure: failureDisabled}}
		case RouteLocal:
			entryPoints = relevantLocalContexts(results, combined, service.config.SemanticThreshold)
			graph = service.graphCandidates(ctx, input, entryPoints, false, 1)
		case RouteGlobal:
			entryPoints = candidateContexts(combine([]channelResult{global}))
			graph = service.graphCandidates(ctx, input, entryPoints, true, hopDepth(input.MaxHops))
		}
	} else {
		relevantLocal := hasRelevantSemantic(results[0].candidates, service.config.SemanticThreshold) || len(results[1].candidates) > 0
		globalFallbackTriggered = input.Scope == "auto" && !relevantLocal
		var global channelResult
		if input.Scope == "global" || globalFallbackTriggered {
			global = service.globalSummaries(ctx, input)
			channels[global.name] = global.metric
			// 전역 범위의 진입 채널은 전역 요약 하나뿐이므로 그 채널의 실패가 곧 모든 채널
			// 실패다. 비교 단계에서 끈 상태는 조회 실패가 아니라 구성이므로 제외한다.
			if input.Scope == "global" && global.metric.Failure != "" && global.metric.Failure != failureDisabled {
				return allEntryChannelsFailed()
			}
			results = append(results, global)
			combined = combine(results)
		}
		globalFallbackApplied = globalFallbackTriggered && service.config.GlobalFallback && len(global.candidates) > 0
		entryPoints = candidateContexts(combined)
		// auto가 전역 요약으로 전환됐으면 광범위한 시간 후보가 홉 결과 상한을 먼저
		// 채우지 않게 실제 전역 요약이 있을 때 그 요약만 확장 시작점으로 쓴다. 전역 요약이
		// 없거나 비교 단계가 되돌림을 껐으면 기존 국소 후보를 그대로 쓴다.
		if globalFallbackApplied {
			entryPoints = candidateContexts(combine([]channelResult{global}))
		}
		graph = service.graphCandidates(ctx, input, entryPoints, globalFallbackApplied, hopDepth(input.MaxHops))
		route, reason = legacyRoute(input.Scope, service.config.GraphStage, globalFallbackApplied, graph)
	}
	channels[graph.name] = graph.metric
	results = append(results, graph)
	combined = combine(results)
	combined = foldSimilarDerived(combined, service.config.FoldThreshold)
	graphView := buildCandidateSubgraph(combined, results)
	var selected []combinedCandidate
	var used int
	var truncation *Truncation
	var selection Selection
	if service.config.EvidencePathSelection {
		selected, used, truncation, selection = selectEvidenceSets(combined, graphView, input.Budget)
	} else {
		selected, used, truncation = applyBudget(combined, input.Budget)
		selection = Selection{Candidates: len(combined), Selected: len(selected)}
	}
	// 연결 근거 집합은 경로 노드를 대표 후보 뒤에 붙이므로 응답 정렬인 통합 순위로 되돌린다.
	// 순위는 선택 전 통합 순위를 그대로 쓴다. 비활성 구성은 앞부분만 남기므로 위치와 같다.
	ranks := make(map[model.ID]int, len(combined))
	for index, candidate := range combined {
		ranks[candidate.value.ID] = index + 1
	}
	slices.SortFunc(selected, func(left, right combinedCandidate) int {
		return cmp.Compare(ranks[left.value.ID], ranks[right.value.ID])
	})
	ids := make([]model.ID, 0, len(selected))
	for _, candidate := range selected {
		ids = append(ids, candidate.value.ID)
	}
	// 출처 추적은 응답의 부가 메타데이터다. 실패해도 채널이 낸 결과를 버리지 않고 빈 값으로
	// 두며, 「정상 응답의 부분 상태」가 internal을 모든 채널 실패에만 쓰기로 확정했다.
	origins, err := service.store.ContextOriginKinds(ctx, input.GraphID, ids)
	if err != nil {
		service.logger.ErrorContext(ctx, "컨텍스트 출처 계산 실패", "graph_id", input.GraphID.String(), "error", err.Error())
		origins = map[model.ID][]model.OriginKind{}
	}
	contexts := make([]Context, 0, len(selected))
	for _, candidate := range selected {
		contexts = append(contexts, Context{Value: candidate.value, OriginKinds: origins[candidate.value.ID], MatchedChannels: candidate.channels, Rank: ranks[candidate.value.ID], FoldedCount: candidate.folded, EntryDistance: candidate.distance, CandidateDegree: graphView.degrees[candidate.value.ID]})
		for _, channel := range candidate.channels {
			metric := channels[channel]
			metric.Contribution++
			channels[channel] = metric
		}
	}
	edges := filterEdges(selected, results)
	flow := Flow{Contexts: contexts, Edges: edges, EntryPoints: contextIDs(entryPoints), BudgetUsed: used, Budget: input.Budget, Channels: channels, Truncation: truncation, HopBoundary: graph.boundary, Selection: selection, Route: route, RouteReason: reason, RouteSignals: signals}
	service.logger.InfoContext(ctx, "작업 컨텍스트 흐름 검색 완료", "semantic_candidates", channels["semantic"].Candidates, "keyword_candidates", channels["keyword"].Candidates, "time_candidates", channels["time"].Candidates, "global_fallback_triggered", globalFallbackTriggered, "global_fallback_applied", globalFallbackApplied, "selected_route", route, "route_reason", reason, "graph_candidates", channels["graph"].Candidates, "budget_used", used, "budget", input.Budget, "truncated", truncation != nil, "hop_truncated", graph.boundary != nil, "evidence_path_selection", service.config.EvidencePathSelection, "selection_candidates", selection.Candidates, "selection_selected", selection.Selected, "selection_connectors", selection.Connectors, "selection_unreachable", selection.Unreachable)
	return flow, nil
}

// ErrEmbeddingUnavailable은 질의 임베딩 생성이 실패했음을 나타낸다.
// 상류 제공자 장애와 채널 자체의 장애를 나눠 재려면 두 사유가 구분되어야 한다.
var ErrEmbeddingUnavailable = errors.New("질의 임베딩을 만들지 못했다")

// failureDisabled는 비교 단계 구성으로 채널을 끈 상태다. 조회가 실패한 것이 아니므로
// 모든 채널 실패 판정에는 넣지 않는다.
const failureDisabled = "disabled"

// failureReason은 실패를 「측정」이 집계할 수 있는 고정 분류로 좁힌다.
func failureReason(err error) string {
	switch {
	case errors.Is(err, ErrEmbeddingUnavailable):
		return "embedding_unavailable"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "store_unavailable"
	}
}

type combinedCandidate struct {
	value    model.Context
	channels []string
	score    float64
	folded   int
	distance int
}

type channelResult struct {
	name       string
	candidates []store.SearchCandidate
	metric     Channel
	distances  map[model.ID]int
	edges      []store.HopEdge
	boundary   *int
}

func (service *Service) globalSummaries(ctx context.Context, input Input) channelResult {
	result := channelResult{name: "global_summary"}
	// 명시한 전역 범위는 클라이언트가 고른 진입점 계약이므로 배포 구성으로 끄지 않는다.
	// 구성이 끄는 것은 국소 결과의 관련성이 낮을 때 전역 요약을 시작점으로 더하는
	// `auto`의 자동 전환뿐이다. 적응형 라우팅은 전역 진입 여부를 직접 정하므로 이 값을 보지
	// 않는다.
	if input.Scope != "global" && !service.config.GlobalFallback && !service.config.AdaptiveRouting {
		result.metric.Failure = failureDisabled
		return result
	}
	started := time.Now()
	candidates, err := service.store.GlobalSummaryCandidates(ctx, input.GraphID, input.AsOf, service.config.CandidateLimit)
	result.metric.Latency = time.Since(started)
	if err != nil {
		result.metric.Failure = failureReason(err)
		service.logger.ErrorContext(ctx, "전역 요약 시작점 조회 실패", "reason", result.metric.Failure, "error", err.Error())
		return result
	}
	result.candidates = candidates
	result.metric.Candidates = len(candidates)
	return result
}

// graphCandidates는 진입점에서 depth홉까지 확장한다. globalFallback은 전역 요약에서
// derived_from만 따라 내려가는 확장이다.
func (service *Service) graphCandidates(ctx context.Context, input Input, entryPoints []model.Context, globalFallback bool, depth int) channelResult {
	result := channelResult{name: "graph"}
	// 진입점 부재를 먼저 판정한다. 「검색 채널 실행기」가 이 사유를 2단계에 두었으므로
	// 비교 단계로 채널을 끈 것과 구분되어야 한다.
	if len(entryPoints) == 0 {
		result.metric.Failure = "no_entry_points"
		return result
	}
	if service.config.GraphStage == GraphStageBaseline && !globalFallback {
		result.metric.Failure = failureDisabled
		return result
	}
	started := time.Now()
	filters := []string{"derived_from", "supersedes", "has_member"}
	if service.config.GraphStage == GraphStageRelations || service.config.GraphStage == GraphStageGlobal {
		filters = append(filters, "precedes", "causes", "part_of", "relates_to")
	}
	if input.Scope == "global" || globalFallback {
		// 전역 범위는 전역 요약에서 근거를 따라 내려가는 흐름이다.
		filters = []string{"derived_from"}
	}
	// 시작 노드를 한 번에 넘긴다. 진입점마다 따로 물으면 왕복이 진입점 수만큼 늘고
	// 「채널 구현」이 하나로 두기로 한 결과 상한이 진입점마다 겹친다.
	hops, err := service.store.HopContextsFrom(ctx, input.GraphID, entryPoints, depth, "both", filters, input.MaxHopNodes)
	result.metric.Latency = time.Since(started)
	if err != nil {
		result.metric.Failure = failureReason(err)
		service.logger.ErrorContext(ctx, "그래프 경로 검색 실패", "reason", result.metric.Failure, "error", err.Error())
		return result
	}
	// 거리 0은 진입 채널이 이미 낸 시작 노드이므로 확장분만 이 채널의 후보로 둔다.
	values := make([]store.SearchCandidate, 0, len(hops.Contexts))
	for _, value := range hops.Contexts {
		if hops.Distances[value.ID] == 0 {
			continue
		}
		values = append(values, store.SearchCandidate{Context: value})
	}
	slices.SortFunc(values, func(left, right store.SearchCandidate) int {
		if order := cmp.Compare(hops.Distances[left.Context.ID], hops.Distances[right.Context.ID]); order != 0 {
			return order
		}
		if order := right.Context.RecordedAt.Compare(left.Context.RecordedAt); order != 0 {
			return order
		}
		return cmp.Compare(left.Context.ID.String(), right.Context.ID.String())
	})
	result.candidates, result.distances, result.edges = values, hops.Distances, hops.Edges
	if hops.Truncated {
		result.boundary = &hops.Boundary
	}
	result.metric.Candidates = len(values)
	return result
}

// hopDepth는 플랜의 최대 홉 수를 탐색 깊이로 바꾼다. MaxHops 0은 「계정 플랜」이 선언한
// 대로 한도 없음이다. 그대로 탐색 깊이로 넘기면 시작 노드만 돌아오고 거리 0은 후보에서
// 빠져 그래프 채널이 실패 표시 없이 항상 0건이 된다. 탐색은 더 넓힐 곳이 없거나 결과
// 상한에 닿으면 끝난다.
func hopDepth(maxHops int) int {
	if maxHops == 0 {
		return math.MaxInt
	}
	return maxHops
}

// legacyRoute는 적응형 라우팅을 거치지 않은 요청이 실제로 실행한 경로를 적응형 경로 이름으로
// 나타낸다. 기존 auto 정책의 경로 오선택률을 같은 기준으로 재기 위한 표시다.
func legacyRoute(scope string, stage GraphStage, globalFallbackApplied bool, graph channelResult) (Route, string) {
	reason := ReasonLegacyAuto
	if scope != "auto" {
		reason = ReasonExplicitScope
	}
	switch {
	case scope == "global" || globalFallbackApplied:
		return RouteGlobal, reason
	case stage != GraphStageBaseline && graph.metric.Failure == "":
		return RouteLocal, reason
	default:
		return RouteDirect, reason
	}
}

func candidateContexts(candidates []combinedCandidate) []model.Context {
	values := make([]model.Context, 0, len(candidates))
	for _, candidate := range candidates {
		values = append(values, candidate.value)
	}
	return values
}

func contextIDs(values []model.Context) []model.ID {
	ids := make([]model.ID, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	return ids
}

func filterEdges(selected []combinedCandidate, results []channelResult) []store.HopEdge {
	included := make(map[model.ID]struct{}, len(selected))
	for _, candidate := range selected {
		included[candidate.value.ID] = struct{}{}
	}
	edges := make(map[string]store.HopEdge, len(selected))
	for _, result := range results {
		for _, edge := range result.edges {
			if _, found := included[edge.FromID]; !found {
				continue
			}
			if _, found := included[edge.ToID]; !found {
				continue
			}
			edges[edge.FromID.String()+"|"+edge.ToID.String()+"|"+edge.Kind] = edge
		}
	}
	return slices.SortedFunc(edgesValues(edges), compareEdges)
}

func edgesValues(edges map[string]store.HopEdge) func(func(store.HopEdge) bool) {
	return func(yield func(store.HopEdge) bool) {
		for _, edge := range edges {
			if !yield(edge) {
				return
			}
		}
	}
}

func compareEdges(left, right store.HopEdge) int {
	if order := cmp.Compare(left.FromID.String(), right.FromID.String()); order != 0 {
		return order
	}
	if order := cmp.Compare(left.ToID.String(), right.ToID.String()); order != 0 {
		return order
	}
	return cmp.Compare(left.Kind, right.Kind)
}

func combine(results []channelResult) []combinedCandidate {
	byID := make(map[model.ID]int)
	combined := make([]combinedCandidate, 0)
	for _, result := range results {
		if result.metric.Failure != "" {
			continue
		}
		for rank, candidate := range result.candidates {
			distance := 0
			if result.distances != nil {
				distance = result.distances[candidate.Context.ID]
			}
			if index, found := byID[candidate.Context.ID]; found {
				combined[index].score += 1 / float64(rank+1)
				combined[index].channels = append(combined[index].channels, result.name)
				combined[index].distance = min(combined[index].distance, distance)
				continue
			}
			byID[candidate.Context.ID] = len(combined)
			combined = append(combined, combinedCandidate{value: candidate.Context, channels: []string{result.name}, score: 1 / float64(rank+1), distance: distance})
		}
	}
	slices.SortFunc(combined, func(left, right combinedCandidate) int {
		if order := cmp.Compare(right.score, left.score); order != 0 {
			return order
		}
		if order := right.value.RecordedAt.Compare(left.value.RecordedAt); order != 0 {
			return order
		}
		return cmp.Compare(left.value.ID.String(), right.value.ID.String())
	})
	for index := range combined {
		slices.Sort(combined[index].channels)
	}
	return combined
}

// foldSimilarDerived는 근거 집합과 종류가 같은 파생 중 본문이 충분히 비슷한 것을 접는다.
//
// 근거가 다르면 같은 문장이라도 접지 않는다. 서로 다른 근거를 가리키는 파생은 예산을 차지할
// 값이 있다는 것이 「예산 적용과 절단」의 근거다. 후보는 이미 순위 순이므로 먼저 만난 쪽이
// 대표가 된다.
func foldSimilarDerived(candidates []combinedCandidate, threshold float64) []combinedCandidate {
	groups := make(map[string][]int)
	result := make([]combinedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		key, ok := derivedFoldKey(candidate.value)
		if !ok {
			result = append(result, candidate)
			continue
		}
		folded := false
		for _, representative := range groups[key] {
			if bodySimilarity(result[representative].value.Body, candidate.value.Body) >= threshold {
				result[representative].folded++
				folded = true
				break
			}
		}
		if folded {
			continue
		}
		groups[key] = append(groups[key], len(result))
		result = append(result, candidate)
	}
	return result
}

// derivedFoldKey는 같은 근거와 같은 종류라는 묶음 조건을 하나의 키로 만든다.
func derivedFoldKey(value model.Context) (string, bool) {
	if value.Layer != model.LayerDerived || value.Derived == nil {
		return "", false
	}
	references := slices.Clone(value.Derived.DerivedFrom)
	slices.SortFunc(references, func(left, right model.ID) int { return cmp.Compare(left.String(), right.String()) })
	parts := make([]string, len(references))
	for index, referenceID := range references {
		parts[index] = referenceID.String()
	}
	return string(value.Derived.Kind) + "|" + string(value.Derived.EvidenceState) + "|" + strings.Join(parts, ","), true
}

// bodySimilarity는 본문의 문자 삼중자 자카드 유사도를 돌려준다.
//
// 임베딩을 다시 부르지 않는 이유는 접기가 응답 구성 시점의 정리이기 때문이다. 채널이 고른
// 후보만 비교하면 되고, 같은 입력에 같은 결과를 주어야 「검색 품질 평가」의 반복 측정이
// 성립한다. 문자 단위라 공백 규칙이 다른 언어에서도 같은 방식으로 동작한다.
func bodySimilarity(left, right string) float64 {
	if left == right {
		return 1
	}
	leftGrams, rightGrams := trigrams(left), trigrams(right)
	if len(leftGrams) == 0 || len(rightGrams) == 0 {
		return 0
	}
	shared := 0
	for gram := range leftGrams {
		if _, found := rightGrams[gram]; found {
			shared++
		}
	}
	union := len(leftGrams) + len(rightGrams) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// trigrams는 본문을 문자 삼중자 집합으로 바꾼다. 세 글자보다 짧으면 본문 자체를 담는다.
func trigrams(text string) map[string]struct{} {
	runes := []rune(text)
	grams := make(map[string]struct{})
	if len(runes) == 0 {
		return grams
	}
	if len(runes) < 3 {
		grams[string(runes)] = struct{}{}
		return grams
	}
	for index := 0; index+3 <= len(runes); index++ {
		grams[string(runes[index:index+3])] = struct{}{}
	}
	return grams
}

// applyBudget은 순위 순서대로 담다가 예산을 넘기는 컨텍스트에서 멈춘다.
// budget 0은 한도 없음이므로 절단하지 않는다.
func applyBudget(candidates []combinedCandidate, budget int) ([]combinedCandidate, int, *Truncation) {
	used := 0
	for index, candidate := range candidates {
		length := utf8.RuneCountInString(candidate.value.Body)
		if budget > 0 && used+length > budget {
			return candidates[:index], used, &Truncation{Reason: "budget", Excluded: len(candidates) - index}
		}
		used += length
	}
	return candidates, used, nil
}
