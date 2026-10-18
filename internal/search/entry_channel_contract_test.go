package search

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type partialEntryStore struct {
	Store
	values []model.SearchCandidate
	mask   int
}

func (database partialEntryStore) candidates(channel int) ([]model.SearchCandidate, error) {
	if database.mask&(1<<channel) != 0 {
		return []model.SearchCandidate{database.values[channel]}, errors.New("채널 장애")
	}
	return []model.SearchCandidate{database.values[channel]}, nil
}
func (database partialEntryStore) SemanticCandidates(context.Context, model.ID, string, []float64, time.Time, int) ([]model.SearchCandidate, error) {
	return database.candidates(0)
}
func (database partialEntryStore) KeywordCandidates(context.Context, model.ID, string, time.Time, int) ([]model.SearchCandidate, error) {
	return database.candidates(1)
}
func (database partialEntryStore) TimeCandidates(context.Context, model.ID, time.Time, int) ([]model.SearchCandidate, error) {
	return database.candidates(2)
}

func TestEntryChannelsDiscardFailedPayloads(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000001")
	values := make([]model.SearchCandidate, 3)
	for i := range values {
		values[i] = model.SearchCandidate{Context: testContext(t, graph, fmt.Sprintf("019a0000-0000-7000-8000-%012d", i+2), "후보", i), Similarity: 1}
	}
	for mask := range 8 {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			var baseline Flow
			for _, execution := range []Execution{ExecutionSequential, ExecutionParallel} {
				database := partialEntryStore{Store: &fakeStore{}, values: values, mask: mask}
				service, err := New(database, fakeEmbedder{}, Config{Execution: execution, CandidateLimit: 3, FoldThreshold: 0.9}, nil)
				if err != nil {
					t.Fatal(err)
				}
				flow, err := service.Flow(t.Context(), Input{GraphID: graph, WorkContext: "채널", Scope: "local"})
				if errors.Is(err, ErrAllChannelsFailed) != (mask == 7) || err != nil && mask != 7 {
					t.Fatalf("전체 실패: %v", err)
				}
				for i, channel := range []string{"semantic", "keyword", "time"} {
					failed := mask&(1<<i) != 0
					metric := flow.Channels[channel]
					found := slices.ContainsFunc(flow.Contexts, func(value Context) bool { return value.Value.ID == values[i].Context.ID })
					if failed == found || failed && (metric.Failure != "store_unavailable" || metric.Candidates != 0 || metric.Contribution != 0) || !failed && (metric.Failure != "" || metric.Candidates != 1 || metric.Contribution != 1) {
						t.Fatalf("%s mask=%d: found=%t metric=%+v", channel, mask, found, metric)
					}
					metric.Latency = 0
					flow.Channels[channel] = metric
				}
				wantGraph := failureDisabled
				if mask == 7 {
					wantGraph = "no_entry_points"
				}
				if flow.Channels["graph"].Failure != wantGraph {
					t.Fatalf("확장 상태: %+v", flow.Channels["graph"])
				}
				if execution == ExecutionSequential {
					baseline = flow
				} else if !reflect.DeepEqual(baseline, flow) {
					t.Fatal("병렬·순차 결과가 다르다")
				}
			}
		})
	}
}

func TestEntryEmptySuccessAndFailedRelevance(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000001")
	service, err := New(&fakeStore{}, fakeEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 3, FoldThreshold: 0.9}, nil)
	if err != nil {
		t.Fatal(err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: graph, WorkContext: "빈 결과"})
	if err != nil || len(flow.Contexts) != 0 || flow.Channels["semantic"].Failure != "" || flow.Channels["keyword"].Failure != "" || flow.Channels["time"].Failure != "" || flow.Channels["graph"].Failure != "no_entry_points" {
		t.Fatalf("빈 성공: %+v %v", flow, err)
	}
	values := make([]model.SearchCandidate, 3)
	for i := range values {
		values[i] = model.SearchCandidate{Context: testContext(t, graph, fmt.Sprintf("019a0000-0000-7000-8000-%012d", i+2), "후보", i), Similarity: 1}
	}
	summary := testContext(t, graph, "019a0000-0000-7000-8000-000000000099", "전역", 4)
	database := partialEntryStore{Store: &fakeStore{global: []model.SearchCandidate{{Context: summary}}}, values: values, mask: 3}
	service, err = New(database, fakeEmbedder{}, Config{Execution: ExecutionParallel, CandidateLimit: 3, FoldThreshold: 0.9, GraphStage: GraphStageGlobal, GlobalFallback: true, SemanticThreshold: 0.5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	flow, err = service.Flow(t.Context(), Input{GraphID: graph, WorkContext: "관련성"})
	if err != nil || flow.Route != RouteGlobal || !slices.Equal(flow.EntryPoints, []model.ID{summary.ID}) {
		t.Fatalf("실패 후보가 라우팅에 섞였다: %+v %v", flow, err)
	}
}
