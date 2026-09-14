// Package search는 비그래프 검색 채널 실행, 결합과 컨텍스트 예산 적용을 소유한다.
package search

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	Execution      Execution
	CandidateLimit int
	FoldThreshold  float64
	GraphStage     GraphStage
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
	store    Store
	embedder Embedder
	config   Config
	logger   *slog.Logger
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
	if config.FoldThreshold <= 0 || config.FoldThreshold > 1 {
		return nil, fmt.Errorf("중복 파생 접기 임계값은 0 초과 1 이하여야 한다")
	}
	if config.GraphStage == "" {
		config.GraphStage = GraphStageBaseline
	}
	if !slices.Contains([]GraphStage{GraphStageBaseline, GraphStageReferences, GraphStageRelations, GraphStageGlobal}, config.GraphStage) {
		return nil, fmt.Errorf("그래프 검색 비교 단계가 올바르지 않다")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: database, embedder: embedder, config: config, logger: logger}, nil
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
		embedding, err := service.embedder.Embed(ctx, input.WorkContext)
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
		graph := service.graphCandidates(ctx, input, nil)
		channels[graph.name] = graph.metric
		return Flow{Channels: channels}, ErrAllChannelsFailed
	}
	if input.Scope != "global" && channels["semantic"].Failure != "" && channels["keyword"].Failure != "" && channels["time"].Failure != "" {
		return allEntryChannelsFailed()
	}
	combined := combine(results)
	if input.Scope == "global" || (input.Scope == "auto" && len(combined) == 0) {
		global := service.globalSummaries(ctx, input)
		channels[global.name] = global.metric
		// 전역 범위의 진입 채널은 전역 요약 하나뿐이므로 그 채널의 실패가 곧 모든 채널
		// 실패다. 비교 단계에서 끈 상태는 조회 실패가 아니라 구성이므로 제외한다.
		if input.Scope == "global" && global.metric.Failure != "" && global.metric.Failure != failureDisabled {
			return allEntryChannelsFailed()
		}
		results = append(results, global)
		combined = combine(results)
	}
	entryPoints := candidateContexts(combined)
	graph := service.graphCandidates(ctx, input, entryPoints)
	channels[graph.name] = graph.metric
	results = append(results, graph)
	combined = combine(results)
	combined = foldSimilarDerived(combined, service.config.FoldThreshold)
	selected, used, truncation := applyBudget(combined, input.Budget)
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
	for rank, candidate := range selected {
		contexts = append(contexts, Context{Value: candidate.value, OriginKinds: origins[candidate.value.ID], MatchedChannels: candidate.channels, Rank: rank + 1, FoldedCount: candidate.folded, EntryDistance: candidate.distance})
		for _, channel := range candidate.channels {
			metric := channels[channel]
			metric.Contribution++
			channels[channel] = metric
		}
	}
	edges := filterEdges(selected, results)
	flow := Flow{Contexts: contexts, Edges: edges, EntryPoints: contextIDs(entryPoints), BudgetUsed: used, Budget: input.Budget, Channels: channels, Truncation: truncation, HopBoundary: graph.boundary}
	service.logger.InfoContext(ctx, "작업 컨텍스트 흐름 검색 완료", "semantic_candidates", channels["semantic"].Candidates, "keyword_candidates", channels["keyword"].Candidates, "time_candidates", channels["time"].Candidates, "graph_candidates", channels["graph"].Candidates, "budget_used", used, "budget", input.Budget, "truncated", truncation != nil, "hop_truncated", graph.boundary != nil)
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
	// 명시한 전역 범위는 클라이언트가 고른 진입점 계약이므로 비교 단계로 끄지 않는다.
	// 비교 단계가 끄는 것은 국소 결과가 비었을 때 전역 요약을 시작점으로 더하는
	// `auto`의 되돌림이며, 그것이 「그래프 효과 비교」의 마지막 단계다.
	if input.Scope != "global" && service.config.GraphStage != GraphStageGlobal {
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

func (service *Service) graphCandidates(ctx context.Context, input Input, entryPoints []model.Context) channelResult {
	result := channelResult{name: "graph"}
	// 진입점 부재를 먼저 판정한다. 「검색 채널 실행기」가 이 사유를 2단계에 두었으므로
	// 비교 단계로 채널을 끈 것과 구분되어야 한다.
	if len(entryPoints) == 0 {
		result.metric.Failure = "no_entry_points"
		return result
	}
	if service.config.GraphStage == GraphStageBaseline {
		result.metric.Failure = failureDisabled
		return result
	}
	started := time.Now()
	filters := []string{"derived_from", "supersedes", "has_member"}
	if service.config.GraphStage == GraphStageRelations || service.config.GraphStage == GraphStageGlobal {
		filters = append(filters, "precedes", "causes", "part_of", "relates_to")
	}
	if input.Scope == "global" {
		// 전역 범위는 전역 요약에서 근거를 따라 내려가는 흐름이다.
		filters = []string{"derived_from"}
	}
	// 시작 노드를 한 번에 넘긴다. 진입점마다 따로 물으면 왕복이 진입점 수만큼 늘고
	// 「채널 구현」이 하나로 두기로 한 결과 상한이 진입점마다 겹친다.
	hops, err := service.store.HopContextsFrom(ctx, input.GraphID, entryPoints, input.MaxHops, "both", filters, input.MaxHopNodes)
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
