package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type search10Sample struct {
	graph     model.ID
	starts    []model.Context
	hops      int
	direction string
	filter    []string
	limit     int
	vle       bool
}

func search10Fixture(t testing.TB, s *Store, name string) search10Sample {
	t.Helper()
	sample := search10Sample{hops: 1, direction: "in", filter: []string{"derived_from"}, limit: 10}
	switch name {
	case "hub1000", "hub20000":
		size := 1000
		if name == "hub20000" {
			size = 20000
		}
		graph, hub := createHopHub(t, s, size)
		sample.graph, sample.starts = graph, []model.Context{hub}
	case "sparse100":
		sample.graph, sample.starts = createHopChain(t, s, 25)
		sample.hops, sample.direction, sample.filter, sample.vle = 2, "both", nil, true
	case "deleted-proposed-multi":
		mixed := createHopSample(t, s, "search10")
		sample.graph = mixed.graphID
		sample.starts = []model.Context{mixed.nodes["e3"], mixed.nodes["s1"], mixed.nodes["e3"]}
		sample.hops, sample.direction, sample.filter, sample.limit, sample.vle = 2, "both", nil, 6, true
	case "dense16":
		graph, source := createHopHub(t, s, 0)
		events := make([]model.Context, 0, 16)
		for range 16 {
			value := testEventContext(t, graph, source.CreatedBy, source.ID)
			value.Body = strings.Repeat("조밀한 사건🙂 ", 100)
			event, err := s.CreateContext(t.Context(), graph, value, nil)
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		var properties []string
		for i, from := range events {
			for _, to := range events[i+1:] {
				properties = append(properties, encodedHopRelation(t, graph, model.RelationTypeRelatesTo, from.ID, to.ID, model.RelationStateConfirmed))
			}
		}
		query := "UNWIND [" + strings.Join(properties, ",") + "] AS props MATCH (a:Context), (b:Context)" +
			" WHERE a.graph_id = " + cypherString(graph.String()) + " AND b.graph_id = " + cypherString(graph.String()) +
			" AND a.context_id = props.from_context_id AND b.context_id = props.to_context_id" +
			" CREATE (a)-[edge:RELATES_TO]->(b) SET edge = props RETURN count(edge)"
		var count string
		if err := s.pool.QueryRow(t.Context(), s.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil || count != "120" {
			t.Fatalf("조밀한 표본: count=%s err=%v", count, err)
		}
		sample.graph, sample.starts = graph, []model.Context{events[15], events[0], events[15]}
		sample.direction, sample.filter, sample.vle = "both", []string{"relates_to"}, true
	default:
		t.Fatalf("알 수 없는 표본 %s", name)
	}
	for _, label := range []string{"Context", "DERIVED_FROM", "HAS_MEMBER", "SUPERSEDES", "PRECEDES", "CAUSES", "PART_OF", "RELATES_TO"} {
		if _, err := s.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{s.graphName, label}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	return sample
}

// 같은 시작점 재조회·읽기 전용 스냅숏·접근 기록 경계로 알고리즘만 비교한다.
// VLE의 조밀/허브 무제한 경로 나열은 별도 자원 제한 없이 실행하지 않는다.
func runSearch10Hop(ctx context.Context, s *Store, sample search10Sample, implementation string) (result HopResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, hopWorkTimeout)
	defer cancel()
	ctx, release, err := s.enterHopReadScope(ctx)
	if err != nil {
		return HopResult{}, err
	}
	defer func() {
		if end := release(); end != nil {
			result, err = HopResult{}, errors.Join(err, end)
		}
	}()
	ids := make([]model.ID, len(sample.starts))
	for i, value := range sample.starts {
		ids[i] = value.ID
	}
	values, err := s.contextVerticesByIDs(ctx, s.reader(ctx), sample.graph, ids)
	if err != nil {
		return HopResult{}, err
	}
	starts := make([]model.Context, len(ids))
	for i, id := range ids {
		value, found := values[id]
		if !found || value.DeletedAt != nil {
			return HopResult{}, fmt.Errorf("시작점 %s 없음", id)
		}
		starts[i] = value
	}
	switch implementation {
	case "full":
		return hopFullBaseline(ctx, s, sample.graph, starts, sample.hops, sample.direction, sample.filter, sample.limit)
	case "bounded":
		return s.HopContextsFrom(ctx, sample.graph, starts, sample.hops, sample.direction, sample.filter, sample.limit)
	case "vle":
		return s.hopContextsPrefetched(ctx, sample.graph, starts, sample.hops, sample.direction, sample.filter, sample.limit)
	default:
		return HopResult{}, fmt.Errorf("알 수 없는 구현 %s", implementation)
	}
}

type search10TraceKey struct{}
type search10Query struct {
	sql  string
	args []any
	rows int64
}
type search10Trace []search10Query
type search10Tracer struct{}
type search10QueryKey struct{}

func (search10Tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if trace, ok := ctx.Value(search10TraceKey{}).(*search10Trace); ok {
		*trace = append(*trace, search10Query{sql: data.SQL, args: slices.Clone(data.Args)})
		return context.WithValue(ctx, search10QueryKey{}, len(*trace)-1)
	}
	return ctx
}

func (search10Tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if trace, ok := ctx.Value(search10TraceKey{}).(*search10Trace); ok {
		(*trace)[ctx.Value(search10QueryKey{}).(int)].rows = data.CommandTag.RowsAffected()
	}
}

func installSearch10Tracer(t testing.TB, s *Store) {
	t.Helper()
	cfg := s.pool.Config()
	s.pool.Close()
	cfg.ConnConfig.Tracer = search10Tracer{}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.pool = pool
}

var search10Fixtures = []string{"hub1000", "hub20000", "sparse100", "deleted-proposed-multi", "dense16"}

func TestSEARCH10AGEPathEligibilityIntegration(t *testing.T) {
	s := newIntegrationStore(t)
	sample := createHopSample(t, s, "path-eligibility")
	for _, name := range []string{"shortestPath", "allShortestPaths"} {
		query := s.cypherSQL("MATCH p="+name+"((a:Context)-[*0..2]-(b:Context)) RETURN p", "p agtype")
		var raw string
		err := s.pool.QueryRow(t.Context(), query, pgx.QueryExecModeExec).Scan(&raw)
		if pgerr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgerr.Code != "42601" {
			t.Fatalf("%s 고정 이미지 파서 결과: %v", name, err)
		}
	}
	for _, probe := range []struct {
		name, from, to, types string
		rows                  int
	}{
		{"proposed", "e3", "e4", "'PRECEDES'", 1},
		{"deleted-intermediate", "d4", "s3", "'DERIVED_FROM'", 1},
		{"unknown-label", "e3", "e4", "'SEARCH10_UNKNOWN_LABEL'", 0},
		{"label-array", "e3", "e4", "['SEARCH10_UNKNOWN_LABEL']", 0},
		{"mixed-label-array", "e3", "e4", "['PRECEDES','SEARCH10_UNKNOWN_LABEL']", 1},
	} {
		t.Run(probe.name, func(t *testing.T) {
			query := "MATCH (a:Context), (b:Context) WHERE a.context_id=" + cypherString(sample.nodes[probe.from].ID.String()) +
				" AND b.context_id=" + cypherString(sample.nodes[probe.to].ID.String()) +
				" RETURN shortest_path(a,b," + probe.types + ",'out',0,3)"
			rows, err := s.pool.Query(t.Context(), s.cypherSQL(query, "p agtype"), pgx.QueryExecModeExec)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				count++
			}
			if err := rows.Err(); err != nil || count != probe.rows {
				t.Fatalf("최단 경로 자격 표본: rows=%d want=%d err=%v", count, probe.rows, err)
			}
			t.Logf("shortest-path rows=%d", count)
		})
	}
	assertConnectionRecovery(t, s, nil, 0)
}

// EXPLAIN은 측정 호출과 분리하여 같은 불변 표본의 읽기 질의만 재실행한다.
// 버퍼는 각 질의 root만 합하고 정점/간선 행은 실제 relation 접근 행×loops다.
func TestSEARCH10HopResourceComparisonIntegration(t *testing.T) {
	s := newIntegrationStore(t)
	installSearch10Tracer(t, s)
	for _, name := range search10Fixtures {
		t.Run(name, func(t *testing.T) {
			sample := search10Fixture(t, s, name)
			baseline, err := runSearch10Hop(t.Context(), s, sample, "full")
			if err != nil {
				t.Fatal(err)
			}
			implementations := []string{"full", "bounded"}
			if sample.vle {
				implementations = append(implementations, "vle")
			}
			for _, implementation := range implementations {
				var trace search10Trace
				result, err := runSearch10Hop(context.WithValue(t.Context(), search10TraceKey{}, &trace), s, sample, implementation)
				if err != nil {
					t.Fatal(err)
				}
				compareHopResults(t, implementation, baseline, result)
				var rows int64
				var contexts, edges, milliseconds float64
				var hit, read, tempRead, tempWritten, plans int
				for _, query := range trace {
					rows += query.rows
					if !strings.Contains(query.sql, "cypher(") {
						continue // BEGIN/ROLLBACK/접근 기록은 왕복에는 포함하되 재실행하지 않는다.
					}
					plan := explainHop(t, s, query.sql, query.args...)
					contexts += plan.Root.scanned("Context")
					for _, label := range []string{"DERIVED_FROM", "HAS_MEMBER", "SUPERSEDES", "PRECEDES", "CAUSES", "PART_OF", "RELATES_TO", "_ag_label_edge"} {
						edges += plan.Root.scanned(label)
					}
					hit, read = hit+plan.Root.Hit, read+plan.Root.Read
					tempRead, tempWritten = tempRead+plan.Root.TempRead, tempWritten+plan.Root.TempWritten
					milliseconds += plan.Execution
					plans++
				}
				t.Logf("%s nodes=%d edges=%d truncated=%t/%d work=%+v roundtrips=%d sqlrows=%d plans=%d db-context-access=%.0f db-edge-access=%.0f buffers=%d+%d temp=%d/%d explain-ms=%.3f", implementation, len(result.Contexts), len(result.Edges), result.Truncated, result.Boundary, result.work, len(trace), rows, plans, contexts, edges, hit, read, tempRead, tempWritten, milliseconds)
				assertConnectionRecovery(t, s, nil, 0)
			}
		})
	}
}

// 동등성을 먼저 확인하고 순서를 뒤집은 두 회차에서 warm-cache 조회만 측정한다.
func BenchmarkSEARCH10HopComparisonIntegration(b *testing.B) {
	s := newIntegrationStore(b)
	installSearch10Tracer(b, s)
	for _, name := range search10Fixtures {
		sample := search10Fixture(b, s, name)
		baseline, err := runSearch10Hop(b.Context(), s, sample, "full")
		if err != nil {
			b.Fatal(err)
		}
		implementations := []string{"full", "bounded"}
		if sample.vle {
			implementations = append(implementations, "vle")
		}
		for round := range 2 {
			if round == 1 {
				slices.Reverse(implementations)
			}
			for _, implementation := range implementations {
				b.Run(fmt.Sprintf("%s/round%d/%s", name, round, implementation), func(b *testing.B) {
					var trace search10Trace
					check, err := runSearch10Hop(context.WithValue(b.Context(), search10TraceKey{}, &trace), s, sample, implementation)
					if err != nil {
						b.Fatal(err)
					}
					compareHopResults(b, name, baseline, check)
					b.ReportAllocs()
					for b.Loop() {
						if _, err := runSearch10Hop(b.Context(), s, sample, implementation); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportMetric(float64(len(trace)), "roundtrips/op")
					var rows int64
					for _, query := range trace {
						rows += query.rows
					}
					b.ReportMetric(float64(rows), "sqlrows/op")
					b.ReportMetric(float64(check.work.identifierRows), "idrows/op")
					b.ReportMetric(float64(check.work.bodyRows), "bodyrows/op")
					b.ReportMetric(float64(check.work.bodyBytes), "bodybytes/op")
					b.ReportMetric(float64(check.work.edgeRows), "edgerows/op")
					b.ReportMetric(float64(check.work.referenceRows), "refrows/op")
					if implementation == "vle" {
						// VLE는 본문 중복을 포함한 전송 행만 tracer로 측정한다.
						for _, metric := range []string{"idrows/op", "bodyrows/op", "bodybytes/op", "edgerows/op"} {
							b.ReportMetric(-1, metric)
						}
					}
				})
			}
		}
	}
}
