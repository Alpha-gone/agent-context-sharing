package store

import (
	"fmt"
	"math"
	"testing"

	"agent_context_sharing/internal/model"
)

// createHopChain은 그래프 크기를 바꿔 가며 홉 탐색을 재기 위한 표본이다. 묶음 하나는 원천
// 둘, 둘을 근거로 한 파생 하나와 둘을 구성원으로 한 사건 하나다. 사건은 다음 묶음과
// precedes로, 세 묶음 뒤와 relates_to로 확정 연결되어 홉이 늘수록 이웃이 넓어진다. 시작점은
// 가운데 묶음의 파생과 사건이다.
func createHopChain(b *testing.B, database *Store, groups int) (model.ID, []model.Context) {
	b.Helper()
	actorID := newTestID(b)
	createTestAccount(b, database, actorID)
	graphID := createTestGraph(b, database, actorID)
	events := make([]model.Context, 0, groups)
	var starts []model.Context
	for group := range groups {
		sources := make([]model.ID, 0, 2)
		for index := range 2 {
			locator := fmt.Sprintf("api://hop-bench/%s/%d/%d", graphID, group, index)
			source, err := database.CreateContext(b.Context(), graphID, testSourceContext(b, graphID, actorID, locator), nil)
			if err != nil {
				b.Fatalf("원천 생성: %v", err)
			}
			sources = append(sources, source.ID)
		}
		derived, err := database.CreateContext(b.Context(), graphID, testDerivedContext(b, graphID, actorID), sources)
		if err != nil {
			b.Fatalf("파생 생성: %v", err)
		}
		value := testEventContext(b, graphID, actorID, sources[0])
		value.Event.MemberIDs = sources
		event, err := database.CreateContext(b.Context(), graphID, value, nil)
		if err != nil {
			b.Fatalf("사건 생성: %v", err)
		}
		events = append(events, event)
		if group == groups/2 {
			starts = []model.Context{derived, event}
		}
	}
	for index := range events {
		for _, link := range []struct {
			kind   model.RelationType
			offset int
		}{{model.RelationTypePrecedes, 1}, {model.RelationTypeRelatesTo, 3}} {
			if index+link.offset >= len(events) {
				continue
			}
			relation := confirmable(b, graphID, link.kind, events[index].ID, events[index+link.offset].ID)
			if _, err := database.ConfirmRelation(b.Context(), graphID, relation, nil); err != nil {
				b.Fatalf("관계 확정: %v", err)
			}
		}
	}
	return graphID, starts
}

// BenchmarkHopImplementationsIntegration은 「홉 탐색 구현 비교」의 왕복 횟수와 지연을 잰다.
// 그래프 크기, 홉 수와 관계 유형 수를 바꾸고, 결과 동등성은
// TestHopImplementationsReturnSameResultIntegration이 따로 확인한다. 왕복은 확장 질의 수이며
// 두 구현이 공유하는 참조 채우기와 접근 시각 갱신은 뺀다. 홉 6은 플랜 기본 최대 홉 수다.
//
// 상한 없는 탐색은 기준선만 잰다. 가변 길이 간선 후보는 100노드 표본의 가운데에서 상한 없이
// 경로를 나열하다 데이터베이스 임시 파일로 디스크를 모두 채우고 실패했다.
//
//	TEST_DATABASE_URL=... go test -run '^$' -bench HopImplementations -benchtime 20x ./internal/store
func BenchmarkHopImplementationsIntegration(b *testing.B) {
	database := newIntegrationStore(b)
	implementations := []struct {
		name string
		run  func(model.ID, []model.Context, int, []string) (HopResult, error)
	}{
		{"bfs", func(graphID model.ID, starts []model.Context, hops int, filter []string) (HopResult, error) {
			return database.HopContextsFrom(b.Context(), graphID, starts, hops, "both", filter, 0)
		}},
		{"vle", func(graphID model.ID, starts []model.Context, hops int, filter []string) (HopResult, error) {
			return database.hopContextsPrefetched(b.Context(), graphID, starts, hops, "both", filter, 0)
		}},
	}
	filters := []struct {
		name   string
		filter []string
	}{{"labels=1", []string{"precedes"}}, {"labels=7", nil}}
	for _, groups := range []int{25, 100, 400} {
		graphID, starts := createHopChain(b, database, groups)
		for _, hops := range []int{1, 2, 4, 6, math.MaxInt} {
			hopName := fmt.Sprint(hops)
			if hops == math.MaxInt {
				hopName = "all"
			}
			for _, filter := range filters {
				for _, implementation := range implementations {
					if hops == math.MaxInt && implementation.name == "vle" {
						continue
					}
					name := fmt.Sprintf("nodes=%d/hops=%s/%s/%s", groups*4, hopName, filter.name, implementation.name)
					b.Run(name, func(b *testing.B) {
						var result HopResult
						for b.Loop() {
							var err error
							if result, err = implementation.run(graphID, starts, hops, filter.filter); err != nil {
								b.Fatalf("홉 탐색: %v", err)
							}
						}
						b.ReportMetric(float64(result.expansionQueries), "queries/op")
						b.ReportMetric(float64(len(result.Contexts)), "contexts/op")
					})
				}
			}
		}
	}
}
