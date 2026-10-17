package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func TestIsolationReadSurfacesIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	foreignGraph, _, foreign := boundaryGraph(t, database)
	derived, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]func(context.Context) ([]model.Context, error){
		"single": func(ctx context.Context) ([]model.Context, error) {
			v, e := database.Context(ctx, graph, source.ID)
			return []model.Context{v}, e
		},
		"batch": func(ctx context.Context) ([]model.Context, error) {
			return database.ContextsByIDs(ctx, graph, []model.ID{derived.ID, source.ID})
		},
		"keyword": func(ctx context.Context) ([]model.Context, error) {
			v, e := database.KeywordCandidates(ctx, graph, source.Body, time.Now().UTC(), 20)
			return candidateContexts(v), e
		},
		"time": func(ctx context.Context) ([]model.Context, error) {
			v, e := database.TimeCandidates(ctx, graph, time.Now().UTC(), 20)
			return candidateContexts(v), e
		},
		"active": func(ctx context.Context) ([]model.Context, error) {
			v, _, e := database.ListActiveContexts(ctx, graph, 20)
			return v, e
		},
		"visualization": func(ctx context.Context) ([]model.Context, error) {
			v, e := database.GraphVisualization(ctx, graph, 1, 20)
			return v.Contexts, e
		},
		"hop": func(ctx context.Context) ([]model.Context, error) {
			v, e := database.HopContexts(ctx, graph, source.ID, 1, "in", nil, 20)
			return v.Contexts, e
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			values, err := read(t.Context())
			if err != nil || len(values) == 0 {
				t.Fatalf("read: %v, %d", err, len(values))
			}
			for _, v := range values {
				if v.GraphID != graph || v.ID == foreign.ID {
					t.Fatalf("다른 그래프 반환: %s", v.ID)
				}
				if err := v.Validate(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for _, scoped := range []bool{false, true} {
		run := func(ctx context.Context) error {
			if v, e := database.Context(ctx, graph, foreign.ID); !errors.Is(e, ErrNotFound) || v.ID.IsV7() {
				t.Errorf("single foreign: %+v %v", v, e)
			}
			if v, e := database.ContextsByIDs(ctx, graph, []model.ID{source.ID, foreign.ID}); !errors.Is(e, ErrNotFound) || len(v) != 0 {
				t.Errorf("batch foreign: %v %v", v, e)
			}
			if v, e := database.HopContextsFrom(ctx, graph, []model.Context{foreign}, 1, "both", nil, 10); e == nil || len(v.Contexts) != 0 {
				t.Errorf("hop foreign: %+v %v", v, e)
			}
			return nil
		}
		if scoped {
			if _, e := database.WithReadSnapshot(t.Context(), run); e != nil {
				t.Fatal(e)
			}
		} else {
			_ = run(t.Context())
		}
	}
	// 다른 그래프 식별자를 커서로 써도 요청 그래프의 범위는 유지한다.
	for _, cursor := range []string{"' OR true //", encodeRelationCursor(foreign.ID)} {
		values, _, err := database.ListRelations(t.Context(), graph, cursor, 10)
		if strings.HasPrefix(cursor, "'") {
			if !errors.Is(err, ErrInvalidCursor) {
				t.Fatal(err)
			}
		} else if err != nil || len(values) != 0 {
			t.Fatalf("foreign cursor: %v %v", values, err)
		}
	}
	for _, state := range []model.RelationState{"confirmed'] RETURN edge //", "unknown"} {
		if values, _, err := database.ListContextRelations(t.Context(), graph, source.ID, []model.RelationState{state}, nil, "", 10); err == nil || len(values) != 0 {
			t.Fatal("조작된 상태 필터 허용")
		}
	}
	if _, _, err := database.ListContextRelations(t.Context(), graph, source.ID, nil, []model.RelationType{"precedes'] RETURN edge //"}, "", 10); err == nil {
		t.Fatal("조작된 종류 필터 허용")
	}
	if _, err := database.HopContexts(t.Context(), foreignGraph, source.ID, 1, "both", nil, 10); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := database.HopContextsFrom(t.Context(), graph, []model.Context{source}, 1, "invalid", nil, 10); err == nil {
		t.Fatal("잘못된 방향 허용")
	}
	if _, err := database.HopContextsFrom(t.Context(), graph, []model.Context{source}, 1, "both", []string{"HAS_MEMBER] RETURN node //"}, 10); err == nil {
		t.Fatal("조작된 홉 필터 허용")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func candidateContexts(values []SearchCandidate) []model.Context {
	result := make([]model.Context, len(values))
	for i, v := range values {
		result[i] = v.Context
	}
	return result
}

func TestIsolationReferenceCorruptionIntegration(t *testing.T) {
	for _, kind := range []string{"edge_graph", "target_graph", "endpoint_layer", "duplicate", "layer_property", "missing_flag"} {
		t.Run(kind, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			foreignGraph, _, foreign := boundaryGraph(t, database)
			derived, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "edge_graph":
				isolationExec(t, database, "MATCH (a:Context)-[edge:DERIVED_FROM]->() WHERE a.context_id = "+cypherString(derived.ID.String())+" SET edge.graph_id = "+cypherString(foreignGraph.String()))
			case "target_graph":
				corruptEdge(t, database, graph, "DERIVED_FROM", derived.ID, foreign.ID, "")
			case "endpoint_layer":
				event, e := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
				if e != nil {
					t.Fatal(e)
				}
				corruptEdge(t, database, graph, "DERIVED_FROM", derived.ID, event.ID, "")
			case "duplicate":
				corruptEdge(t, database, graph, "DERIVED_FROM", derived.ID, source.ID, "")
			case "missing_flag":
				isolationExec(t, database, "MATCH (a:Context) WHERE a.context_id = "+cypherString(derived.ID.String())+" REMOVE a.evidence_invalidated")
			case "layer_property":
				isolationExec(t, database, "MATCH (a:Context) WHERE a.context_id = "+cypherString(derived.ID.String())+" SET a.origin_kind = 'human'")
			}
			for name, read := range map[string]func() error{
				"single": func() error {
					v, e := database.Context(t.Context(), graph, derived.ID)
					if v.ID.IsV7() {
						t.Error("손상된 단건 반환")
					}
					return e
				},
				"batch": func() error {
					v, e := database.ContextsByIDs(t.Context(), graph, []model.ID{source.ID, derived.ID})
					if len(v) != 0 {
						t.Error("손상된 묶음 반환")
					}
					return e
				},
				"list": func() error {
					v, _, e := database.ListActiveContexts(t.Context(), graph, 10)
					if len(v) != 0 {
						t.Error("손상된 목록 반환")
					}
					return e
				},
				"write": func() error {
					_, e := database.KeepContext(t.Context(), graph, derived.ID, derived.Version, nil)
					return e
				},
			} {
				t.Run(name, func(t *testing.T) {
					e := read()
					if _, ok := errors.AsType[*StorageError](e); !ok {
						t.Fatalf("내부 저장 오류 필요: %v", e)
					}
				})
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func isolationExec(t *testing.T, database *Store, query string) {
	t.Helper()
	if _, err := database.pool.Exec(t.Context(), database.cypherSQL(query, "value agtype"), pgx.QueryExecModeExec); err != nil {
		t.Fatal(err)
	}
}

func TestIsolationWriteReferencesIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	_, _, foreign := boundaryGraph(t, database)
	event, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted := testSourceContext(t, graph, actor, "api://deleted/"+graph.String())
	deleted, err = database.CreateContext(t.Context(), graph, deleted, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err = database.DiscardContext(t.Context(), graph, deleted.ID, nil, WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := database.Graph(t.Context(), graph)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]model.Context{"foreign": foreign, "event": event, "deleted": deleted} {
		t.Run(name, func(t *testing.T) {
			value := testDerivedContext(t, graph, actor)
			if _, err := database.CreateContext(t.Context(), graph, value, []model.ID{target.ID}); err == nil {
				t.Fatal("잘못된 근거 허용")
			}
			if _, err := database.Context(t.Context(), graph, value.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("거부된 정점이 남음: %v", err)
			}
			next := testEventContext(t, graph, actor, target.ID)
			if _, err := database.CreateContext(t.Context(), graph, next, nil); err == nil {
				t.Fatal("잘못된 구성원 허용")
			}
			if _, err := database.Context(t.Context(), graph, next.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("거부한 사건이 남음: %v", err)
			}
			if indexTaskCount(t, database, value.ID) != 0 || indexTaskCount(t, database, next.ID) != 0 {
				t.Fatal("거부한 쓰기의 색인 작업이 남음")
			}
		})
	}
	afterRejected, err := database.Graph(t.Context(), graph)
	if err != nil || !reflect.DeepEqual(afterRejected, baseline) {
		t.Fatalf("거부한 참조 쓰기가 그래프를 변경했다: %+v %v", afterRejected, err)
	}
	previous, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	invalid := testDerivedContext(t, graph, actor)
	invalid.Derived.Kind = model.DerivationKindReflection
	if _, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, invalid, []model.ID{source.ID}, previous.ID, nil, WriteLimits{}); err == nil {
		t.Fatal("다른 파생 종류 대체 허용")
	}
	superseding, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID}, previous.ID, nil, WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := database.Context(t.Context(), graph, superseding.ID)
	if err != nil || loaded.Derived.Supersedes == nil || *loaded.Derived.Supersedes != previous.ID {
		t.Fatalf("대체 참조 유실: %+v %v", loaded, err)
	}
	next := loaded
	next.Derived = new(*loaded.Derived)
	next.Derived.DerivedFrom = []model.ID{foreign.ID}
	next.Version++
	if _, err := database.UpdateContext(t.Context(), graph, loaded.Version, next); err == nil {
		t.Fatal("불변 근거 변경 허용")
	}
	next = source
	next.Source = new(*source.Source)
	next.Body += " changed"
	if _, err := database.UpdateContext(t.Context(), graph, source.Version, next); err == nil {
		t.Fatal("원천 본문 변경 허용")
	}
	current, err := database.Graph(t.Context(), graph)
	if err != nil || current.StoredChars <= baseline.StoredChars {
		t.Fatal("정상 생성의 저장량 반영 실패")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIsolationRelationActualEndpointsIntegration(t *testing.T) {
	for _, kind := range []string{"foreign_endpoint", "wrong_identity", "wrong_layer"} {
		t.Run(kind, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			otherGraph, otherActor, otherSource := boundaryGraph(t, database)
			a, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
			if err != nil {
				t.Fatal(err)
			}
			b, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
			if err != nil {
				t.Fatal(err)
			}
			relation := normalizeRelation(confirmable(t, graph, model.RelationTypeRelatesTo, a.ID, b.ID))
			relation.ID = newTestID(t)
			relation, err = database.ConfirmRelation(t.Context(), graph, relation, nil)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "foreign_endpoint":
				c, e := database.CreateContext(t.Context(), otherGraph, testEventContext(t, otherGraph, otherActor, otherSource.ID), nil)
				if e != nil {
					t.Fatal(e)
				}
				properties, e := encodeProperties(relationProperties(relation))
				if e != nil {
					t.Fatal(e)
				}
				isolationExec(t, database, "MATCH (a:Context), (b:Context) WHERE a.context_id = "+cypherString(a.ID.String())+" AND b.context_id = "+cypherString(c.ID.String())+" CREATE (a)-[:RELATES_TO "+properties+"]->(b)")
			case "wrong_identity":
				isolationExec(t, database, "MATCH ()-[e]->() WHERE e.relation_id = "+cypherString(relation.ID.String())+" SET e.to_context_id = "+cypherString(newTestID(t).String()))
			case "wrong_layer":
				properties, e := encodeProperties(relationProperties(relation))
				if e != nil {
					t.Fatal(e)
				}
				isolationExec(t, database, "MATCH (a:Context), (b:Context) WHERE a.context_id = "+cypherString(a.ID.String())+" AND b.context_id = "+cypherString(source.ID.String())+" CREATE (a)-[:RELATES_TO "+properties+"]->(b)")
			}
			values, _, err := database.ListRelations(t.Context(), graph, "", 100)
			if len(values) != 0 {
				t.Fatal("손상된 관계 목록 반환")
			}
			if _, ok := errors.AsType[*StorageError](err); !ok {
				t.Fatalf("관계 손상 미탐지: %v", err)
			}
			if kind == "wrong_identity" {
				for _, limit := range []int{0, 10} {
					result, err := database.HopContexts(t.Context(), graph, a.ID, 1, "both", []string{"relates_to"}, limit)
					if len(result.Contexts) != 0 {
						t.Error("손상된 홉 반환")
					}
					if _, ok := errors.AsType[*StorageError](err); !ok {
						t.Fatalf("홉 관계 손상 미탐지: %v", err)
					}
				}
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestIsolationEmbeddingRowsIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	foreignGraph, _, foreign := boundaryGraph(t, database)
	other, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://embedding/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	dimension := embeddingDimension(t, database)
	processor := scopeTestProcessor(dimension)
	for _, scope := range []struct {
		graph model.ID
		ids   []model.ID
	}{{graph, []model.ID{source.ID, other.ID}}, {foreignGraph, []model.ID{foreign.ID}}} {
		readyIndexTasks(t, database, scope.graph, scope.ids...)
		for range len(scope.ids) {
			if result, e := database.ProcessNextIndexTaskInGraph(t.Context(), scope.graph, processor); e != nil || !result.Succeeded {
				t.Fatalf("표본 색인: %+v %v", result, e)
			}
		}
	}
	vector := make([]float64, dimension)
	vector[0] = 1
	if values, e := database.SemanticCandidates(t.Context(), graph, "scope-test", vector, time.Now().UTC(), 10); e != nil || len(values) != 2 {
		t.Fatalf("정상 의미 검색: %d %v", len(values), e)
	}
	if _, e := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET graph_id=$1 WHERE context_id=$2`, foreignGraph.String(), source.ID.String()); e != nil {
		t.Fatal(e)
	}
	orphan := newTestID(t)
	if _, e := database.pool.Exec(t.Context(), `INSERT INTO public.context_embedding(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier) SELECT $1,$2,embedding,model_id,indexed_at,last_accessed_at,storage_tier FROM public.context_embedding WHERE context_id=$3`, orphan.String(), graph.String(), other.ID.String()); e != nil {
		t.Fatal(e)
	}
	for _, scope := range []struct{ graph, want model.ID }{{graph, other.ID}, {foreignGraph, foreign.ID}} {
		_, e := database.WithReadSnapshot(t.Context(), func(ctx context.Context) error {
			values, e := database.SemanticCandidates(ctx, scope.graph, "scope-test", vector, time.Now().UTC(), 10)
			if e != nil {
				return e
			}
			if len(values) != 1 || values[0].Context.ID != scope.want || values[0].Context.GraphID != scope.graph {
				t.Errorf("잘못된 임베딩 연결 반환: %+v", values)
			}
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	if values, e := database.SemanticCandidates(t.Context(), graph, "old-model", vector, time.Now().UTC(), 10); e != nil || len(values) != 0 {
		t.Fatalf("옛 모델: %d %v", len(values), e)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIsolationImmutableWritesIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	derived, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		value  model.Context
		mutate func(*model.Context)
	}{
		{"source_body", source, func(v *model.Context) { v.Body += " changed" }},
		{"source_locator", source, func(v *model.Context) { v.Source.Reference.Locator = "api://changed" }},
		{"source_time", source, func(v *model.Context) { v.Source.OccurredAt = v.Source.OccurredAt.Add(time.Second) }},
		{"source_origin", source, func(v *model.Context) { v.Source.OriginKind = model.OriginKindAgentOutput }},
		{"source_version", source, func(v *model.Context) { v.Version++ }},
		{"derived_kind", derived, func(v *model.Context) { v.Derived.Kind = model.DerivationKindReflection; v.Version++ }},
		{"derived_evidence", derived, func(v *model.Context) { v.Derived.EvidenceState = model.EvidenceStateExperience; v.Version++ }},
		{"derived_invalidated", derived, func(v *model.Context) { v.Derived.EvidenceInvalidated = true; v.Version++ }},
		{"derived_supersedes", derived, func(v *model.Context) { v.Derived.Supersedes = new(newTestID(t)); v.Version++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			next := test.value
			if next.Source != nil {
				next.Source = new(*next.Source)
			}
			if next.Derived != nil {
				next.Derived = new(*next.Derived)
			}
			test.mutate(&next)
			if _, e := database.UpdateContext(t.Context(), graph, test.value.Version, next); e == nil {
				t.Fatal("불변 속성이 갱신됨")
			}
			stored, e := database.Context(t.Context(), graph, test.value.ID)
			if e != nil || stored.Version != test.value.Version || stored.Body != test.value.Body {
				t.Fatalf("거부한 변경이 남음: %+v %v", stored, e)
			}
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}
