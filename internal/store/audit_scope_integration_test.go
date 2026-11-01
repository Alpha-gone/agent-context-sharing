package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 다른 그래프의 행을 늘려도 고정된 그래프의 실제 스캔량이 늘지 않아야 한다.
func TestAuditGraphScopeIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph := createInvariantGraph(t, database)
	foreign := newTestID(t)
	contexts := pgx.Identifier{database.graphName, "Context"}.Sanitize()
	previous := 0
	for _, size := range []int{1000, 10000} {
		if _, err := database.pool.Exec(t.Context(), `INSERT INTO `+contexts+` (properties)
   SELECT ag_catalog.agtype_build_map('graph_id', $1::text, 'context_id', 'unrelated-' || n::text, 'layer', 'source', 'sample', n)
   FROM generate_series($2::int, $3::int) n`, foreign.String(), previous+1, size); err != nil {
			t.Fatal(err)
		}
		for _, label := range []string{"DERIVED_FROM", "SUPERSEDES", "HAS_MEMBER", "PRECEDES", "CAUSES", "PART_OF", "RELATES_TO"} {
			table := pgx.Identifier{database.graphName, label}.Sanitize()
			if _, err := database.pool.Exec(t.Context(), `INSERT INTO `+table+` (start_id,end_id,properties)
    SELECT id,id,ag_catalog.agtype_build_map('graph_id',$1::text) FROM `+contexts+` WHERE properties ->> 'graph_id'::text=$1 AND (properties ->> 'sample'::text)::int>$2`, foreign.String(), previous); err != nil {
				t.Fatal(err)
			}
			if _, err := database.pool.Exec(t.Context(), "ANALYZE "+table); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := database.pool.Exec(t.Context(), "ANALYZE "+contexts); err != nil {
			t.Fatal(err)
		}
		tx, err := database.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		traced := &relationPlanTx{Tx: tx}
		snapshot, err := database.loadAuditSnapshot(t.Context(), traced, graph.graphID, nil, graph.expect)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.contexts) != 7 || len(snapshot.edges) != 6 {
			t.Fatalf("감사 범위: 정점=%d 간선=%d", len(snapshot.contexts), len(snapshot.edges))
		}
		var scanned float64
		var buffers int
		for i, query := range traced.queries {
			plan := explainRelationPlan(t, tx, query, traced.args[i]...)
			logHopPlan(t, fmt.Sprint(i), plan.Root)
			scanned += plan.Root.scanned("Context")
			for _, label := range []string{"DERIVED_FROM", "SUPERSEDES", "HAS_MEMBER", "PRECEDES", "CAUSES", "PART_OF", "RELATES_TO"} {
				scanned += plan.Root.scanned(label)
			}
			buffers += plan.Root.Hit + plan.Root.Read
		}
		t.Logf("다른 그래프 정점=%d: scanned=%g buffers=%d queries=%d", size, scanned, buffers, len(traced.queries))
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		previous = size
		if scanned > 300 {
			t.Fatalf("다른 그래프 전체 스캔: %s", fmt.Sprint(scanned))
		}
	}
}

// 기존 질의가 잡던 소속 훼손·교차 연결·양쪽 끝점 누락을 같은 보고로 유지한다.
func TestAuditGraphCorruptionScopeIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	for _, label := range []string{"DERIVED_FROM", "SUPERSEDES", "HAS_MEMBER", "PRECEDES", "CAUSES", "PART_OF", "RELATES_TO"} {
		t.Run(label, func(t *testing.T) {
			graph := createInvariantGraph(t, database)
			from, to := graph.early.ID, graph.late.ID
			foreign := graph.foreignSource
			for _, item := range []struct{ graph, from, to model.ID }{
				{graph.graphID, from, to},         // 세 조회 조건에 걸려도 물리 ID당 한 번.
				{graph.graphID, from, to},         // 같은 속성의 다른 물리 ID는 보존.
				{newTestID(t), from, to},          // 끝점 소속만 일치.
				{newTestID(t), foreign, to},       // 도착점 소속만 일치.
				{graph.graphID, foreign, foreign}, // 간선 소속만 일치.
			} {
				corruptEdge(t, database, item.graph, label, item.from, item.to, newTestID(t).String())
			}
			// 동일 context_id의 물리 정점이 둘이어도 양쪽 정점에 닿는 간선을 보존한다.
			contexts := database.contextTable()
			table := pgx.Identifier{database.graphName, label}.Sanitize()
			if _, err := database.pool.Exec(t.Context(), `WITH duplicate AS (
    INSERT INTO `+contexts+` (properties) SELECT properties FROM `+contexts+` WHERE properties ->> 'context_id'::text=$1 RETURNING id
   ) INSERT INTO `+table+` (start_id,end_id,properties)
    SELECT d.id,c.id,ag_catalog.agtype_build_map('graph_id',$3::text)
    FROM duplicate d CROSS JOIN `+contexts+` c WHERE c.properties ->> 'context_id'::text=$2`, from.String(), to.String(), newTestID(t).String()); err != nil {
				t.Fatal(err)
			}
			// 간선 소속 외에는 단서가 없는 양 끝점 누락도 포함한다.
			if _, err := database.pool.Exec(t.Context(), `INSERT INTO `+table+` (start_id,end_id,properties)
    VALUES ('1'::ag_catalog.graphid,'2'::ag_catalog.graphid,ag_catalog.agtype_build_map('graph_id',$1::text))`, graph.graphID.String()); err != nil {
				t.Fatal(err)
			}
			tx, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), tx)
			snapshot, err := database.loadAuditSnapshot(t.Context(), tx, graph.graphID, nil, graph.expect)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.pathExists = memoryPaths(snapshot.edges)
			got, err := runInvariantRules(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			// 기존 전역 OR/고아 보충과 정렬된 위반 보고를 대조한다.
			legacy := snapshot
			legacy.edges = nil
			endpoints, seen := map[string]struct{}{}, map[string]struct{}{}
			filter := "a.graph_id = " + cypherString(graph.graphID.String()) + " OR b.graph_id = " + cypherString(graph.graphID.String()) + " OR e.graph_id = " + cypherString(graph.graphID.String())
			if err := database.loadAuditEdges(t.Context(), tx, filter, &legacy, endpoints, seen); err != nil {
				t.Fatal(err)
			}
			if err := database.loadDanglingAuditEdges(t.Context(), tx, &legacy); err != nil {
				t.Fatal(err)
			}
			legacy.pathExists = memoryPaths(legacy.edges)
			want, err := runInvariantRules(legacy)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.edges) != len(legacy.edges) || len(got) == 0 || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: 새=%+v 기존=%+v edges=%d/%d", label, got, want, len(snapshot.edges), len(legacy.edges))
			}
			if !slices.ContainsFunc(got, func(v InvariantViolation) bool {
				return v.Rule == RuleGraphMembership && v.Label == strings.ToLower(label)
			}) {
				t.Fatal("소속 손상 누락")
			}
		})
	}
}

func TestAuditUnexpectedLabelsIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph := createInvariantGraph(t, database)
	if _, err := database.pool.Exec(t.Context(), "SELECT ag_catalog.create_elabel("+sqlString(database.graphName)+", 'AUDIT_UNKNOWN')"); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"AUDIT_UNKNOWN", "_ag_label_edge"} {
		table := pgx.Identifier{database.graphName, label}.Sanitize()
		if _, err := database.pool.Exec(t.Context(), `INSERT INTO `+table+` (start_id,end_id,properties)
   SELECT a.id,b.id,ag_catalog.agtype_build_map('graph_id',$1::text)
   FROM `+database.contextTable()+` a CROSS JOIN `+database.contextTable()+` b
   WHERE a.properties ->> 'context_id'::text=$2 AND b.properties ->> 'context_id'::text=$3`, graph.graphID.String(), graph.early.ID.String(), graph.late.ID.String()); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := database.loadAuditSnapshot(t.Context(), database.pool, graph.graphID, nil, graph.expect)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.pathExists = memoryPaths(snapshot.edges)
	got, err := runInvariantRules(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	legacy := snapshot
	legacy.edges = nil
	endpoints, seen := map[string]struct{}{}, map[string]struct{}{}
	if err := database.loadAuditEdges(t.Context(), database.pool, "a.graph_id = "+cypherString(graph.graphID.String()), &legacy, endpoints, seen); err != nil {
		t.Fatal(err)
	}
	legacy.pathExists = memoryPaths(legacy.edges)
	want, err := runInvariantRules(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("예상 밖 label: 새=%+v 기존=%+v", got, want)
	}
}

func TestAuditMalformedEdgePropertiesIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph := createInvariantGraph(t, database)
	for _, key := range []string{"relation_id", "state"} {
		t.Run(key, func(t *testing.T) {
			tx, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), tx)
			table := pgx.Identifier{database.graphName, "PART_OF"}.Sanitize()
			if _, err := tx.Exec(t.Context(), `UPDATE `+table+` SET properties=properties || ag_catalog.agtype_build_map($1::text,123) WHERE properties ->> 'graph_id'::text=$2`, key, graph.graphID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := database.loadAuditSnapshot(t.Context(), tx, graph.graphID, nil, graph.expect); err == nil {
				t.Fatal("손상된 속성을 문자열로 허용했다")
			}
		})
	}
}
