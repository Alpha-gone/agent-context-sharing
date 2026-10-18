package search

import (
	"context"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func channelOrders(size int) [][]int {
	var orders [][]int
	var visit func([]int)
	visit = func(prefix []int) {
		if len(prefix) == size {
			orders = append(orders, slices.Clone(prefix))
			return
		}
		for index := range size {
			if !slices.Contains(prefix, index) {
				visit(append(prefix, index))
			}
		}
	}
	visit(nil)
	return orders
}

func TestRankingChannelOrderContract(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000801")
	a := testContext(t, graph, "019a0000-0000-7000-8000-000000000802", "A", 1)
	b := testContext(t, graph, "019a0000-0000-7000-8000-000000000803", "B", 2)
	b.Layer = model.LayerEvent
	c := testContext(t, graph, "019a0000-0000-7000-8000-000000000804", "C", 0)
	d := testContext(t, graph, "019a0000-0000-7000-8000-000000000805", "D", 0)
	e := testContext(t, graph, "019a0000-0000-7000-8000-000000000806", "E", 0)
	f := testContext(t, graph, "019a0000-0000-7000-8000-000000000807", "F", 0)
	list := func(values ...model.Context) []model.SearchCandidate {
		result := make([]model.SearchCandidate, len(values))
		for index, value := range values {
			result[index] = model.SearchCandidate{Context: value, Similarity: 0.9 - float64(index)/10}
		}
		return result
	}
	// A와 B는 서로 다른 누적 순서로 5/3이 된다. C는 3/2이다.
	entry := []channelResult{
		{name: "semantic", candidates: list(a, b)},
		{name: "keyword", candidates: list(c, a, d, e, f, b)},
		{name: "time", candidates: list(b, c, d, e, f, a)},
	}
	for _, stage := range []string{"entry", "final", "failed"} {
		t.Run(stage, func(t *testing.T) {
			results := slices.Clone(entry)
			wantScore, wantDistance := big.NewRat(5, 3), 0
			if stage == "final" {
				results = append(results, channelResult{name: "graph", candidates: list(a, b), distances: map[model.ID]int{a.ID: 2, b.ID: 3}})
				wantScore = big.NewRat(13, 6)
			} else if stage == "failed" {
				results[0].metric.Failure = "store_unavailable"
				// 오류와 함께 온 semantic 후보는 결합·관련성 판정 모두에서 빠진다.
				wantScore = big.NewRat(7, 6)
			}
			baseline := combine(results)
			wantSignals := routeSignals(results, baseline, 0.85)
			wantLocal := relevantLocalContexts(results, baseline, 0.85)
			for _, order := range channelOrders(len(results)) {
				permuted := make([]channelResult, len(order))
				for index, original := range order {
					permuted[index] = results[original]
					if distances := results[original].distances; distances != nil {
						permuted[index].distances = map[model.ID]int{}
						for _, id := range []model.ID{b.ID, a.ID} {
							permuted[index].distances[id] = distances[id]
						}
					}
				}
				got := combine(permuted)
				if !reflect.DeepEqual(got, baseline) || routeSignals(permuted, got, 0.85) != wantSignals || !reflect.DeepEqual(relevantLocalContexts(permuted, got, 0.85), wantLocal) {
					t.Fatalf("채널 순서 %v에서 순위·기여·거리·라우팅 입력이 달라졌다", order)
				}
				for _, candidate := range got {
					if candidate.value.ID == b.ID && (candidate.score.Cmp(wantScore) != 0 || candidate.distance != wantDistance) {
						t.Fatalf("B 점수·거리 = %s/%d", candidate.score, candidate.distance)
					}
				}
			}
			if stage == "entry" && (baseline[0].value.ID != b.ID || baseline[1].value.ID != a.ID || !wantSignals.TopIsEvent || !wantSignals.CrossChannelAgreement) {
				t.Fatal("정확 동점의 기록 시각과 라우팅 입력이 다르다")
			}
			if stage == "final" && (baseline[0].value.ID != a.ID || baseline[1].value.ID != b.ID) {
				t.Fatal("최종 결합에 그래프 순위의 역수가 반영되지 않았다")
			}
		})
	}
	// 채널이 없거나 실패/빈 채널만 있어도 위치 접근 없이 같은 빈 입력을 만든다.
	for _, results := range [][]channelResult{nil, {{name: "time"}}, {{name: "semantic", candidates: list(a), metric: Channel{Failure: "timeout"}}}} {
		combined := combine(results)
		if len(combined) != 0 || routeSignals(results, combined, 0) != (RouteSignals{}) || len(relevantLocalContexts(results, combined, 0)) != 0 {
			t.Fatal("빈 채널·실패 채널이 라우팅 입력에 남았다")
		}
	}
}

func TestRankingKeepsDistinctNearScores(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000811")
	a := testContext(t, graph, "019a0000-0000-7000-8000-000000000812", "앞선 점수", 1)
	b := testContext(t, graph, "019a0000-0000-7000-8000-000000000813", "최근 기록", 9)
	results := []channelResult{{name: "semantic"}, {name: "keyword"}}
	for channel := range results {
		for rank := 1; rank <= 2002; rank++ {
			value := testContext(t, graph, fmt.Sprintf("019a0000-0000-7000-8000-%012d", 10000+channel*3000+rank), "채움", 0)
			if rank == 2001 {
				value = b
			} else if channel == 0 && rank == 2000 || channel == 1 && rank == 2002 {
				value = a
			}
			results[channel].candidates = append(results[channel].candidates, model.SearchCandidate{Context: value})
		}
	}
	// 1/2000+1/2002는 2/2001보다 정확히 2/(2000*2001*2002) 크다.
	// 약 2.5e-10 차이를 동점으로 처리하면 더 최근인 B가 잘못 앞선다.
	for range 2 {
		combined := combine(results)
		aIndex := slices.IndexFunc(combined, func(value combinedCandidate) bool { return value.value.ID == a.ID })
		bIndex := slices.IndexFunc(combined, func(value combinedCandidate) bool { return value.value.ID == b.ID })
		if aIndex < 0 || bIndex < 0 || aIndex >= bIndex {
			t.Fatalf("서로 다른 점수가 동점 처리됐다: A=%d B=%d", aIndex, bIndex)
		}
		difference := new(big.Rat).Sub(combined[aIndex].score, combined[bIndex].score)
		if difference.Cmp(big.NewRat(2, 2000*2001*2002)) != 0 {
			t.Fatalf("정확한 점수 차이 = %s", difference)
		}
		slices.Reverse(results)
	}
}

type orderedEntryStore struct {
	*fakeStore
	started, finished chan string
	release           map[string]chan struct{}
}

func (database *orderedEntryStore) candidates(ctx context.Context, name string, values []model.SearchCandidate) ([]model.SearchCandidate, error) {
	database.started <- name
	select {
	case <-database.release[name]:
		database.finished <- name
		return slices.Clone(values), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (database *orderedEntryStore) SemanticCandidates(ctx context.Context, _ model.ID, _ string, _ []float64, _ time.Time, _ int) ([]model.SearchCandidate, error) {
	return database.candidates(ctx, "semantic", database.semantic)
}
func (database *orderedEntryStore) KeywordCandidates(ctx context.Context, _ model.ID, _ string, _ time.Time, _ int) ([]model.SearchCandidate, error) {
	return database.candidates(ctx, "keyword", database.keyword)
}
func (database *orderedEntryStore) TimeCandidates(ctx context.Context, _ model.ID, _ time.Time, _ int) ([]model.SearchCandidate, error) {
	return database.candidates(ctx, "time", database.time)
}

func TestRankingFlowCompletionOrderAndMetadata(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000821")
	a := testContext(t, graph, "019a0000-0000-7000-8000-000000000822", "의미", 1)
	b := testContext(t, graph, "019a0000-0000-7000-8000-000000000823", "사건", 2)
	b.Layer = model.LayerEvent
	c := testContext(t, graph, "019a0000-0000-7000-8000-000000000824", "시간", 3)
	isolated := testContext(t, graph, "019a0000-0000-7000-8000-000000000825", "고립", 9)
	connected := testContext(t, graph, "019a0000-0000-7000-8000-000000000826", "연결", 8)
	isolated.Layer, connected.Layer = model.LayerEvent, model.LayerEvent
	fixture := func() *fakeStore {
		return &fakeStore{
			semantic: []model.SearchCandidate{{Context: a, Similarity: 0.9}, {Context: b, Similarity: 0.7}},
			keyword:  []model.SearchCandidate{{Context: b}},
			time:     []model.SearchCandidate{{Context: c}},
			hops:     model.HopResult{Contexts: []model.Context{connected, isolated}, Distances: map[model.ID]int{isolated.ID: 1, connected.ID: 2}, Edges: []model.HopEdge{{FromID: b.ID, ToID: connected.ID, Kind: "relates_to"}}},
		}
	}
	config := Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, SemanticThreshold: 0.5, GraphStage: GraphStageRelations, AdaptiveRouting: true, EvidencePathSelection: true}
	input := Input{GraphID: graph, WorkContext: "작업", Scope: "auto", AsOf: time.Now().UTC()}
	normalize := func(flow Flow) Flow {
		for name, metric := range flow.Channels {
			metric.Latency = 0
			flow.Channels[name] = metric
		}
		return flow
	}
	service, err := New(fixture(), fakeEmbedder{}, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := service.Flow(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	baseline = normalize(baseline)
	if !slices.Equal(flowIDs(baseline), idStrings([]model.ID{b.ID, c.ID, a.ID, connected.ID})) || baseline.Route != RouteLocal || baseline.RouteReason != ReasonEventAnchor || !slices.Equal(baseline.EntryPoints, []model.ID{b.ID, a.ID}) {
		t.Fatalf("초기 순위·라우팅·최종 선택 = %+v", baseline)
	}
	for index, want := range []struct {
		rank, distance int
		channels       []string
	}{{1, 0, []string{"keyword", "semantic"}}, {3, 0, []string{"time"}}, {4, 0, []string{"semantic"}}, {5, 2, []string{"graph"}}} {
		got := baseline.Contexts[index]
		if got.Rank != want.rank || got.EntryDistance != want.distance || !slices.Equal(got.MatchedChannels, want.channels) {
			t.Fatalf("선택 전 순위·거리·기여 = %+v, want %+v", got, want)
		}
	}
	if baseline.Selection.Unreachable != 1 || baseline.Channels["semantic"].Contribution != 2 || baseline.Channels["keyword"].Contribution != 1 || baseline.Channels["time"].Contribution != 1 || baseline.Channels["graph"].Contribution != 1 {
		t.Fatalf("기여·선택 집계 = %+v", baseline)
	}
	names := []string{"semantic", "keyword", "time"}
	for _, order := range channelOrders(3) {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			database := &orderedEntryStore{fakeStore: fixture(), started: make(chan string, 3), finished: make(chan string, 3), release: map[string]chan struct{}{}}
			for _, name := range names {
				database.release[name] = make(chan struct{})
			}
			parallelConfig := config
			parallelConfig.Execution = ExecutionParallel
			service, err := New(database, fakeEmbedder{}, parallelConfig, nil)
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				flow Flow
				err  error
			}
			done := make(chan outcome, 1)
			stopped := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				<-stopped
			})
			go func() {
				defer close(stopped)
				flow, err := service.Flow(ctx, input)
				done <- outcome{flow, err}
			}()
			for range 3 {
				select {
				case <-database.started:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			for _, index := range order {
				close(database.release[names[index]])
				select {
				case got := <-database.finished:
					if got != names[index] {
						t.Fatalf("채널 완료 = %s, want %s", got, names[index])
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case got := <-done:
				if got.err != nil || !reflect.DeepEqual(normalize(got.flow), baseline) || !slices.Equal(database.hopStarts, []model.ID{b.ID, a.ID}) {
					t.Fatalf("완료 순서 %v의 결과·라우팅·확장 진입점이 다르다: %v", order, got.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
