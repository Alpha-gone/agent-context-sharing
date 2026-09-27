package search

import (
	"reflect"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func TestRouteSignalsDecideAppliesRulesInOrder(t *testing.T) {
	cases := map[string]struct {
		signals RouteSignals
		route   Route
		reason  string
	}{
		"관련 국소 후보 없음":  {RouteSignals{CrossChannelAgreement: true, SemanticCount: 2, SemanticTop: 0.9}, RouteGlobal, ReasonNoRelevantLocal},
		"채널 간 일치":      {RouteSignals{RelevantLocal: true, CrossChannelAgreement: true}, RouteDirect, ReasonCrossChannelAgreement},
		"의미 후보 분리":     {RouteSignals{RelevantLocal: true, SemanticCount: 2, SemanticTop: 0.8, SemanticSecond: 0.6}, RouteDirect, ReasonSemanticSeparation},
		"분리 폭 부족":      {RouteSignals{RelevantLocal: true, SemanticCount: 2, SemanticTop: 0.8, SemanticSecond: 0.75}, RouteLocal, ReasonAmbiguousLocal},
		"충분성 하한 미달":    {RouteSignals{RelevantLocal: true, SemanticCount: 2, SemanticTop: 0.6, SemanticSecond: 0.1}, RouteLocal, ReasonAmbiguousLocal},
		"의미 후보 하나뿐":    {RouteSignals{RelevantLocal: true, SemanticCount: 1, SemanticTop: 0.9}, RouteLocal, ReasonAmbiguousLocal},
		"의미 채널 실패·키워드": {RouteSignals{RelevantLocal: true}, RouteLocal, ReasonAmbiguousLocal},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			for range 2 {
				route, reason := test.signals.Decide(0.7, 0.1)
				if route != test.route || reason != test.reason {
					t.Fatalf("경로 = %s/%s, want %s/%s", route, reason, test.route, test.reason)
				}
			}
		})
	}
}

// routeFixture는 의미·키워드·시간 채널 후보와 한 홉 확장 결과를 가진 저장소를 만든다.
// 의미 1위 semanticTop과 키워드 후보 keywordOnly는 서로 다르므로 채널 간 일치가 없다.
type routeFixture struct {
	database     *fakeStore
	graphID      model.ID
	semanticTop  model.Context
	keywordOnly  model.Context
	timeOnly     model.Context
	neighbor     model.Context
	globalDigest model.Context
}

func newRouteFixture(t *testing.T, second float64) routeFixture {
	t.Helper()
	graphID := testID(t, "019a0000-0000-7000-8000-000000000301")
	fixture := routeFixture{
		graphID:      graphID,
		semanticTop:  testContext(t, graphID, "019a0000-0000-7000-8000-000000000302", "의미 1위", 1),
		keywordOnly:  testContext(t, graphID, "019a0000-0000-7000-8000-000000000303", "키워드", 2),
		timeOnly:     testContext(t, graphID, "019a0000-0000-7000-8000-000000000304", "시간", 9),
		neighbor:     testContext(t, graphID, "019a0000-0000-7000-8000-000000000305", "이웃", 4),
		globalDigest: testContext(t, graphID, "019a0000-0000-7000-8000-000000000306", "전역 요약", 5),
	}
	semanticSecond := testContext(t, graphID, "019a0000-0000-7000-8000-000000000307", "의미 2위", 3)
	fixture.database = &fakeStore{
		semantic: []store.SearchCandidate{{Context: fixture.semanticTop, Similarity: 0.9}, {Context: semanticSecond, Similarity: second}},
		keyword:  []store.SearchCandidate{{Context: fixture.keywordOnly}},
		time:     []store.SearchCandidate{{Context: fixture.timeOnly}},
		global:   []store.SearchCandidate{{Context: fixture.globalDigest}},
		hops: store.HopResult{
			Contexts:  []model.Context{fixture.semanticTop, fixture.neighbor},
			Distances: map[model.ID]int{fixture.semanticTop.ID: 0, fixture.neighbor.ID: 1},
			Edges:     []store.HopEdge{{FromID: fixture.semanticTop.ID, ToID: fixture.neighbor.ID, Kind: "relates_to"}},
		},
	}
	return fixture
}

func adaptiveService(t *testing.T, database Store, route Route, globalFallback bool) *Service {
	t.Helper()
	service, err := New(database, fakeEmbedder{}, Config{
		Execution: ExecutionSequential, CandidateLimit: 5, SemanticThreshold: 0.5, FoldThreshold: 0.9,
		GraphStage: GraphStageRelations, GlobalFallback: globalFallback,
		AdaptiveRouting: true, AdaptiveDirectThreshold: 0.7, AdaptiveMarginThreshold: 0.1, Route: route,
	}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	return service
}

func routeFlow(t *testing.T, service *Service, graphID model.ID, scope string) Flow {
	t.Helper()
	flow, err := service.Flow(t.Context(), Input{GraphID: graphID, WorkContext: "질의", AsOf: time.Now().UTC(), Budget: 0, Scope: scope, MaxHops: 4, MaxHopNodes: 10})
	if err != nil {
		t.Fatalf("흐름 검색: %v", err)
	}
	return flow
}

func TestFlowAdaptiveDirectSkipsGraphExpansion(t *testing.T) {
	fixture := newRouteFixture(t, 0.2)
	flow := routeFlow(t, adaptiveService(t, fixture.database, "", true), fixture.graphID, "auto")
	if flow.Route != RouteDirect || flow.RouteReason != ReasonSemanticSeparation {
		t.Fatalf("경로 = %s/%s", flow.Route, flow.RouteReason)
	}
	if fixture.database.hopCalls != 0 || fixture.database.globalCalls != 0 || flow.Channels["graph"].Failure != failureDisabled {
		t.Fatalf("직접 경로가 확장을 실행했다: hop %d, global %d, graph %+v", fixture.database.hopCalls, fixture.database.globalCalls, flow.Channels["graph"])
	}
	if !flow.RouteSignals.RelevantLocal || flow.RouteSignals.SemanticTop != 0.9 || flow.RouteSignals.SemanticSecond != 0.2 {
		t.Fatalf("라우터 신호 = %+v", flow.RouteSignals)
	}
}

func TestFlowAdaptiveLocalExpandsRelevantCandidatesOneHop(t *testing.T) {
	fixture := newRouteFixture(t, 0.85)
	flow := routeFlow(t, adaptiveService(t, fixture.database, "", true), fixture.graphID, "auto")
	if flow.Route != RouteLocal || flow.RouteReason != ReasonAmbiguousLocal {
		t.Fatalf("경로 = %s/%s", flow.Route, flow.RouteReason)
	}
	if fixture.database.hopDepth != 1 {
		t.Fatalf("국소 확장 깊이 = %d, want 1", fixture.database.hopDepth)
	}
	// 시간 필터 후보는 질의 관련성이 없어 확장 시작점에서 빠진다.
	for _, id := range fixture.database.hopStarts {
		if id == fixture.timeOnly.ID {
			t.Fatalf("시간 후보가 국소 확장 시작점에 들어갔다: %v", fixture.database.hopStarts)
		}
	}
	if len(fixture.database.hopStarts) != 3 {
		t.Fatalf("국소 확장 시작점 = %v", fixture.database.hopStarts)
	}
	if !slicesContainID(flowIDs(flow), fixture.neighbor.ID) {
		t.Fatalf("한 홉 이웃이 결과에 없다: %v", flowIDs(flow))
	}
}

func TestFlowAdaptiveGlobalIgnoresFallbackConfigAndFallsBackWithoutSummary(t *testing.T) {
	fixture := newRouteFixture(t, 0.2)
	fixture.database.semantic = []store.SearchCandidate{{Context: fixture.semanticTop, Similarity: 0.1}}
	fixture.database.keyword = nil
	// 기존 전역 전환을 꺼도 적응형 라우팅은 전역 진입을 직접 정한다.
	flow := routeFlow(t, adaptiveService(t, fixture.database, "", false), fixture.graphID, "auto")
	if flow.Route != RouteGlobal || flow.RouteReason != ReasonNoRelevantLocal {
		t.Fatalf("경로 = %s/%s", flow.Route, flow.RouteReason)
	}
	if got, want := fixture.database.hopStarts, []model.ID{fixture.globalDigest.ID}; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(fixture.database.hopFilters, []string{"derived_from"}) {
		t.Fatalf("전역 진입 시작점 = %v, 필터 %v", got, fixture.database.hopFilters)
	}

	fixture.database.global = nil
	fixture.database.hopCalls = 0
	flow = routeFlow(t, adaptiveService(t, fixture.database, "", false), fixture.graphID, "auto")
	if flow.Route != RouteDirect || flow.RouteReason != ReasonGlobalSummaryAbsent || fixture.database.hopCalls != 0 {
		t.Fatalf("요약 부재 되돌림 = %s/%s, hop %d", flow.Route, flow.RouteReason, fixture.database.hopCalls)
	}
}

// TestFlowExplicitScopeOverridesAdaptiveRouting은 호출자가 지정한 범위가 라우터와 강제
// 경로보다 우선하는지 확인한다.
func TestFlowExplicitScopeOverridesAdaptiveRouting(t *testing.T) {
	for _, forced := range []Route{"", RouteDirect} {
		fixture := newRouteFixture(t, 0.2)
		flow := routeFlow(t, adaptiveService(t, fixture.database, forced, true), fixture.graphID, "local")
		if flow.Route != RouteLocal || flow.RouteReason != ReasonExplicitScope || fixture.database.hopDepth != 4 || fixture.database.globalCalls != 0 {
			t.Fatalf("local 지정(강제 %q) = %s/%s, 깊이 %d, 전역 %d", forced, flow.Route, flow.RouteReason, fixture.database.hopDepth, fixture.database.globalCalls)
		}
		flow = routeFlow(t, adaptiveService(t, fixture.database, forced, true), fixture.graphID, "global")
		if flow.Route != RouteGlobal || flow.RouteReason != ReasonExplicitScope || fixture.database.globalCalls != 1 {
			t.Fatalf("global 지정(강제 %q) = %s/%s, 전역 %d", forced, flow.Route, flow.RouteReason, fixture.database.globalCalls)
		}
	}
}

func TestFlowForcedRouteReplacesRouterDecision(t *testing.T) {
	fixture := newRouteFixture(t, 0.2)
	flow := routeFlow(t, adaptiveService(t, fixture.database, RouteLocal, true), fixture.graphID, "auto")
	if flow.Route != RouteLocal || flow.RouteReason != ReasonForced || fixture.database.hopDepth != 1 {
		t.Fatalf("강제 경로 = %s/%s, 깊이 %d", flow.Route, flow.RouteReason, fixture.database.hopDepth)
	}
}

func TestFlowLegacyAutoLabelsExecutedRoute(t *testing.T) {
	fixture := newRouteFixture(t, 0.2)
	service, err := New(fixture.database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, SemanticThreshold: 0.5, FoldThreshold: 0.9, GraphStage: GraphStageRelations, GlobalFallback: true}, nil)
	if err != nil {
		t.Fatalf("검색기 생성: %v", err)
	}
	flow := routeFlow(t, service, fixture.graphID, "auto")
	if flow.Route != RouteLocal || flow.RouteReason != ReasonLegacyAuto || fixture.database.hopDepth != 4 || flow.RouteSignals != (RouteSignals{}) {
		t.Fatalf("기존 auto = %s/%s, 깊이 %d, 신호 %+v", flow.Route, flow.RouteReason, fixture.database.hopDepth, flow.RouteSignals)
	}
}

func TestNewRejectsInvalidAdaptiveConfig(t *testing.T) {
	base := Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, GraphStage: GraphStageRelations, AdaptiveRouting: true, AdaptiveDirectThreshold: 0.7, AdaptiveMarginThreshold: 0.1}
	cases := map[string]func(*Config){
		"baseline 조합":  func(config *Config) { config.GraphStage = GraphStageBaseline },
		"하한 범위":        func(config *Config) { config.AdaptiveDirectThreshold = 1.5 },
		"분리 폭 범위":      func(config *Config) { config.AdaptiveMarginThreshold = -0.1 },
		"적응형 없는 강제 경로": func(config *Config) { config.AdaptiveRouting, config.Route = false, RouteDirect },
		"알 수 없는 강제 경로": func(config *Config) { config.Route = "other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := New(&fakeStore{}, fakeEmbedder{}, config, nil); err == nil {
				t.Fatal("잘못된 적응형 구성을 받아들였다")
			}
		})
	}
}

func slicesContainID(ids []string, id model.ID) bool {
	for _, value := range ids {
		if value == id.String() {
			return true
		}
	}
	return false
}
