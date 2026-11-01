package store

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
)

func TestGlobalSummaryEntryAsOfIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	grantAccount(t, database, graph, actor, model.GraphGradeOwner)
	now := time.Now().UTC()
	source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://global-asof/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	create := func(scope model.SummaryScope, from, to time.Time, evidence model.ID, supersedes model.ID) model.Context {
		t.Helper()
		value := testDerivedContext(t, graph, actor)
		value.Body = value.ID.String()
		value.Derived.Kind, value.Derived.SummaryScope = model.DerivationKindSummary, scope
		value.Derived.ValidFrom, value.Derived.ValidTo = new(from), new(to)
		var created model.Context
		var err error
		if supersedes.IsZero() {
			created, err = database.CreateContext(t.Context(), graph, value, []model.ID{evidence})
		} else {
			created, err = database.CreateSupersedingContextWithOperation(t.Context(), graph, value, []model.ID{evidence}, supersedes, nil, WriteLimits{})
		}
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	day := 24 * time.Hour
	expiredLocal := create(model.SummaryScopeLocal, now.Add(-60*day), now.Add(-10*day), source.ID, model.ID{})
	currentLocal := create(model.SummaryScopeLocal, now.Add(-10*day), now.Add(10*day), source.ID, model.ID{})
	futureLocal := create(model.SummaryScopeLocal, now.Add(10*day), now.Add(60*day), source.ID, model.ID{})
	expired := create(model.SummaryScopeGlobal, now.Add(-60*day), now.Add(-10*day), source.ID, model.ID{})
	current := create(model.SummaryScopeGlobal, now.Add(-10*day), now.Add(10*day), expiredLocal.ID, expired.ID)
	future := create(model.SummaryScopeGlobal, now.Add(10*day), now.Add(60*day), source.ID, model.ID{})
	deleted := create(model.SummaryScopeGlobal, now.Add(-10*day), now.Add(10*day), source.ID, model.ID{})
	if _, err := database.SetContextDeleted(t.Context(), graph, deleted.ID, actor, true, 0); err != nil {
		t.Fatal(err)
	}
	vector := make([]float64, embeddingDimension(t, database))
	vector[0] = 1
	for _, consistency := range []search.Consistency{"", search.ConsistencySnapshot} {
		for _, route := range []string{"global", "auto", "adaptive"} {
			t.Run(fmt.Sprintf("%s/%s", consistency, route), func(t *testing.T) {
				for _, sample := range []struct {
					name  string
					at    time.Time
					local model.ID
				}{
					{"past", now.Add(-30 * day), expiredLocal.ID}, {"current", now, currentLocal.ID}, {"future", now.Add(30 * day), futureLocal.ID}, {"omitted", time.Time{}, currentLocal.ID},
				} {
					t.Run(sample.name, func(t *testing.T) {
						recorder := &recordingEntryStore{Store: database, lists: map[string][]model.ID{}}
						cfg := search.Config{Execution: search.ExecutionParallel, CandidateLimit: 20, FoldThreshold: 0.9, GraphStage: search.GraphStageBaseline, Consistency: consistency}
						scope := route
						if route == "auto" {
							cfg.GlobalFallback = true
						}
						if route == "adaptive" {
							scope = "auto"
							cfg.GraphStage = search.GraphStageReferences
							cfg.AdaptiveRouting = true
						}
						service, err := search.New(recorder, entryEmbedder{vector: vector}, cfg, nil)
						if err != nil {
							t.Fatal(err)
						}
						flow, err := service.Flow(t.Context(), search.Input{GraphID: graph, WorkContext: "unmatchedquery" + graph.String(), Scope: scope, AsOf: sample.at, Budget: 10000, MaxHops: 1, MaxHopNodes: 20})
						if err != nil {
							t.Fatal(err)
						}
						if !slices.Equal(flow.EntryPoints, []model.ID{current.ID}) || flow.Route != search.RouteGlobal {
							t.Fatalf("전역 진입점=%v route=%s", flow.EntryPoints, flow.Route)
						}
						var ids []model.ID
						for _, value := range flow.Contexts {
							ids = append(ids, value.Value.ID)
						}
						if !slices.Contains(ids, expiredLocal.ID) {
							t.Fatal("만료된 근거의 홉 조회 예외를 없앴다")
						}
						for _, forbidden := range []model.ID{expired.ID, future.ID, deleted.ID} {
							if slices.Contains(ids, forbidden) {
								t.Fatalf("부적격 전역 요약 포함: %s", forbidden)
							}
						}
						if scope != "global" {
							timed := recorder.lists["time"]
							if !slices.Contains(timed, sample.local) {
								t.Fatalf("as_of 시간 후보=%v", timed)
							}
							for _, local := range []model.ID{expiredLocal.ID, currentLocal.ID, futureLocal.ID} {
								if local != sample.local && slices.Contains(timed, local) {
									t.Fatalf("다른 시점의 시간 후보=%s", local)
								}
							}
							if len(recorder.lists["semantic"]) != 0 || len(recorder.lists["keyword"]) != 0 {
								t.Fatal("표본에 관련 국소 후보가 생겼다")
							}
						}
					})
				}
			})
		}
	}
}
