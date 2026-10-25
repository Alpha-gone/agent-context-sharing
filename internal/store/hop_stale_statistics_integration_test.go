package store

import (
	"fmt"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func TestHopStaleStatisticsHubIntegration(t *testing.T) {
	s := newIsolatedIntegrationStore(t)
	// 통계 고정은 이 시험이 생성한 DB에만 적용한다.
	for _, label := range []string{"Context", "DERIVED_FROM"} {
		if _, err := s.pool.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{s.graphName, label}.Sanitize()+" SET (autovacuum_enabled = false)"); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		createHopSample(t, s, fmt.Sprintf("stale-statistics-%d", i))
	}
	createHopHub(t, s, 2)
	analyze := func() {
		t.Helper()
		for _, label := range []string{"Context", "DERIVED_FROM"} {
			if _, err := s.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{s.graphName, label}.Sanitize()); err != nil {
				t.Fatal(err)
			}
		}
	}
	analyze()
	assertSmallStatistics := func() {
		t.Helper()
		for label, want := range map[string]int{"Context": 39, "DERIVED_FROM": 17} {
			var rows int
			if err := s.pool.QueryRow(t.Context(), "SELECT reltuples::int FROM pg_class WHERE oid = $1::regclass", pgx.Identifier{s.graphName, label}.Sanitize()).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != want {
				t.Fatalf("%s 통계가 갱신되었다: %d, 기대 %d", label, rows, want)
			}
		}
	}
	assertSmallStatistics()
	graph, hub := createHopHub(t, s, 20000)
	assertSmallStatistics()
	before := s.pool.Stat().AcquiredConns()
	accepted := make(map[int]HopResult)
	for _, stage := range []string{"stale", "analyzed"} {
		if stage == "analyzed" {
			analyze()
		}
		for _, limit := range []int{2, 10} {
			for _, direction := range []string{"both", "in"} {
				t.Run(fmt.Sprintf("%s/%s/%d", stage, direction, limit), func(t *testing.T) {
					started := time.Now()
					result, err := s.HopContextsFrom(t.Context(), graph, []model.Context{hub}, 2, direction, nil, limit)
					elapsed := time.Since(started)
					if err != nil {
						t.Fatal(err)
					}
					if len(result.Contexts) != limit || len(result.Edges) != limit-1 || !result.Truncated || result.Boundary != 1 {
						t.Fatalf("상한·간선·절단: nodes=%d edges=%d truncated=%v boundary=%d", len(result.Contexts), len(result.Edges), result.Truncated, result.Boundary)
					}
					if expected, exists := accepted[limit]; exists {
						compareHopResults(t, stage, expected, result)
					} else {
						accepted[limit] = result
					}
					query, err := s.hopCandidateSQL(graph, []model.ID{hub.ID}, []model.ID{hub.ID}, traversalLabel{label: "DERIVED_FROM"}, direction, limit-1)
					if err != nil {
						t.Fatal(err)
					}
					plan := explainHop(t, s, query)
					if scanned := plan.Root.scanned("DERIVED_FROM"); scanned > 1000 {
						t.Fatalf("후보마다 허브 간선을 반복 조회했다: %g행", scanned)
					}
					t.Logf("request=%v candidate=%.3fms edges=%.0f vertices=%.0f buffers=%d+%d", elapsed, plan.Execution, plan.Root.scanned("DERIVED_FROM"), plan.Root.scanned("Context"), plan.Root.Hit, plan.Root.Read)
					waitHopResources(t, s, nil, before)
				})
				if t.Failed() {
					return
				}
			}
		}
		if stage == "stale" {
			assertSmallStatistics()
		}
	}
	for _, limit := range []int{2, 10} {
		full, err := hopFullBaseline(t.Context(), s, graph, []model.Context{hub}, 2, "both", nil, limit)
		if err != nil {
			t.Fatal(err)
		}
		compareHopResults(t, "전체 본문 기준선", full, accepted[limit])
	}
}
