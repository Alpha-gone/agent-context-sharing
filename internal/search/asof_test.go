package search

import (
	"context"
	"fmt"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type asOfStore struct {
	Store
	called bool
	asOf   time.Time
}

func (database *asOfStore) TimeCandidates(ctx context.Context, graph model.ID, asOf time.Time, limit int) ([]store.SearchCandidate, error) {
	database.called, database.asOf = true, asOf
	return database.Store.TimeCandidates(ctx, graph, asOf, limit)
}

type asOfEmbedder struct{ called bool }

func (embedder *asOfEmbedder) Embed(context.Context, string) ([]float64, error) {
	embedder.called = true
	return []float64{1}, nil
}
func (*asOfEmbedder) ModelID() string { return "test" }

func TestFlowValidatesAsOfBeforeSearching(t *testing.T) {
	at := time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)
	for name, value := range map[string]struct {
		at    time.Time
		valid bool
	}{
		"UTC":      {at, true},
		"미지정":      {time.Time{}, true},
		"양수 오프셋":   {at.In(time.FixedZone("KST", 9*60*60)), false},
		"음수 오프셋":   {at.In(time.FixedZone("west", -5*60*60)), false},
		"별도 영 오프셋": {at.In(time.FixedZone("zero", 0)), false},
	} {
		t.Run(name, func(t *testing.T) {
			database := &asOfStore{Store: &fakeStore{}}
			embedder := &asOfEmbedder{}
			service, err := New(database, embedder, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9}, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now().UTC()
			_, err = service.Flow(t.Context(), Input{GraphID: testID(t, "019a0000-0000-7000-8000-000000000001"), WorkContext: "현재 작업", AsOf: value.at})
			if !value.valid {
				if err == nil {
					t.Fatal("UTC가 아닌 기준 시각을 허용했다")
				}
				if database.called || embedder.called {
					t.Fatal("잘못된 시각이 검색 또는 임베딩 호출까지 전달됐다")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !database.called || !embedder.called || database.asOf.Location() != time.UTC {
				t.Fatalf("UTC 검색 입력 = %v", database.asOf)
			}
			if value.at.IsZero() {
				if database.asOf.Before(before) || database.asOf.After(time.Now().UTC()) {
					t.Fatal("미지정 기준 시각을 현재 시각으로 채우지 않았다")
				}
			} else if database.asOf != value.at {
				t.Fatal("명시한 UTC 기준 시각을 변경했다")
			}
		})
	}
}

type entryClockStore struct {
	*fakeStore
	times map[string]time.Time
}

func (database *entryClockStore) recordTime(channel string, at time.Time) {
	database.mu.Lock()
	defer database.mu.Unlock()
	database.times[channel] = at
}
func (database *entryClockStore) SemanticCandidates(ctx context.Context, graph model.ID, modelID string, vector []float64, at time.Time, limit int) ([]model.SearchCandidate, error) {
	database.recordTime("semantic", at)
	return database.fakeStore.SemanticCandidates(ctx, graph, modelID, vector, at, limit)
}
func (database *entryClockStore) KeywordCandidates(ctx context.Context, graph model.ID, query string, at time.Time, limit int) ([]model.SearchCandidate, error) {
	database.recordTime("keyword", at)
	return database.fakeStore.KeywordCandidates(ctx, graph, query, at, limit)
}
func (database *entryClockStore) TimeCandidates(ctx context.Context, graph model.ID, at time.Time, limit int) ([]model.SearchCandidate, error) {
	database.recordTime("time", at)
	return database.fakeStore.TimeCandidates(ctx, graph, at, limit)
}
func (database *entryClockStore) GlobalSummaryCandidates(ctx context.Context, graph model.ID, at time.Time, limit int) ([]model.SearchCandidate, error) {
	database.recordTime("global", at)
	return database.fakeStore.GlobalSummaryCandidates(ctx, graph, at, limit)
}

func TestGlobalSummaryUsesCurrentChannelClock(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-00000000d001")
	for _, execution := range []Execution{ExecutionSequential, ExecutionParallel} {
		for _, route := range []string{"global", "auto", "adaptive"} {
			for _, offset := range []time.Duration{-30 * 24 * time.Hour, 30 * 24 * time.Hour} {
				t.Run(fmt.Sprintf("%s/%s/%s", execution, route, offset), func(t *testing.T) {
					summary := testContext(t, graph, "019a0000-0000-7000-8000-00000000d002", "전역 요약", 1)
					database := &entryClockStore{fakeStore: &fakeStore{global: []model.SearchCandidate{{Context: summary}}}, times: map[string]time.Time{}}
					cfg := Config{Execution: execution, CandidateLimit: 5, FoldThreshold: 0.9, GlobalFallback: true, GraphStage: GraphStageReferences}
					scope := route
					if route == "adaptive" {
						scope = "auto"
						cfg.AdaptiveRouting = true
						cfg.Route = RouteGlobal
					}
					service, err := New(database, fakeEmbedder{}, cfg, nil)
					if err != nil {
						t.Fatal(err)
					}
					asOf := time.Now().UTC().Add(offset)
					before := time.Now().UTC()
					flow, err := service.Flow(t.Context(), Input{GraphID: graph, WorkContext: "질의", Scope: scope, AsOf: asOf, Budget: 100, MaxHops: 1, MaxHopNodes: 10})
					if err != nil {
						t.Fatal(err)
					}
					current := database.times["global"]
					if current.Before(before) || current.After(time.Now().UTC()) || current.Location() != time.UTC {
						t.Fatalf("전역 요약 기준이 현재 UTC가 아니다: %v", current)
					}
					if len(flow.EntryPoints) != 1 || flow.EntryPoints[0] != summary.ID {
						t.Fatalf("전역 진입점=%v", flow.EntryPoints)
					}
					if scope != "global" {
						if !database.times["semantic"].Equal(current) || !database.times["keyword"].Equal(current) || !database.times["time"].Equal(asOf) {
							t.Fatalf("채널 시각 분리 실패: %v", database.times)
						}
					}
				})
			}
		}
	}
}
