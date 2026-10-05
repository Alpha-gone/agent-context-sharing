// 「요청 단위 일관 읽기 비교」를 실행한다. Read Committed 검색과 채택한 동기화 스냅숏을
// 병렬·순차 실행과 함께 같은 그래프·질의·검색 구성에서 견준다. 채택하지 않은 내용 판
// 재시도의 측정값은 reviews/2026-09-27-consistent-read.md에 남아 있다.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

// consistencyToggled는 동시 쓰기 표본에서 폐기와 복구를 오가는 파생 수다. 허브 원천, 사건
// 둘과 임시 파생 하나를 더해도 채널 후보 상한보다 작아야 일관성 판정이 성립한다.
const consistencyToggled = 20

// consistencyVariant는 비교할 구성 하나다.
type consistencyVariant struct {
	Name        string
	Consistency search.Consistency
	Execution   search.Execution
}

func consistencyVariants() []consistencyVariant {
	variants := make([]consistencyVariant, 0, 6)
	for _, consistency := range []struct {
		name  string
		value search.Consistency
	}{{"read_committed", ""}, {"snapshot", search.ConsistencySnapshot}} {
		for _, execution := range []search.Execution{search.ExecutionParallel, search.ExecutionSequential} {
			variants = append(variants, consistencyVariant{Name: consistency.name + "/" + string(execution), Consistency: consistency.value, Execution: execution})
		}
	}
	return variants
}

// consistencyReport는 결과 파일이다. 질의문과 본문은 싣지 않는다.
type consistencyReport struct {
	StartedAt      time.Time              `json:"started_at"`
	ContextVersion string                 `json:"context_dataset_version"`
	QueryVersion   string                 `json:"query_dataset_version"`
	Conditions     consistencyConditions  `json:"conditions"`
	Equivalence    []equivalenceResult    `json:"equivalence"`
	Load           []loadResult           `json:"concurrent_requests"`
	Concurrent     []concurrentReadResult `json:"concurrent_writes"`
}

type consistencyConditions struct {
	Requests int `json:"requests_per_variant"`
	// WritePausesMS 필드에는 쓰기 작업자가 쓰기 사이에 쉬는 시간 후보를 둔다. 불일치 빈도가
	// 쓰기 빈도에 따라 달라지므로 빈도 여럿에서 잰다.
	WritePausesMS []int `json:"write_pauses_ms"`
	// LoadConcurrency와 LoadRequests 필드에는 동시 요청 부하의 동시 요청자 수 후보와 조건마다
	// 보낼 요청 수를 둔다.
	LoadConcurrency   []int   `json:"load_concurrency"`
	LoadRequests      int     `json:"load_requests"`
	ToggledContexts   int     `json:"toggled_contexts"`
	GraphStage        string  `json:"graph_stage"`
	Budget            int     `json:"budget"`
	MaxHops           int     `json:"max_hops"`
	MaxHopNodes       int     `json:"max_hop_nodes"`
	CandidateLimit    int     `json:"channel_candidate_limit"`
	SemanticThreshold float64 `json:"semantic_similarity_threshold"`
	EmbeddingModel    string  `json:"embedding_model"`
	FoldThreshold     float64 `json:"fold_threshold"`
	IndexTargets      string  `json:"index_targets"`
}

// loadResult는 동시 요청 부하에서 한 구성의 처리량과 연결 풀 대기다. 대기 획득과 대기
// 시간은 측정 구간의 증가분이다.
type loadResult struct {
	Variant       string         `json:"variant"`
	Concurrency   int            `json:"concurrency"`
	Requests      int            `json:"requests"`
	Errors        int            `json:"errors"`
	Throughput    float64        `json:"requests_per_second"`
	LatencyMS     latencySummary `json:"latency_ms"`
	MaxConns      int32          `json:"pool_max_conns"`
	EmptyAcquires int64          `json:"pool_empty_acquires"`
	AcquireWaitMS float64        `json:"pool_acquire_wait_ms"`
}

// equivalenceResult는 동시 쓰기가 없을 때 한 구성이 기준 구성과 다른 응답을 낸 질의 수다.
type equivalenceResult struct {
	Variant    string   `json:"variant"`
	Queries    int      `json:"queries"`
	Mismatches int      `json:"mismatches"`
	Examples   []string `json:"mismatch_query_ids,omitzero"`
}

// concurrentReadResult는 동시 쓰기 중 한 구성의 결과다.
type concurrentReadResult struct {
	Variant         string         `json:"variant"`
	WritePauseMS    int            `json:"write_pause_ms"`
	WritesPerSecond float64        `json:"writes_per_second"`
	Requests        int            `json:"requests"`
	Errors          int            `json:"errors"`
	Inconsistent    int            `json:"inconsistent"`
	Writes          int            `json:"writes"`
	WriteErrors     int            `json:"write_errors"`
	LatencyMS       latencySummary `json:"latency_ms"`
	Connections     float64        `json:"connections_per_request"`
	PeakConnections int            `json:"peak_connections"`
	HoldMS          latencySummary `json:"snapshot_hold_ms"`
}

type latencySummary struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	Max  float64 `json:"max"`
}

func summarize(values []float64) latencySummary {
	if len(values) == 0 {
		return latencySummary{}
	}
	sorted := slices.Sorted(slices.Values(values))
	at := func(percent int) float64 { return sorted[(percent*len(sorted)+99)/100-1] }
	return latencySummary{Mean: mean(sorted), P50: at(50), P95: at(95), Max: sorted[len(sorted)-1]}
}

// consistencyService는 비교 구성 하나의 검색 실행기를 만든다. 그래프 단계는 확장 채널이
// 동시 쓰기를 진입 채널과 다른 시점에 읽을 수 있도록 참조 확장을 켠다.
func consistencyService(database *store.Store, worker search.Embedder, loaded settings, stage search.GraphStage, variant consistencyVariant) (*search.Service, error) {
	return search.New(database, worker, search.Config{
		Execution:         variant.Execution,
		CandidateLimit:    loaded.candidateLimit,
		SemanticThreshold: loaded.semanticThreshold,
		FoldThreshold:     loaded.foldThreshold,
		GraphStage:        stage,
		GlobalFallback:    true,
		Consistency:       variant.Consistency,
	}, slog.Default())
}

// runConsistency는 결과 동등성과 동시 쓰기 일관성을 차례로 잰다.
func runConsistency(ctx context.Context, database *store.Store, worker *index.Worker, graph loadedGraph, contexts contextSet, queries querySet, loaded settings, conditions consistencyConditions) (result consistencyReport, err error) {
	stage := search.GraphStage(conditions.GraphStage)
	result = consistencyReport{StartedAt: time.Now().UTC(), ContextVersion: contexts.Version, QueryVersion: queries.Version, Conditions: conditions}
	variants := consistencyVariants()
	signatures := map[string]string{}
	for index, variant := range variants {
		service, err := consistencyService(database, worker, loaded, stage, variant)
		if err != nil {
			return consistencyReport{}, err
		}
		entry := equivalenceResult{Variant: variant.Name}
		for _, query := range queries.Queries {
			flow, err := service.Flow(ctx, consistencyInput(graph.GraphID, query.WorkContext, query.Scope, conditions))
			if err != nil {
				return consistencyReport{}, fmt.Errorf("구성 %s 질의 %s: %w", variant.Name, query.ID, err)
			}
			entry.Queries++
			signature := flowSignature(flow)
			if index == 0 {
				signatures[query.ID] = signature
				continue
			}
			if signatures[query.ID] != signature {
				entry.Mismatches++
				if len(entry.Examples) < 5 {
					entry.Examples = append(entry.Examples, query.ID)
				}
			}
		}
		result.Equivalence = append(result.Equivalence, entry)
		slog.Info("동등성 비교 완료", "variant", variant.Name, "mismatches", entry.Mismatches)
	}

	for _, concurrency := range conditions.LoadConcurrency {
		for _, variant := range variants {
			if variant.Execution != search.ExecutionParallel {
				continue
			}
			service, err := consistencyService(database, worker, loaded, stage, variant)
			if err != nil {
				return consistencyReport{}, err
			}
			measured := measureLoad(ctx, database, service, graph.GraphID, queries, variant.Name, concurrency, conditions)
			result.Load = append(result.Load, measured)
			slog.Info("동시 요청 비교 완료", "variant", variant.Name, "concurrency", concurrency, "errors", measured.Errors, "empty_acquires", measured.EmptyAcquires)
		}
	}

	scenario, err := createConsistencyScenario(ctx, database, worker)
	if err != nil {
		return consistencyReport{}, err
	}
	defer func() {
		err = errors.Join(err, deleteConsistencyGraph(ctx, database, scenario.graph))
	}()
	for _, pause := range conditions.WritePausesMS {
		for _, variant := range variants {
			service, err := consistencyService(database, worker, loaded, stage, variant)
			if err != nil {
				return consistencyReport{}, err
			}
			measured, err := scenario.measure(ctx, service, variant, conditions, time.Duration(pause)*time.Millisecond)
			if err != nil {
				return consistencyReport{}, err
			}
			result.Concurrent = append(result.Concurrent, measured)
			slog.Info("동시 쓰기 비교 완료", "variant", variant.Name, "pause_ms", pause, "inconsistent", measured.Inconsistent, "errors", measured.Errors)
		}
	}
	return result, nil
}

// measureLoad는 동시 요청자 concurrency개가 데이터셋 질의를 돌려가며 LoadRequests개의 요청을
// 보낸다. 운영 기본 실행 방식인 병렬 실행만 재며, 한 요청이 동시에 쥐는 연결이 가장 많은
// 조건에서 풀이 포화되는지 본다.
func measureLoad(ctx context.Context, database *store.Store, service *search.Service, graphID model.ID, queries querySet, name string, concurrency int, conditions consistencyConditions) loadResult {
	result := loadResult{Variant: name, Concurrency: concurrency, Requests: conditions.LoadRequests}
	before := database.PoolStats()
	jobs := make(chan int)
	var mu sync.Mutex
	latencies := make([]float64, 0, conditions.LoadRequests)
	var group sync.WaitGroup
	started := time.Now()
	for range concurrency {
		group.Go(func() {
			for index := range jobs {
				query := queries.Queries[index%len(queries.Queries)]
				requestStarted := time.Now()
				_, err := service.Flow(ctx, consistencyInput(graphID, query.WorkContext, query.Scope, conditions))
				elapsed := float64(time.Since(requestStarted).Microseconds()) / 1000
				mu.Lock()
				latencies = append(latencies, elapsed)
				if err != nil {
					result.Errors++
				}
				mu.Unlock()
			}
		})
	}
	for index := range conditions.LoadRequests {
		jobs <- index
	}
	close(jobs)
	group.Wait()
	after := database.PoolStats()
	result.Throughput = float64(conditions.LoadRequests) / time.Since(started).Seconds()
	result.LatencyMS = summarize(latencies)
	result.MaxConns = after.MaxConns
	result.EmptyAcquires = after.EmptyAcquires - before.EmptyAcquires
	result.AcquireWaitMS = float64((after.AcquireWait - before.AcquireWait).Microseconds()) / 1000
	return result
}

func consistencyInput(graphID model.ID, workContext, scope string, conditions consistencyConditions) search.Input {
	return search.Input{GraphID: graphID, WorkContext: workContext, Scope: scope, Budget: conditions.Budget, MaxHops: conditions.MaxHops, MaxHopNodes: conditions.MaxHopNodes}
}

// flowSignature는 「요청 단위 일관 읽기 비교」가 대조하라고 한 응답 구성 요소를 한 문자열로
// 만든다. 컨텍스트·관계·출처·정렬·부분 상태이며 지연과 측정값은 뺀다.
func flowSignature(flow search.Flow) string {
	var builder strings.Builder
	for _, item := range flow.Contexts {
		fmt.Fprintf(&builder, "c %s %d %v %v %d %d;", item.Value.ID, item.Rank, item.MatchedChannels, item.OriginKinds, item.EntryDistance, item.FoldedCount)
	}
	for _, edge := range flow.Edges {
		fmt.Fprintf(&builder, "e %s %s %s;", edge.FromID, edge.ToID, edge.Kind)
	}
	for _, id := range flow.EntryPoints {
		fmt.Fprintf(&builder, "p %s;", id)
	}
	fmt.Fprintf(&builder, "b %d %d;", flow.BudgetUsed, flow.Budget)
	if flow.Truncation != nil {
		fmt.Fprintf(&builder, "t %s %d;", flow.Truncation.Reason, flow.Truncation.Excluded)
	}
	if flow.HopBoundary != nil {
		fmt.Fprintf(&builder, "h %d;", *flow.HopBoundary)
	}
	for _, name := range slices.Sorted(maps.Keys(flow.Channels)) {
		fmt.Fprintf(&builder, "f %s %s %d;", name, flow.Channels[name].Failure, flow.Channels[name].Candidates)
	}
	return builder.String()
}

// mixedStates는 응답이 한 커밋 상태로 설명되지 않는지 판정한다. 표본의 활성 컨텍스트 수가
// 채널 후보 상한보다 작으므로 어느 한 상태에서든 활성 컨텍스트는 모두 시간 필터 후보가
// 된다. 그래프 경로 채널만 낸 컨텍스트가 있다는 것은 확장이 시간 필터와 다른 상태를 읽어
// 그 사이에 공개된 컨텍스트를 봤다는 뜻이다.
func mixedStates(flow search.Flow) bool {
	return slices.ContainsFunc(flow.Contexts, func(item search.Context) bool {
		return slices.Equal(item.MatchedChannels, []string{"graph"})
	})
}

// consistencyScenario는 동시 쓰기를 받는 표본 그래프다. 허브 원천 하나를 근거로 한 파생
// 스무 개, 허브를 구성원으로 한 사건 둘이 있다.
type consistencyScenario struct {
	database *store.Store
	worker   *index.Worker
	graph    loadedGraph
	toggled  []model.ID
	events   [2]model.ID
	hub      model.ID
}

func createConsistencyScenario(ctx context.Context, database *store.Store, worker *index.Worker) (scenario *consistencyScenario, err error) {
	occurred := time.Now().UTC().Add(-time.Hour)
	set := contextSet{Version: "consistency-scenario", Contexts: []contextSpec{{Key: "hub", Layer: "source", Body: "동시 쓰기 검증 허브 원천이다.",
		Source: &sourceSpec{Channel: "conversation", Locator: "urn:consistency:hub:" + occurred.Format(time.RFC3339Nano), OccurredAt: occurred, OriginKind: "user_utterance"}}}}
	for index := range consistencyToggled {
		set.Contexts = append(set.Contexts, contextSpec{Key: fmt.Sprintf("c%02d", index), Layer: "derived", Body: fmt.Sprintf("동시 쓰기 검증 파생 %02d이다.", index),
			Derived: &derivedSpec{Kind: "proposition", DerivedFrom: []string{"hub"}, EvidenceState: "observation"}})
	}
	for index := range 2 {
		start := occurred.Add(-time.Duration(2-index) * time.Minute)
		end := occurred.Add(time.Minute)
		set.Contexts = append(set.Contexts, contextSpec{Key: fmt.Sprintf("e%d", index), Layer: "event", Body: fmt.Sprintf("동시 쓰기 검증 사건 %d이다.", index),
			Event: &eventSpec{Members: []string{"hub"}, Start: start, End: &end}})
	}
	graph, err := loadGraph(ctx, database, set)
	if err != nil {
		return nil, fmt.Errorf("동시 쓰기 표본 적재: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, deleteConsistencyGraph(ctx, database, graph))
		}
	}()
	if err := drainIndexQueue(ctx, worker, graph.GraphID, len(set.Contexts)*4); err != nil {
		return nil, err
	}
	scenario = &consistencyScenario{database: database, worker: worker, graph: graph, hub: graph.Keys["hub"], events: [2]model.ID{graph.Keys["e0"], graph.Keys["e1"]}}
	for index := range consistencyToggled {
		scenario.toggled = append(scenario.toggled, graph.Keys[fmt.Sprintf("c%02d", index)])
	}
	return scenario, nil
}

func deleteConsistencyGraph(ctx context.Context, database *store.Store, graph loadedGraph) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := database.SetGraphDeleted(cleanupCtx, graph.GraphID, graph.AccountID, true); err != nil {
		return fmt.Errorf("동시 쓰기 그래프 정리: %w", err)
	}
	return nil
}

// writerState는 쓰기 작업자가 돌아가며 바꾸는 상태다.
type writerState struct {
	next      int
	discarded []bool
	relation  *model.Relation
	temporary *model.ID
}

// write는 쓰기 하나를 실행한다. 컨텍스트 폐기·복구, 관계 확정·폐기, 임시 파생 생성과 색인
// 공개·폐기를 차례로 돌려 「요청 단위 일관 읽기 비교」가 정한 동시 쓰기를 모두 겹친다.
func (scenario *consistencyScenario) write(ctx context.Context, state *writerState) error {
	step := state.next
	state.next++
	switch step % 3 {
	case 0:
		target := (step / 3) % len(scenario.toggled)
		discarded := !state.discarded[target]
		if _, err := scenario.database.SetContextDeleted(ctx, scenario.graph.GraphID, scenario.toggled[target], scenario.graph.AccountID, discarded, 0); err != nil {
			return err
		}
		state.discarded[target] = discarded
		return nil
	case 1:
		if state.relation != nil && state.relation.State == model.RelationStateConfirmed {
			discarded, err := scenario.database.DiscardRelation(ctx, scenario.graph.GraphID, state.relation.ID, nil)
			if err != nil {
				return err
			}
			state.relation = &discarded
			return nil
		}
		relationID, err := model.NewID()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		confirmed, err := scenario.database.ConfirmRelation(ctx, scenario.graph.GraphID, model.Relation{ID: relationID, GraphID: scenario.graph.GraphID, Type: model.RelationTypePrecedes,
			FromContextID: scenario.events[0], ToContextID: scenario.events[1], State: model.RelationStateConfirmed, ProposedBy: model.ProposalSourceAgent, ProposedAt: now,
			ConfirmedBy: scenario.graph.AccountID, ConfirmedByAgent: scenario.graph.AccountID, ConfirmedAt: &now}, nil)
		if err != nil {
			return err
		}
		state.relation = &confirmed
		return nil
	default:
		if state.temporary != nil {
			if _, err := scenario.database.SetContextDeleted(ctx, scenario.graph.GraphID, *state.temporary, scenario.graph.AccountID, true, 0); err != nil {
				return err
			}
			state.temporary = nil
			return nil
		}
		created, err := createContext(ctx, scenario.database, scenario.graph.GraphID, scenario.graph.AccountID, contextSpec{Layer: "derived", Body: fmt.Sprintf("동시 쓰기 검증 임시 파생 %d이다.", step),
			Derived: &derivedSpec{Kind: "proposition", EvidenceState: "observation"}}, []model.ID{scenario.hub})
		if err != nil {
			return err
		}
		state.temporary = &created
		_, err = scenario.worker.RunOnceInGraph(ctx, scenario.graph.GraphID)
		return err
	}
}

// measure는 쓰기 작업자를 돌리는 동안 한 구성으로 요청을 차례로 보낸다.
func (scenario *consistencyScenario) measure(ctx context.Context, service *search.Service, variant consistencyVariant, conditions consistencyConditions, pause time.Duration) (concurrentReadResult, error) {
	result := concurrentReadResult{Variant: variant.Name, WritePauseMS: int(pause.Milliseconds()), Requests: conditions.Requests}
	measureStarted := time.Now()
	stop := make(chan struct{})
	var group sync.WaitGroup
	state := &writerState{discarded: make([]bool, len(scenario.toggled))}
	var writes, writeErrors int
	group.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := scenario.write(ctx, state); err != nil {
				writeErrors++
				slog.Warn("동시 쓰기 실패", "error", err.Error())
			}
			writes++
			if pause > 0 {
				select {
				case <-stop:
					return
				case <-time.After(pause):
				}
			}
		}
	})
	latencies := make([]float64, 0, conditions.Requests)
	holds := make([]float64, 0, conditions.Requests)
	connections := 0
	for range conditions.Requests {
		started := time.Now()
		flow, err := service.Flow(ctx, consistencyInput(scenario.graph.GraphID, "동시 쓰기 검증 질의", "local", conditions))
		latencies = append(latencies, float64(time.Since(started).Microseconds())/1000)
		if err != nil {
			result.Errors++
			slog.Warn("동시 쓰기 중 검색 실패", "variant", variant.Name, "error", err.Error())
			continue
		}
		if mixedStates(flow) {
			result.Inconsistent++
		}
		connections += flow.Read.Connections
		result.PeakConnections = max(result.PeakConnections, flow.Read.PeakConnections)
		if variant.Consistency == search.ConsistencySnapshot {
			holds = append(holds, float64(flow.Read.Hold.Microseconds())/1000)
		}
	}
	close(stop)
	group.Wait()
	result.Writes, result.WriteErrors = writes, writeErrors
	result.WritesPerSecond = float64(writes) / time.Since(measureStarted).Seconds()
	result.LatencyMS = summarize(latencies)
	result.HoldMS = summarize(holds)
	if answered := conditions.Requests - result.Errors; answered > 0 {
		result.Connections = float64(connections) / float64(answered)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := scenario.reset(cleanupCtx, state); err != nil {
		return concurrentReadResult{}, err
	}
	return result, nil
}

// reset은 쓰기 작업자가 멈춘 뒤 활성 파생·관계·임시 파생을 원래 상태로 돌린다.
// 실패를 호출자에게 전달해 상태가 다른 다음 구성의 측정을 막는다.
func (scenario *consistencyScenario) reset(ctx context.Context, state *writerState) error {
	if state.temporary != nil {
		if _, err := scenario.database.SetContextDeleted(ctx, scenario.graph.GraphID, *state.temporary, scenario.graph.AccountID, true, 0); err != nil {
			return fmt.Errorf("임시 파생 폐기: %w", err)
		}
		state.temporary = nil
	}
	for target, discarded := range state.discarded {
		if discarded {
			if _, err := scenario.database.SetContextDeleted(ctx, scenario.graph.GraphID, scenario.toggled[target], scenario.graph.AccountID, false, 0); err != nil {
				return fmt.Errorf("표본 파생 복구: %w", err)
			}
			state.discarded[target] = false
		}
	}
	if state.relation != nil && state.relation.State == model.RelationStateConfirmed {
		discarded, err := scenario.database.DiscardRelation(ctx, scenario.graph.GraphID, state.relation.ID, nil)
		if err != nil {
			return fmt.Errorf("표본 관계 폐기: %w", err)
		}
		state.relation = &discarded
	}
	return nil
}
