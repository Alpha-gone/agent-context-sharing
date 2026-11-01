package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 합성 부하는 정체성이 겹치는 간선도 허용하며, 시험이 소유한 별도 DB에만 넣는다.
func seedRemainingScopeNoise(t *testing.T, database *Store, graph, from, to model.ID, size int) {
	t.Helper()
	for _, label := range []string{"PRECEDES", "CAUSES", "PART_OF", "RELATES_TO", "DERIVED_FROM", "HAS_MEMBER", "SUPERSEDES"} {
		for offset := 0; offset < size; offset += 100 {
			var properties []string
			for range min(100, size-offset) {
				props := "{graph_id: " + cypherString(graph.String()) + "}"
				if kind, ok := relationLabels[label]; ok {
					relation := normalizeRelation(proposedRelation(graph, kind, from, to))
					relation.ID = newTestID(t)
					encoded, err := encodeProperties(relationProperties(relation))
					if err != nil {
						t.Fatal(err)
					}
					props = encoded
				}
				properties = append(properties, props)
			}
			query := "MATCH (a:Context), (b:Context) WHERE a.context_id = " + cypherString(from.String()) + " AND b.context_id = " + cypherString(to.String()) +
				" WITH a, b UNWIND [" + strings.Join(properties, ",") + "] AS props CREATE (a)-[e:" + label + "]->(b) SET e = props RETURN count(e)"
			if _, err := database.agtypeCount(t.Context(), query); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, label := range []string{"Context", "PRECEDES", "CAUSES", "PART_OF", "RELATES_TO", "DERIVED_FROM", "HAS_MEMBER", "SUPERSEDES"} {
		if _, err := database.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{database.graphName, label}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
}

func remainingScopeQueries(tracer *relationScopeTracer, contains string) ([]string, [][]any) {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	var queries []string
	var args [][]any
	for i, query := range tracer.queries {
		if strings.Contains(query, contains) {
			queries = append(queries, query)
			args = append(args, slices.Clone(tracer.args[i]))
		}
	}
	tracer.queries, tracer.args = nil, nil
	return queries, args
}

func TestDeletionImpactRelationScopeIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, from, to := relationTestEvents(t, database)
	foreign, foreignFrom, foreignTo := relationTestEvents(t, database)
	for _, kind := range []model.RelationType{model.RelationTypePrecedes, model.RelationTypeCauses, model.RelationTypePartOf, model.RelationTypeRelatesTo} {
		if _, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, kind, from.ID, to.ID), nil); err != nil {
			t.Fatal(err)
		}
	}
	// 같은 그래프의 proposed/discarded는 확정 관계 집계에서 제외한다.
	extra := from
	extra.ID = newTestID(t)
	extra, err := database.CreateContext(t.Context(), graph, extra, nil)
	if err != nil {
		t.Fatal(err)
	}
	discarded, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypeRelatesTo, from.ID, extra.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DiscardRelation(t.Context(), graph, discarded.ID, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), tx)
	proposed := proposedRelation(graph, model.RelationTypePrecedes, extra.ID, from.ID)
	proposed.ID = newTestID(t)
	if _, err := database.createRelation(t.Context(), tx, graph, proposed); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	tracer := &relationScopeTracer{}
	configureTierPool(t, database, tracer)
	t.Cleanup(database.Close)
	baseline := map[string]map[string]float64{}
	previous := 0
	for _, size := range []int{0, 1000, 10000} {
		seedRemainingScopeNoise(t, database, foreign, foreignFrom.ID, foreignTo.ID, size-previous)
		previous = size
		for _, sample := range []struct {
			name    string
			context model.ID
		}{{"graph", model.ID{}}, {"from", from.ID}, {"to", to.ID}} {
			remainingScopeQueries(tracer, "")
			impact, err := database.DeletionImpact(t.Context(), graph, sample.context)
			if err != nil || impact.Relations != 4 {
				t.Fatalf("%s/%d: impact=%+v err=%v", sample.name, size, impact, err)
			}
			queries, args := remainingScopeQueries(tracer, "RETURN count(edge)")
			if len(queries) != 1 {
				t.Fatalf("관계 집계는 한 문장이어야 한다: %d", len(queries))
			}
			plan := explainHop(t, database, queries[0], args[0]...)
			old := "MATCH (from:Context)-[edge]->(to:Context) WHERE edge.graph_id = " + cypherString(graph.String()) + " AND edge.relation_type IS NOT NULL AND edge.state = 'confirmed'"
			if sample.context.IsV7() {
				old += " AND (from.context_id = " + cypherString(sample.context.String()) + " OR to.context_id = " + cypherString(sample.context.String()) + ")"
			}
			old += " RETURN count(edge)"
			oldCount, err := database.agtypeCount(t.Context(), old)
			if err != nil || oldCount != impact.Relations {
				t.Fatalf("기존 집계와 불일치: %d/%d %v", oldCount, impact.Relations, err)
			}
			reads := map[string]float64{}
			for _, label := range []string{"PRECEDES", "CAUSES", "PART_OF", "RELATES_TO", "DERIVED_FROM", "HAS_MEMBER", "SUPERSEDES"} {
				reads[label] = plan.Root.scanned(label)
				if size == 0 {
					continue
				}
				if reads[label] != baseline[sample.name][label] {
					t.Errorf("%s/%d %s 읽기 증가: %g -> %g", sample.name, size, label, baseline[sample.name][label], reads[label])
				}
			}
			if size == 0 {
				baseline[sample.name] = reads
			}
			t.Logf("deletion/%s foreign_per_label=%d reads=%v", sample.name, size, reads)
			if size == 10000 && sample.name == "graph" {
				oldPlan := explainHop(t, database, database.cypherSQL(old, "count agtype"))
				t.Logf("legacy deletion reference_reads=%g", oldPlan.Root.scanned("DERIVED_FROM")+oldPlan.Root.scanned("HAS_MEMBER")+oldPlan.Root.scanned("SUPERSEDES"))
			}
		}
	}
	count, err := database.confirmedRelationCount(t.Context(), graph, newTestID(t))
	if err != nil || count != 0 {
		t.Fatalf("관계 없는 사건 집계=%d %v", count, err)
	}
}

func TestSimilarProposalIdentityScopeIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	graph, actor, member := lifeProposalFixture(t, database)
	event := func() model.Context {
		return proposalEvent(t, database, graph, actor, []model.ID{member.ID}, time.Now().UTC(), nil)
	}
	confirmed, discarded := event(), event()
	target := event()
	proposed, eligible := event(), event()
	for _, value := range []model.Context{confirmed, discarded, target, proposed, eligible} {
		lifeSeedProposalEmbedding(t, database, value, "current", 1)
	}
	for i, value := range []model.Context{confirmed, discarded} {
		relation, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypeRelatesTo, target.ID, value.ID), nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if _, err := database.DiscardRelation(t.Context(), graph, relation.ID, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	database.relationProposals = lifeProposalConfig(1)
	if created, err := database.createProposedRelation(t.Context(), graph, normalizeRelation(proposedRelation(graph, model.RelationTypeRelatesTo, target.ID, proposed.ID)), "current"); err != nil || !created {
		t.Fatalf("제안 준비: %t %v", created, err)
	}
	foreign, foreignFrom, foreignTo := relationTestEvents(t, database)
	tracer := &relationScopeTracer{}
	configureTierPool(t, database, tracer)
	t.Cleanup(database.Close)
	var baseline float64
	previous := 0
	for _, size := range []int{0, 1000, 10000} {
		seedRemainingScopeNoise(t, database, foreign, foreignFrom.ID, foreignTo.ID, size-previous)
		previous = size
		remainingScopeQueries(tracer, "")
		if err := database.ProposeSimilarEventRelations(t.Context(), graph, target.ID, "current"); err != nil {
			t.Fatal(err)
		}
		queries, args := remainingScopeQueries(tracer, "FROM public.context_embedding AS candidate")
		if len(queries) != 1 {
			t.Fatalf("후보 질의 수=%d", len(queries))
		}
		created := lifeProposalIdentity(t, database, graph, model.RelationTypeRelatesTo, target.ID, eligible.ID)
		if len(proposalRelations(t, database, graph)) != 4 {
			t.Fatal("기존 세 정체성의 상태가 후보 제외에서 보존되지 않음")
		}
		if removed, err := database.deleteProposedRelation(t.Context(), graph, created.ID, time.Now().UTC().Add(time.Hour)); err != nil || !removed {
			t.Fatalf("시험 생성 후보 정리: %t %v", removed, err)
		}
		rows, err := database.pool.Query(t.Context(), queries[0], append([]any{pgx.QueryExecModeExec}, args[0]...)...)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := searchCandidateIDs(rows)
		if err != nil || !slices.Equal(ids, []model.ID{eligible.ID}) {
			t.Fatalf("정체성 제외 후보=%v %v", ids, err)
		}
		plan := explainHop(t, database, queries[0], args[0]...)
		scanned := plan.Root.scanned("RELATES_TO")
		if size == 0 {
			baseline = scanned
		} else if scanned != baseline {
			t.Errorf("다른 그래프의 간선 증가에 후보 제외 읽기 변경: %g -> %g", baseline, scanned)
		}
		// 같은 graph_id 접두사를 가진 target 인덱스도 이 표본에서 동등하게 범위를 제한한다.
		if size > 0 && !planUsesIndex(plan.Root, "relates_to_graph_identity_idx") && !planUsesIndex(plan.Root, "relates_to_graph_target_idx") {
			logQueryIndexPlan(t, fmt.Sprint(size), plan)
			t.Fatal("관계 그래프 범위 인덱스 미사용")
		}
		t.Logf("similarity foreign_per_label=%d RELATES_TO_reads=%g", size, scanned)
		if size == 10000 {
			logQueryIndexPlan(t, fmt.Sprint(size), plan)
			legacy := queries[0]
			for _, key := range []string{"graph_id", "from_context_id", "to_context_id"} {
				legacy = strings.ReplaceAll(legacy, "ag_catalog.agtype_access_operator(VARIADIC ARRAY[relation.properties, '\""+key+"\"'::agtype])", "relation.properties ->> '"+key+"'::text")
			}
			legacy = strings.NewReplacer("to_jsonb($2::text)::text::agtype", "$2::text", "to_jsonb(LEAST($1::text, candidate.context_id::text))::text::agtype", "LEAST($1::text, candidate.context_id::text)", "to_jsonb(GREATEST($1::text, candidate.context_id::text))::text::agtype", "GREATEST($1::text, candidate.context_id::text)").Replace(legacy)
			rows, err := database.pool.Query(t.Context(), legacy, append([]any{pgx.QueryExecModeExec}, args[0]...)...)
			if err != nil {
				t.Fatal(err)
			}
			oldIDs, err := searchCandidateIDs(rows)
			if err != nil || !slices.Equal(oldIDs, ids) {
				t.Fatalf("기존 후보 제외 결과와 불일치: %v/%v %v", oldIDs, ids, err)
			}
			oldPlan := explainHop(t, database, legacy, args[0]...)
			t.Logf("legacy similarity RELATES_TO_reads=%g", oldPlan.Root.scanned("RELATES_TO"))
		}
	}
}
