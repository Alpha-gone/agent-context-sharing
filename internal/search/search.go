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

// Config는 검색 결합의 배포 구성을 담는다.
type Config struct {
	Execution      Execution
	CandidateLimit int
	FoldThreshold  float64
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
	ContextOriginKinds(context.Context, model.ID, model.ID) ([]model.OriginKind, error)
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
}

// Context는 흐름 응답을 만들기 위해 컨텍스트에 검색 메타데이터를 붙인 값이다.
type Context struct {
	Value           model.Context
	OriginKinds     []model.OriginKind
	MatchedChannels []string
	Rank            int
	FoldedCount     int
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
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: database, embedder: embedder, config: config, logger: logger}, nil
}

// Flow는 그래프 확장 채널을 켠 적 없는 기준선의 세 진입 채널을 결합한다.
func (service *Service) Flow(ctx context.Context, input Input) (Flow, error) {
	if !input.GraphID.IsV7() || input.WorkContext == "" || input.Budget < 1 {
		return Flow{}, fmt.Errorf("검색 입력이 올바르지 않다")
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
	if service.config.Execution == ExecutionParallel {
		var group sync.WaitGroup
		group.Go(func() { run(0, "semantic", semantic) })
		group.Go(func() { run(1, "keyword", keyword) })
		group.Go(func() { run(2, "time", timeFilter) })
		group.Wait()
	} else {
		run(0, "semantic", semantic)
		run(1, "keyword", keyword)
		run(2, "time", timeFilter)
	}
	for _, result := range results {
		channels[result.name] = result.metric
	}
	if channels["semantic"].Failure != "" && channels["keyword"].Failure != "" && channels["time"].Failure != "" {
		return Flow{Channels: channels}, ErrAllChannelsFailed
	}
	combined := combine(results)
	combined = foldSimilarDerived(combined, service.config.FoldThreshold)
	selected, used, truncation := applyBudget(combined, input.Budget)
	ids := make([]model.ID, 0, len(selected))
	contexts := make([]Context, 0, len(selected))
	for rank, candidate := range selected {
		origins, err := service.store.ContextOriginKinds(ctx, input.GraphID, candidate.value.ID)
		if err != nil {
			return Flow{}, fmt.Errorf("컨텍스트 출처 계산: %w", err)
		}
		ids = append(ids, candidate.value.ID)
		contexts = append(contexts, Context{Value: candidate.value, OriginKinds: origins, MatchedChannels: candidate.channels, Rank: rank + 1, FoldedCount: candidate.folded})
		for _, channel := range candidate.channels {
			metric := channels[channel]
			metric.Contribution++
			channels[channel] = metric
		}
	}
	// 비그래프 기준선은 그래프 확장을 끈 구성이므로 references와 relations를 비워 둔다.
	flow := Flow{Contexts: contexts, EntryPoints: ids, BudgetUsed: used, Budget: input.Budget, Channels: channels, Truncation: truncation}
	service.logger.InfoContext(ctx, "비그래프 검색 완료", "semantic_candidates", channels["semantic"].Candidates, "keyword_candidates", channels["keyword"].Candidates, "time_candidates", channels["time"].Candidates, "budget_used", used, "budget", input.Budget, "truncated", truncation != nil)
	return flow, nil
}

// ErrEmbeddingUnavailable은 질의 임베딩 생성이 실패했음을 나타낸다.
// 상류 제공자 장애와 채널 자체의 장애를 나눠 재려면 두 사유가 구분되어야 한다.
var ErrEmbeddingUnavailable = errors.New("질의 임베딩을 만들지 못했다")

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
}

type channelResult struct {
	name       string
	candidates []store.SearchCandidate
	metric     Channel
}

func combine(results []channelResult) []combinedCandidate {
	byID := make(map[model.ID]int)
	combined := make([]combinedCandidate, 0)
	for _, result := range results {
		if result.metric.Failure != "" {
			continue
		}
		for rank, candidate := range result.candidates {
			if index, found := byID[candidate.Context.ID]; found {
				combined[index].score += 1 / float64(rank+1)
				combined[index].channels = append(combined[index].channels, result.name)
				continue
			}
			byID[candidate.Context.ID] = len(combined)
			combined = append(combined, combinedCandidate{value: candidate.Context, channels: []string{result.name}, score: 1 / float64(rank+1)})
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

func applyBudget(candidates []combinedCandidate, budget int) ([]combinedCandidate, int, *Truncation) {
	used := 0
	for index, candidate := range candidates {
		length := utf8.RuneCountInString(candidate.value.Body)
		if used+length > budget {
			return candidates[:index], used, &Truncation{Reason: "budget", Excluded: len(candidates) - index}
		}
		used += length
	}
	return candidates, used, nil
}
