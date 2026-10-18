package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type routingContractStore struct {
	*fakeStore
	failures           int
	entryCalls         [3]atomic.Int32
	summaryErr, hopErr error
	hopLimit           int
	hopDirection       string
}

func (database *routingContractStore) entry(index int, values []model.SearchCandidate) ([]model.SearchCandidate, error) {
	database.entryCalls[index].Add(1)
	if database.failures&(1<<index) != 0 {
		return values, errors.New("진입 채널 장애")
	}
	return values, nil
}
func (database *routingContractStore) SemanticCandidates(context.Context, model.ID, string, []float64, time.Time, int) ([]model.SearchCandidate, error) {
	return database.entry(0, database.semantic)
}
func (database *routingContractStore) KeywordCandidates(context.Context, model.ID, string, time.Time, int) ([]model.SearchCandidate, error) {
	return database.entry(1, database.keyword)
}
func (database *routingContractStore) TimeCandidates(context.Context, model.ID, time.Time, int) ([]model.SearchCandidate, error) {
	return database.entry(2, database.time)
}
func (database *routingContractStore) GlobalSummaryCandidates(ctx context.Context, graph model.ID, at time.Time, limit int) ([]model.SearchCandidate, error) {
	values, _ := database.fakeStore.GlobalSummaryCandidates(ctx, graph, at, limit)
	return values, database.summaryErr
}
func (database *routingContractStore) SearchHopContextsFrom(ctx context.Context, graph model.ID, starts []model.Context, hops int, direction string, filters []string, limit int) (model.HopResult, error) {
	database.hopLimit, database.hopDirection = limit, direction
	values, _ := database.fakeStore.SearchHopContextsFrom(ctx, graph, starts, hops, direction, filters, limit)
	return values, database.hopErr
}

type routingContractEmbedder struct{ calls atomic.Int32 }

func (embedder *routingContractEmbedder) Embed(context.Context, string) ([]float64, error) {
	embedder.calls.Add(1)
	return []float64{1}, nil
}
func (*routingContractEmbedder) ModelID() string { return "test:vector:1" }

func routingConfig(execution Execution, stage GraphStage, adaptive, fallback bool) Config {
	return Config{Execution: execution, CandidateLimit: 5, SemanticThreshold: 0.5, FoldThreshold: 0.9,
		GraphStage: stage, GlobalFallback: fallback, AdaptiveRouting: adaptive,
		AdaptiveDirectThreshold: 0.875, AdaptiveMarginThreshold: 0.25}
}

func routingFixture(t *testing.T) (routeFixture, *routingContractStore) {
	t.Helper()
	fixture := newRouteFixture(t, 0.625)
	fixture.database.semantic[0].Similarity = 0.875
	fixture.globalDigest.Layer = model.LayerDerived
	fixture.globalDigest.Derived = &model.DerivedAttributes{
		Kind: model.DerivationKindSummary, SummaryScope: model.SummaryScopeGlobal,
		EvidenceState: model.EvidenceStateExperience, DerivedFrom: []model.ID{fixture.neighbor.ID},
	}
	fixture.database.global = []model.SearchCandidate{{Context: fixture.globalDigest}}
	return fixture, &routingContractStore{fakeStore: fixture.database}
}

func TestRoutingScopeAndConfigurationMatrix(t *testing.T) {
	for _, scope := range []string{"", "auto", "local", "global"} {
		for _, stage := range []GraphStage{GraphStageBaseline, GraphStageReferences, GraphStageRelations, GraphStageGlobal} {
			for _, adaptive := range []bool{false, true} {
				if adaptive && stage == GraphStageBaseline {
					continue
				}
				for _, fallback := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/adaptive_%t/fallback_%t", scope, stage, adaptive, fallback), func(t *testing.T) {
						var baseline Flow
						for _, execution := range []Execution{ExecutionSequential, ExecutionParallel} {
							fixture, database := routingFixture(t)
							cfg := routingConfig(execution, stage, adaptive, fallback)
							// 명시 scope는 평가용 강제 경로보다도 우선한다.
							if adaptive && (scope == "local" || scope == "global") {
								cfg.Route = RouteDirect
							}
							embedder := &routingContractEmbedder{}
							service, err := New(database, embedder, cfg, nil)
							if err != nil {
								t.Fatal(err)
							}
							flow, err := service.Flow(t.Context(), Input{GraphID: fixture.graphID, WorkContext: "질의", Scope: scope, MaxHops: 3, MaxHopNodes: 2})
							if err != nil {
								t.Fatal(err)
							}
							wantRoute, wantReason, calls, depth := RouteLocal, ReasonExplicitScope, 1, 3
							switch scope {
							case "global":
								wantRoute = RouteGlobal
							case "local":
								if stage == GraphStageBaseline {
									wantRoute, calls = RouteDirect, 0
								}
							default:
								wantReason = ReasonLegacyAuto
								if adaptive {
									wantRoute, wantReason, calls = RouteDirect, ReasonSemanticSeparation, 0
								} else if stage == GraphStageBaseline {
									wantRoute, calls = RouteDirect, 0
								}
							}
							if flow.Route != wantRoute || flow.RouteReason != wantReason || database.hopCalls != calls {
								t.Fatalf("경로=%s/%s hop=%d, want %s/%s/%d", flow.Route, flow.RouteReason, database.hopCalls, wantRoute, wantReason, calls)
							}
							wantEntryCalls, wantSummaryCalls := int32(1), 0
							if scope == "global" {
								wantEntryCalls, wantSummaryCalls = 0, 1
								if !slices.Equal(database.hopStarts, []model.ID{fixture.globalDigest.ID}) || !slices.Equal(database.hopFilters, []string{"derived_from"}) {
									t.Fatal("명시 global이 요약 이외의 시작점이나 필터를 사용했다")
								}
							}
							if embedder.calls.Load() != wantEntryCalls || database.globalCalls != wantSummaryCalls {
								t.Fatal("scope 밖 채널을 호출했다")
							}
							for i := range database.entryCalls {
								if database.entryCalls[i].Load() != wantEntryCalls {
									t.Fatal("진입 채널 호출 수가 다르다")
								}
							}
							if calls == 1 && (database.hopDepth != depth || database.hopLimit != 2 || database.hopDirection != "both") {
								t.Fatal("홉 요청 경계를 잃었다")
							}
							if calls == 0 && flow.Channels["graph"].Failure != failureDisabled {
								t.Fatal("미실행 그래프를 장애로 표시했다")
							}
							if !adaptive || scope == "local" || scope == "global" {
								if flow.RouteSignals != (model.RouteSignals{}) {
									t.Fatal("명시 scope/기준 auto가 적응형 신호를 만들었다")
								}
							}
							for name, channel := range flow.Channels {
								channel.Latency = 0
								flow.Channels[name] = channel
							}
							if execution == ExecutionSequential {
								baseline = flow
							} else if !reflect.DeepEqual(baseline, flow) {
								t.Fatal("실행 방식이 라우팅을 변경했다")
							}
						}
					})
				}
			}
		}
	}
}

func TestRoutingLegacyAndAdaptiveGlobalEntry(t *testing.T) {
	for _, stage := range []GraphStage{GraphStageBaseline, GraphStageReferences, GraphStageRelations, GraphStageGlobal} {
		for _, adaptive := range []bool{false, true} {
			if adaptive && stage == GraphStageBaseline {
				continue
			}
			for _, fallback := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/adaptive_%t/fallback_%t", stage, adaptive, fallback), func(t *testing.T) {
					for _, execution := range []Execution{ExecutionSequential, ExecutionParallel} {
						fixture, database := routingFixture(t)
						database.keyword = nil
						database.semantic[0].Similarity, database.semantic[1].Similarity = 0.1, 0.05
						cfg := routingConfig(execution, stage, adaptive, fallback)
						service, err := New(database, fakeEmbedder{}, cfg, nil)
						if err != nil {
							t.Fatal(err)
						}
						flow, err := service.Flow(t.Context(), Input{GraphID: fixture.graphID, WorkContext: "전역 질의", MaxHops: 4, MaxHopNodes: 1})
						if err != nil {
							t.Fatal(err)
						}
						global := adaptive || fallback
						wantRoute, wantReason := RouteLocal, ReasonLegacyAuto
						if stage == GraphStageBaseline {
							wantRoute = RouteDirect
						}
						if global {
							wantRoute = RouteGlobal
							if adaptive {
								wantReason = ReasonNoRelevantLocal
							}
							if !slices.Equal(database.hopStarts, []model.ID{fixture.globalDigest.ID}) || !slices.Equal(database.hopFilters, []string{"derived_from"}) {
								t.Fatal("전역 전환에 시간 후보가 섞였다")
							}
						}
						if flow.Route != wantRoute || flow.RouteReason != wantReason {
							t.Fatalf("경로: %s/%s", flow.Route, flow.RouteReason)
						}
						if database.globalCalls != boolInt(global) {
							t.Fatal("전역 조회 disabled와 실행을 혼동했다")
						}
						if !global && flow.Channels["global_summary"].Failure != failureDisabled {
							t.Fatal("구성 비활성을 실패로 표시했다")
						}
						if !slices.Contains(flowIDs(flow), fixture.timeOnly.ID.String()) {
							t.Fatal("시간 후보를 최종 결합에서 제외했다")
						}
					}
				})
			}
		}
	}
}

func TestRoutingAdaptiveDecisionAndFailureBoundaries(t *testing.T) {
	for _, name := range []string{"separation_equal", "direct_below", "margin_below", "single_semantic", "agreement", "event", "semantic_failed", "no_relevance", "summary_absent", "summary_failed", "all_failed", "graph_failed"} {
		t.Run(name, func(t *testing.T) {
			var baseline Flow
			for _, execution := range []Execution{ExecutionSequential, ExecutionParallel} {
				fixture, database := routingFixture(t)
				cfg := routingConfig(execution, GraphStageRelations, true, false)
				wantRoute, wantReason, hops, globals := RouteDirect, ReasonSemanticSeparation, 0, 0
				switch name {
				case "direct_below":
					cfg.AdaptiveDirectThreshold = math.Nextafter(0.875, 1)
				case "margin_below":
					cfg.AdaptiveMarginThreshold = math.Nextafter(0.25, 1)
				case "single_semantic":
					database.semantic = database.semantic[:1]
				case "agreement", "event":
					database.time = nil
					if name == "event" {
						database.semantic[0].Context.Layer = model.LayerEvent
					}
					database.keyword = []model.SearchCandidate{database.semantic[0]}
				case "semantic_failed":
					database.failures = 1
				case "no_relevance", "summary_absent", "summary_failed":
					database.keyword = nil
					database.semantic[0].Similarity, database.semantic[1].Similarity = 0.1, 0.05
					wantRoute, wantReason, hops, globals = RouteGlobal, ReasonNoRelevantLocal, 1, 1
					if name != "no_relevance" {
						wantRoute, wantReason, hops = RouteDirect, ReasonGlobalSummaryAbsent, 0
						if name == "summary_absent" {
							database.global = nil
						} else {
							database.summaryErr = errors.New("요약 조회 장애")
						}
					}
				case "all_failed":
					database.failures = 7
				case "graph_failed":
					database.hopErr = errors.New("홉 장애")
					cfg.AdaptiveDirectThreshold = 1
				}
				switch name {
				case "direct_below", "margin_below", "single_semantic", "semantic_failed", "graph_failed":
					wantRoute, wantReason, hops = RouteLocal, ReasonAmbiguousLocal, 1
				case "agreement":
					wantReason = ReasonCrossChannelAgreement
				case "event":
					wantRoute, wantReason, hops = RouteLocal, ReasonEventAnchor, 1
				case "all_failed":
					wantRoute, wantReason = "", ""
				}
				service, err := New(database, fakeEmbedder{}, cfg, nil)
				if err != nil {
					t.Fatal(err)
				}
				flow, err := service.Flow(t.Context(), Input{GraphID: fixture.graphID, WorkContext: "질의", Scope: "auto", MaxHops: 4})
				if errors.Is(err, ErrAllChannelsFailed) != (name == "all_failed") || err != nil && name != "all_failed" {
					t.Fatal(err)
				}
				if flow.Route != wantRoute || flow.RouteReason != wantReason || database.hopCalls != hops || database.globalCalls != globals {
					t.Fatalf("경로: %s/%s hop=%d global=%d", flow.Route, flow.RouteReason, database.hopCalls, database.globalCalls)
				}
				if wantRoute == RouteLocal && database.hopDepth != 1 {
					t.Fatal("적응형 국소 확장이 한 홉을 초과했다")
				}
				switch name {
				case "summary_absent":
					if flow.Channels["global_summary"].Failure != "" {
						t.Fatal("요약 부재가 장애가 됐다")
					}
				case "summary_failed":
					if flow.Channels["global_summary"].Failure != "store_unavailable" || slices.Contains(flowIDs(flow), fixture.globalDigest.ID.String()) {
						t.Fatal("실패한 요약 후보가 반환됐다")
					}
				case "semantic_failed":
					if flow.RouteSignals.SemanticCount != 0 || flow.RouteSignals.SemanticTop != 0 || flow.Channels["semantic"].Failure != "store_unavailable" {
						t.Fatal("실패한 의미 후보를 라우팅에 사용했다")
					}
				case "all_failed":
					if flow.RouteSignals != (model.RouteSignals{}) || flow.Channels["graph"].Failure != "no_entry_points" {
						t.Fatal("전체 진입 실패가 전역 전환에 감춰졌다")
					}
				case "graph_failed":
					if flow.Channels["graph"].Failure != "store_unavailable" || slices.Contains(flowIDs(flow), fixture.neighbor.ID.String()) {
						t.Fatal("실패한 홉 후보가 반환됐다")
					}
				}
				for channelName, channel := range flow.Channels {
					channel.Latency = 0
					flow.Channels[channelName] = channel
				}
				if execution == ExecutionSequential {
					baseline = flow
				} else if !reflect.DeepEqual(baseline, flow) {
					t.Fatal("라우팅·부분 상태가 실행 방식에 의존한다")
				}
			}
		})
	}
}

func TestRoutingLocalStartFilterAndSafeLog(t *testing.T) {
	fixture, database := routingFixture(t)
	database.semantic[1].Similarity = 0.1
	cfg := routingConfig(ExecutionParallel, GraphStageReferences, true, false)
	cfg.AdaptiveDirectThreshold = 1
	var output bytes.Buffer
	service, err := New(database, fakeEmbedder{}, cfg, slog.New(slog.NewTextHandler(&output, nil)))
	if err != nil {
		t.Fatal(err)
	}
	flow, err := service.Flow(t.Context(), Input{GraphID: fixture.graphID, WorkContext: "private_query_text", Scope: "auto", MaxHops: 5, MaxHopNodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if flow.Route != RouteLocal || !slices.Equal(database.hopStarts, []model.ID{fixture.keywordOnly.ID, fixture.semanticTop.ID}) ||
		!slices.Equal(database.hopFilters, []string{"derived_from", "supersedes", "has_member"}) || database.hopDepth != 1 {
		t.Fatal("관련성·종류·깊이의 국소 확장 경계가 다르다")
	}
	if !slices.Contains(flowIDs(flow), fixture.timeOnly.ID.String()) {
		t.Fatal("시간 후보가 결합에서도 사라졌다")
	}
	log := output.String()
	if !strings.Contains(log, "selected_route=local") || !strings.Contains(log, "route_reason=ambiguous_local") {
		t.Fatal("라우팅 근거를 기록하지 않았다")
	}
	for _, private := range []string{"private_query_text", fixture.semanticTop.Body, fixture.keywordOnly.Body, fixture.neighbor.Body, fixture.graphID.String()} {
		if strings.Contains(log, private) {
			t.Fatal("성공 로그에 질의·본문·그래프 원문이 노출됐다")
		}
	}
}

func TestRoutingSummaryAbsenceFailureAndForcedRoute(t *testing.T) {
	for _, scope := range []string{"auto", "global"} {
		for _, adaptive := range []bool{false, true} {
			for _, state := range []string{"empty", "failed"} {
				t.Run(fmt.Sprintf("%s/adaptive_%t/%s", scope, adaptive, state), func(t *testing.T) {
					fixture, database := routingFixture(t)
					database.semantic, database.keyword = nil, nil
					if state == "empty" {
						database.global = nil
					} else {
						database.summaryErr = errors.New("요약 조회 장애")
					}
					cfg := routingConfig(ExecutionParallel, GraphStageRelations, adaptive, true)
					service, err := New(database, fakeEmbedder{}, cfg, nil)
					if err != nil {
						t.Fatal(err)
					}
					flow, err := service.Flow(t.Context(), Input{GraphID: fixture.graphID, WorkContext: "질의", Scope: scope})
					wantError := scope == "global" && state == "failed"
					if errors.Is(err, ErrAllChannelsFailed) != wantError || err != nil && !wantError {
						t.Fatal(err)
					}
					wantFailure := ""
					if state == "failed" {
						wantFailure = "store_unavailable"
					}
					if flow.Channels["global_summary"].Failure != wantFailure || flow.Channels["global_summary"].Candidates != 0 ||
						slices.Contains(flowIDs(flow), fixture.globalDigest.ID.String()) || flow.Truncation != nil {
						t.Fatal("빈 요약·장애·절단을 혼동했다")
					}
					if scope == "global" {
						if database.hopCalls != 0 || len(flow.Contexts) != 0 || !wantError && flow.Channels["graph"].Failure != "no_entry_points" {
							t.Fatal("명시 global이 국소 후보로 돌아갔다")
						}
					} else if adaptive {
						if flow.Route != RouteDirect || flow.RouteReason != ReasonGlobalSummaryAbsent || database.hopCalls != 0 ||
							flow.Channels["graph"].Failure != failureDisabled {
							t.Fatal("적응형 요약 부재가 직접 경로로 돌아가지 않았다")
						}
					} else if flow.Route != RouteLocal || flow.RouteReason != ReasonLegacyAuto || !slices.Equal(database.hopStarts, []model.ID{fixture.timeOnly.ID}) {
						t.Fatal("기준 auto가 국소 결과를 보존하지 않았다")
					}
				})
			}
		}
	}
	for _, forced := range []Route{RouteDirect, RouteLocal, RouteGlobal} {
		t.Run(string(forced), func(t *testing.T) {
			fixture, database := routingFixture(t)
			cfg := routingConfig(ExecutionSequential, GraphStageRelations, true, false)
			cfg.Route = forced
			service, err := New(database, fakeEmbedder{}, cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			flow, err := service.Flow(t.Context(), Input{GraphID: fixture.graphID, WorkContext: "질의", Scope: "auto"})
			if err != nil || flow.Route != forced || flow.RouteReason != ReasonForced {
				t.Fatalf("강제 경로: %+v %v", flow, err)
			}
			if forced == RouteGlobal && !slices.Equal(database.hopStarts, []model.ID{fixture.globalDigest.ID}) {
				t.Fatal("강제 전역 경로가 국소 후보를 시작점으로 썼다")
			}
		})
	}
}

func TestRoutingThresholdValidation(t *testing.T) {
	for _, field := range []string{"semantic", "direct", "margin"} {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, math.Nextafter(1, 2), 0, 1} {
			t.Run(fmt.Sprintf("%s/%g", field, value), func(t *testing.T) {
				cfg := routingConfig(ExecutionSequential, GraphStageRelations, true, false)
				switch field {
				case "semantic":
					cfg.SemanticThreshold = value
				case "direct":
					cfg.AdaptiveDirectThreshold = value
				case "margin":
					cfg.AdaptiveMarginThreshold = value
				}
				_, err := New(&fakeStore{}, fakeEmbedder{}, cfg, nil)
				valid := value == 0 || value == 1
				if (err == nil) != valid {
					t.Fatalf("%s=%g error=%v", field, value, err)
				}
			})
		}
	}
	// 비활성 전용 임계값은 기준 auto에 영향을 주지 않는다.
	cfg := routingConfig(ExecutionSequential, GraphStageBaseline, false, true)
	cfg.AdaptiveDirectThreshold, cfg.AdaptiveMarginThreshold = math.NaN(), math.NaN()
	if _, err := New(&fakeStore{}, fakeEmbedder{}, cfg, nil); err != nil {
		t.Fatal(err)
	}
}
