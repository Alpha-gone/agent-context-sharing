package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestFlowFoldThresholdBounds(t *testing.T) {
	for _, sample := range []struct {
		name  string
		value float64
		valid bool
	}{
		{"nan", math.NaN(), false},
		{"positive_infinity", math.Inf(1), false},
		{"negative_infinity", math.Inf(-1), false},
		{"negative", -0.1, false},
		{"zero", 0, false},
		{"above_one", math.Nextafter(1, 2), false},
		{"smallest_positive", math.SmallestNonzeroFloat64, true},
		{"one", 1, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			service, err := New(&fakeStore{}, fakeEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: sample.value}, nil)
			if (err == nil) != sample.valid || (service != nil) != sample.valid {
				t.Fatalf("threshold=%g service=%v error=%v", sample.value, service, err)
			}
		})
	}
}

func TestFlowFoldIdentityAndSimilarityBoundary(t *testing.T) {
	evidence := []model.ID{newFoldID(t), newFoldID(t)}
	originalEvidence := slices.Clone(evidence)
	base := model.Context{ID: newFoldID(t), Layer: model.LayerDerived, Body: "가나다라", Derived: &model.DerivedAttributes{
		Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateObservation, DerivedFrom: evidence,
	}}
	for _, sample := range []struct {
		name   string
		change func(*model.Context)
		fold   bool
	}{
		{"same", func(*model.Context) {}, true},
		{"evidence_order", func(v *model.Context) { slices.Reverse(v.Derived.DerivedFrom) }, true},
		{"different_evidence", func(v *model.Context) { v.Derived.DerivedFrom[0] = newFoldID(t) }, false},
		{"subset_evidence", func(v *model.Context) { v.Derived.DerivedFrom = v.Derived.DerivedFrom[:1] }, false},
		{"different_kind", func(v *model.Context) { v.Derived.Kind = model.DerivationKindReflection }, false},
		{"different_state", func(v *model.Context) { v.Derived.EvidenceState = model.EvidenceStateExperience }, false},
		{"source", func(v *model.Context) { v.Layer, v.Derived = model.LayerSource, nil }, false},
		{"event", func(v *model.Context) { v.Layer, v.Derived = model.LayerEvent, nil }, false},
		{"different_body", func(v *model.Context) { v.Body = "마바사아" }, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			other := base
			other.ID = newFoldID(t)
			other.Derived = new(*base.Derived)
			other.Derived.DerivedFrom = slices.Clone(evidence)
			sample.change(&other)
			got := foldSimilarDerived([]combinedCandidate{{value: base}, {value: other}}, 1)
			if len(got) != 2-boolInt(sample.fold) || got[0].value.ID != base.ID || got[0].folded != boolInt(sample.fold) {
				t.Fatalf("fold=%t result=%+v", sample.fold, got)
			}
			if !slices.Equal(base.Derived.DerivedFrom, originalEvidence) {
				t.Fatal("접기가 저장 근거 배열을 변경했다")
			}
		})
	}
	// {가나다, 나다라}와 {가나다, 나다마}의 교집합/합집합은 정확히 1/3이다.
	for _, threshold := range []float64{math.Nextafter(1.0/3, 0), 1.0 / 3, math.Nextafter(1.0/3, 1)} {
		t.Run(fmt.Sprintf("threshold_%g", threshold), func(t *testing.T) {
			other := base
			other.ID, other.Body = newFoldID(t), "가나다마"
			got := foldSimilarDerived([]combinedCandidate{{value: base}, {value: other}}, threshold)
			want := 1
			if threshold > 1.0/3 {
				want = 2
			}
			if len(got) != want {
				t.Fatalf("임계값 %g: %d개, want %d", threshold, len(got), want)
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestFlowUnicodeBodyBudgetContract(t *testing.T) {
	candidates := []combinedCandidate{
		{value: model.Context{Body: "가😀e\u0301👩\u200d💻", Source: &model.SourceAttributes{Reference: model.SourceReference{Locator: strings.Repeat("x", 1000)}}}},
		{value: model.Context{Body: "긴후보"}},
		{value: model.Context{Body: "짧"}},
	}
	for _, sample := range []struct {
		budget, selected, used, excluded int
	}{
		{0, 3, 11, 0},
		{6, 0, 0, 3},
		{7, 1, 7, 2},
		{9, 1, 7, 2},
		{10, 2, 10, 1},
		{11, 3, 11, 0},
	} {
		t.Run(fmt.Sprint(sample.budget), func(t *testing.T) {
			selected, used, truncated := applyBudget(candidates, sample.budget)
			if len(selected) != sample.selected || used != sample.used {
				t.Fatalf("selected=%d used=%d", len(selected), used)
			}
			var want *Truncation
			if sample.excluded != 0 {
				want = &Truncation{Reason: "budget", Excluded: sample.excluded}
			}
			if !reflect.DeepEqual(truncated, want) {
				t.Fatalf("truncation=%+v, want %+v", truncated, want)
			}
		})
	}
}

type flowAssemblyStore struct {
	*fakeStore
	origins   map[model.ID][]model.OriginKind
	originIDs []model.ID
}

func (database *flowAssemblyStore) ContextOriginKinds(_ context.Context, _ model.ID, ids []model.ID) (map[model.ID][]model.OriginKind, error) {
	database.originIDs = slices.Clone(ids)
	return database.origins, nil
}

func TestFlowFoldBudgetAndFlatAssembly(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000901")
	externalSource := testID(t, "019a0000-0000-7000-8000-000000000908")
	source := testContext(t, graph, "019a0000-0000-7000-8000-000000000902", "e\u0301", 0)
	source.Source = &model.SourceAttributes{OriginKind: model.OriginKindUserUtterance}
	representative := testContext(t, graph, "019a0000-0000-7000-8000-000000000903", "가😀", 1)
	representative.Layer = model.LayerDerived
	representative.Derived = &model.DerivedAttributes{
		Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateOpinion, ConfidenceState: model.ConfidenceStateDisputed,
		DerivedFrom: []model.ID{source.ID, externalSource}, EvidenceInvalidated: true,
	}
	duplicate := representative
	duplicate.ID, duplicate.RecordedAt = testID(t, "019a0000-0000-7000-8000-000000000904"), representative.RecordedAt.Add(time.Second*6)
	firstEvent := testContext(t, graph, "019a0000-0000-7000-8000-000000000905", "나", 5)
	firstEvent.Layer, firstEvent.Event = model.LayerEvent, &model.EventAttributes{Start: source.RecordedAt, MemberIDs: []model.ID{source.ID}}
	secondEvent := testContext(t, graph, "019a0000-0000-7000-8000-000000000906", "👩\u200d💻", 4)
	secondEvent.Layer, secondEvent.Event = model.LayerEvent, &model.EventAttributes{Start: source.RecordedAt, MemberIDs: []model.ID{source.ID}}
	expired := representative
	expired.ID, expired.Body = testID(t, "019a0000-0000-7000-8000-000000000907"), "만료"
	expired.Derived = new(*representative.Derived)
	expired.Derived.ValidTo = new(source.RecordedAt.Add(-time.Hour))
	edges := []model.HopEdge{
		{FromID: representative.ID, ToID: source.ID, Kind: "derived_from"},
		{FromID: representative.ID, ToID: expired.ID, Kind: "supersedes"},
		{FromID: firstEvent.ID, ToID: source.ID, Kind: "has_member"},
		{FromID: firstEvent.ID, ToID: secondEvent.ID, Kind: "causes"},
		{FromID: firstEvent.ID, ToID: secondEvent.ID, Kind: "part_of"},
		{FromID: firstEvent.ID, ToID: secondEvent.ID, Kind: "precedes"},
		{FromID: firstEvent.ID, ToID: secondEvent.ID, Kind: "relates_to"},
	}
	allEdges := append(slices.Clone(edges), edges[0], model.HopEdge{FromID: duplicate.ID, ToID: source.ID, Kind: "derived_from"},
		model.HopEdge{FromID: secondEvent.ID, ToID: externalSource, Kind: "has_member"})
	slices.Reverse(allEdges)
	wantValues := []model.Context{representative, firstEvent, source, secondEvent, expired}
	for _, budget := range []int{0, 5, 1} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			var baseline Flow
			for _, execution := range []Execution{ExecutionSequential, ExecutionParallel} {
				database := &flowAssemblyStore{fakeStore: &fakeStore{
					semantic: []model.SearchCandidate{{Context: representative}, {Context: duplicate}},
					keyword:  []model.SearchCandidate{{Context: representative}, {Context: duplicate}},
					time:     []model.SearchCandidate{{Context: source}},
					hops: model.HopResult{
						Contexts:  []model.Context{representative, duplicate, source, secondEvent, expired, firstEvent},
						Distances: map[model.ID]int{source.ID: 0, representative.ID: 0, duplicate.ID: 0, firstEvent.ID: 1, secondEvent.ID: 1, expired.ID: 2},
						Edges:     allEdges, Truncated: true, Boundary: 2,
					},
				}, origins: map[model.ID][]model.OriginKind{}}
				for _, value := range wantValues {
					database.origins[value.ID] = []model.OriginKind{model.OriginKindUserUtterance, model.OriginKindExternalContent}
				}
				database.origins[source.ID] = []model.OriginKind{source.Source.OriginKind}
				service, err := New(database, fakeEmbedder{}, Config{Execution: execution, CandidateLimit: 10, FoldThreshold: 1, GraphStage: GraphStageRelations}, nil)
				if err != nil {
					t.Fatal(err)
				}
				flow, err := service.Flow(t.Context(), Input{GraphID: graph, WorkContext: "질의", Scope: "local", Budget: budget, MaxHops: 2})
				if err != nil {
					t.Fatal(err)
				}
				count, used, excluded := 5, 10, 0
				wantEdges := edges
				if budget == 5 {
					count, used, excluded, wantEdges = 3, 5, 2, []model.HopEdge{edges[0], edges[2]}
				}
				if budget == 1 {
					count, used, excluded, wantEdges = 0, 0, 5, nil
				}
				if len(flow.Contexts) != count || flow.BudgetUsed != used || flow.Budget != budget || flow.HopBoundary == nil || *flow.HopBoundary != 2 {
					t.Fatalf("선택·예산·홉 경계: %+v", flow)
				}
				if (flow.Truncation != nil) != (excluded > 0) || excluded > 0 && *flow.Truncation != (Truncation{Reason: "budget", Excluded: excluded}) {
					t.Fatalf("예산 절단: %+v", flow.Truncation)
				}
				if !slices.Equal(flow.Edges, wantEdges) || len(database.originIDs) != count || flow.Selection != (Selection{Candidates: 5, Selected: count}) {
					t.Fatalf("평면 간선·출처·선택: %+v", flow)
				}
				contributions := map[string]int{}
				for i, item := range flow.Contexts {
					wantChannels, distance, folded := []string{"graph"}, 1, 0
					switch i {
					case 0:
						wantChannels, distance, folded = []string{"keyword", "semantic"}, 0, 1
					case 2:
						wantChannels, distance = []string{"time"}, 0
					case 4:
						distance = 2
					}
					if !reflect.DeepEqual(item.Value, wantValues[i]) || item.Value.GraphID != graph || item.Rank != i+1 || item.EntryDistance != distance || item.FoldedCount != folded ||
						!slices.Equal(item.MatchedChannels, wantChannels) || !slices.Equal(item.OriginKinds, database.origins[item.Value.ID]) || database.originIDs[i] != item.Value.ID {
						t.Fatalf("검색·근거 메타데이터: %+v", item)
					}
					for _, name := range item.MatchedChannels {
						contributions[name]++
					}
				}
				for name, channel := range flow.Channels {
					if channel.Failure != "" || channel.Contribution != contributions[name] {
						t.Fatalf("%s: %+v", name, channel)
					}
					channel.Latency = 0
					flow.Channels[name] = channel
				}
				if !slices.Equal(flow.EntryPoints, []model.ID{representative.ID, duplicate.ID, source.ID}) ||
					len(database.semantic) != 2 || database.semantic[1].Context.ID != duplicate.ID || len(database.hops.Edges) != len(allEdges) {
					t.Fatal("접기·절단이 진입점 또는 저장 후보를 변경했다")
				}
				if execution == ExecutionSequential {
					baseline = flow
				} else if !reflect.DeepEqual(flow, baseline) {
					t.Fatal("병렬·순차 조립 결과가 다르다")
				}
			}
		})
	}
}

func TestFlowEmptyResultIsNotBudgetTruncation(t *testing.T) {
	service := testService(t, &fakeStore{}, ExecutionParallel)
	flow, err := service.Flow(t.Context(), Input{
		GraphID: testID(t, "019a0000-0000-7000-8000-000000000941"), WorkContext: "없는 결과", Scope: "local", Budget: 1,
	})
	if err != nil || len(flow.Contexts) != 0 || len(flow.Edges) != 0 || len(flow.EntryPoints) != 0 ||
		flow.BudgetUsed != 0 || flow.Truncation != nil || flow.HopBoundary != nil {
		t.Fatalf("빈 성공이 절단으로 표시됐다: %+v %v", flow, err)
	}
}

type flowPartialStore struct {
	partialEntryStore
	hops        model.HopResult
	graphFailed bool
}

func (database *flowPartialStore) SearchHopContextsFrom(context.Context, model.ID, []model.Context, int, string, []string, int) (model.HopResult, error) {
	if database.graphFailed {
		return database.hops, errors.New("그래프 장애")
	}
	return database.hops, nil
}

func TestFlowFourChannelPartialStates(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000951")
	values := make([]model.SearchCandidate, 3)
	for i := range values {
		values[i] = model.SearchCandidate{Context: testContext(t, graph, fmt.Sprintf("019a0000-0000-7000-8000-%012d", 952+i), "후보", i)}
	}
	extra := testContext(t, graph, "019a0000-0000-7000-8000-000000000955", "확장", 4)
	for mask := range 16 {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			for _, execution := range []Execution{ExecutionSequential, ExecutionParallel} {
				database := &flowPartialStore{
					partialEntryStore: partialEntryStore{Store: &fakeStore{}, values: values, mask: mask & 7},
					graphFailed:       mask&8 != 0,
					hops: model.HopResult{Contexts: []model.Context{extra}, Distances: map[model.ID]int{extra.ID: 1},
						Edges: []model.HopEdge{{FromID: values[0].Context.ID, ToID: extra.ID, Kind: "derived_from"}}, Truncated: true, Boundary: 1},
				}
				service, err := New(database, fakeEmbedder{}, Config{Execution: execution, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageRelations}, nil)
				if err != nil {
					t.Fatal(err)
				}
				flow, err := service.Flow(t.Context(), Input{GraphID: graph, WorkContext: "부분 실패", Scope: "local"})
				allFailed := mask&7 == 7
				if errors.Is(err, ErrAllChannelsFailed) != allFailed || err != nil && !allFailed {
					t.Fatalf("mask=%d: %v", mask, err)
				}
				graphSuccess := !allFailed && mask&8 == 0
				want := 3 - bits.OnesCount(uint(mask&7)) + boolInt(graphSuccess)
				if len(flow.Contexts) != want || flow.Truncation != nil || (flow.HopBoundary != nil) != graphSuccess {
					t.Fatalf("부분 상태: %+v", flow)
				}
				wantFailure := ""
				if allFailed {
					wantFailure = "no_entry_points"
				} else if !graphSuccess {
					wantFailure = "store_unavailable"
				}
				if flow.Channels["graph"].Failure != wantFailure || flow.Channels["graph"].Contribution != boolInt(graphSuccess) {
					t.Fatalf("그래프 상태: %+v", flow.Channels["graph"])
				}
				for i, name := range []string{"semantic", "keyword", "time"} {
					failed := mask&(1<<i) != 0
					if (flow.Channels[name].Failure != "") != failed || flow.Channels[name].Contribution != boolInt(!failed) {
						t.Fatalf("진입 채널 %s: %+v", name, flow.Channels[name])
					}
				}
				for _, item := range flow.Contexts {
					if item.Value.ID == extra.ID && !graphSuccess {
						t.Fatal("실패한 그래프의 부분 본문이 반환됐다")
					}
				}
				if (!graphSuccess || mask&1 != 0) && len(flow.Edges) != 0 {
					t.Fatal("실패·미반환 끝점의 간선이 반환됐다")
				}
			}
		})
	}
}
