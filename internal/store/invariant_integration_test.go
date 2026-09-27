package store

import (
	"errors"
	"reflect"
	"testing"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// invariantGraph는 규칙을 모두 만족하는 표본 그래프다. 원천 둘, 원천 하나를 근거로 한 파생,
// 시간이 다른 사건 둘과 시간이 같은 사건 둘이 있고, 같은 시간 사건 사이에 확정 part_of가
// 있다. 모든 컨텍스트는 임베딩을 가진다.
type invariantGraph struct {
	graphID, foreignSource   model.ID
	source, second, derived  model.Context
	early, late, whole, part model.Context
	expect                   EmbeddingExpectation
}

func createInvariantGraph(t *testing.T, database *Store) invariantGraph {
	t.Helper()
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	create := func(value model.Context, derivedFrom ...model.ID) model.Context {
		t.Helper()
		created, err := database.CreateContext(t.Context(), graphID, value, derivedFrom)
		if err != nil {
			t.Fatalf("표본 생성: %v", err)
		}
		return created
	}
	graph := invariantGraph{graphID: graphID}
	graph.source = create(testSourceContext(t, graphID, actorID, "api://invariant/"+graphID.String()+"/1"))
	graph.second = create(testSourceContext(t, graphID, actorID, "api://invariant/"+graphID.String()+"/2"))
	graph.derived = create(testDerivedContext(t, graphID, actorID), graph.source.ID)
	graph.early = create(testEventContext(t, graphID, actorID, graph.source.ID))
	graph.late = create(testEventContext(t, graphID, actorID, graph.second.ID))
	graph.whole = create(testEventContext(t, graphID, actorID, graph.source.ID))
	partValue := testEventContext(t, graphID, actorID, graph.source.ID)
	partValue.Event.Start, partValue.Event.End = graph.whole.Event.Start, graph.whole.Event.End
	graph.part = create(partValue)
	if _, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePartOf, graph.part.ID, graph.whole.ID), nil); err != nil {
		t.Fatalf("표본 관계 확정: %v", err)
	}
	processor := scopeTestProcessor(embeddingDimension(t, database))
	readyIndexTasks(t, database, graphID, graph.source.ID, graph.second.ID, graph.derived.ID, graph.early.ID, graph.late.ID, graph.whole.ID, graph.part.ID)
	for {
		result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, processor)
		if err != nil {
			t.Fatalf("표본 색인: %v", err)
		}
		if !result.Found {
			break
		}
	}
	graph.expect = EmbeddingExpectation{ModelID: "scope-test", Dimension: embeddingDimension(t, database)}

	otherActor := newTestID(t)
	createTestAccount(t, database, otherActor)
	foreignGraph := createTestGraph(t, database, otherActor)
	foreign, err := database.CreateContext(t.Context(), foreignGraph, testSourceContext(t, foreignGraph, otherActor, "api://invariant/foreign/"+foreignGraph.String()), nil)
	if err != nil {
		t.Fatalf("다른 그래프 원천 생성: %v", err)
	}
	graph.foreignSource = foreign.ID
	return graph
}

// corruptEdge는 도메인 검증을 거치지 않고 간선을 만든다. 규칙이 저장 상태를 직접 보는지
// 확인하려면 쓰기 경로가 막는 상태를 저장소에 먼저 만들어야 한다.
func corruptEdge(t *testing.T, database *Store, graphID model.ID, label string, from, to model.ID, relationID string) {
	t.Helper()
	properties := "graph_id: " + cypherString(graphID.String())
	if relationID != "" {
		properties += ", relation_id: " + cypherString(relationID) + ", state: 'confirmed'"
	}
	query := "MATCH (a:Context {context_id: " + cypherString(from.String()) + "}), (b:Context {context_id: " + cypherString(to.String()) + "})" +
		" CREATE (a)-[:" + label + " {" + properties + "}]->(b) RETURN 1"
	if _, err := database.pool.Exec(t.Context(), database.cypherSQL(query, "v agtype"), pgx.QueryExecModeExec); err != nil {
		t.Fatalf("간선 훼손: %v", err)
	}
}

// TestInvariantWriteCheckAndAuditAgreeIntegration은 규칙마다 한 규칙만 깨진 저장 상태에서
// 쓰기 검사와 전체 감사가 같은 규칙 식별자와 위치를 보고하고, 감사가 저장 상태를 바꾸지
// 않으며, 그 주변부를 건드리는 쓰기가 커밋되지 않는지 확인한다.
func TestInvariantWriteCheckAndAuditAgreeIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	clean := createInvariantGraph(t, database)
	violations, err := database.AuditGraph(t.Context(), clean.graphID, clean.expect)
	if err != nil || len(violations) != 0 {
		t.Fatalf("정상 표본 감사 = %+v, %v", violations, err)
	}

	cases := map[string]struct {
		corrupt func(t *testing.T, graph invariantGraph) []model.ID
		want    func(graph invariantGraph) InvariantViolation
	}{
		RuleGraphMembership: {
			func(t *testing.T, graph invariantGraph) []model.ID {
				corruptEdge(t, database, graph.graphID, "HAS_MEMBER", graph.late.ID, graph.foreignSource, "")
				return []model.ID{graph.late.ID}
			},
			func(graph invariantGraph) InvariantViolation {
				return InvariantViolation{Rule: RuleGraphMembership, Target: TargetReference, Label: "has_member", IDs: []string{graph.late.ID.String(), graph.foreignSource.String()}}
			},
		},
		RuleEndpointLayer: {
			func(t *testing.T, graph invariantGraph) []model.ID {
				corruptEdge(t, database, graph.graphID, "DERIVED_FROM", graph.derived.ID, graph.late.ID, "")
				return []model.ID{graph.derived.ID}
			},
			func(graph invariantGraph) InvariantViolation {
				return InvariantViolation{Rule: RuleEndpointLayer, Target: TargetReference, Label: "derived_from", IDs: []string{graph.derived.ID.String(), graph.late.ID.String()}}
			},
		},
		RuleReferenceCardinality: {
			func(t *testing.T, graph invariantGraph) []model.ID {
				query := "MATCH (a:Context {context_id: " + cypherString(graph.derived.ID.String()) + "})-[e:DERIVED_FROM]->() DELETE e RETURN 1"
				if _, err := database.pool.Exec(t.Context(), database.cypherSQL(query, "v agtype"), pgx.QueryExecModeExec); err != nil {
					t.Fatalf("근거 간선 삭제: %v", err)
				}
				return []model.ID{graph.derived.ID}
			},
			func(graph invariantGraph) InvariantViolation {
				return InvariantViolation{Rule: RuleReferenceCardinality, Target: TargetContext, IDs: []string{graph.derived.ID.String()}}
			},
		},
		RuleRelationIdentity: {
			func(t *testing.T, graph invariantGraph) []model.ID {
				relationID := newTestID(t).String()
				low, high := graph.early.ID, graph.late.ID
				if low.String() < high.String() {
					low, high = high, low
				}
				corruptEdge(t, database, graph.graphID, "RELATES_TO", low, high, relationID)
				return []model.ID{low}
			},
			func(graph invariantGraph) InvariantViolation {
				low, high := graph.early.ID, graph.late.ID
				if low.String() < high.String() {
					low, high = high, low
				}
				return InvariantViolation{Rule: RuleRelationIdentity, Target: TargetRelation, Label: "relates_to", IDs: []string{"*", low.String(), high.String()}}
			},
		},
		RuleRelationAcyclic: {
			func(t *testing.T, graph invariantGraph) []model.ID {
				corruptEdge(t, database, graph.graphID, "PART_OF", graph.whole.ID, graph.part.ID, newTestID(t).String())
				return []model.ID{graph.whole.ID}
			},
			nil,
		},
		RuleRelationTime: {
			func(t *testing.T, graph invariantGraph) []model.ID {
				corruptEdge(t, database, graph.graphID, "PRECEDES", graph.late.ID, graph.early.ID, newTestID(t).String())
				return []model.ID{graph.late.ID}
			},
			func(graph invariantGraph) InvariantViolation {
				return InvariantViolation{Rule: RuleRelationTime, Target: TargetRelation, Label: "precedes", IDs: []string{"*", graph.late.ID.String(), graph.early.ID.String()}}
			},
		},
		RuleEmbeddingLink: {
			func(t *testing.T, graph invariantGraph) []model.ID {
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET model_id = 'retired-model' WHERE context_id = $1`, graph.second.ID.String()); err != nil {
					t.Fatalf("임베딩 모델 훼손: %v", err)
				}
				return []model.ID{graph.second.ID}
			},
			func(graph invariantGraph) InvariantViolation {
				return InvariantViolation{Rule: RuleEmbeddingLink, Target: TargetEmbedding, IDs: []string{graph.second.ID.String()}}
			},
		},
	}
	for rule, test := range cases {
		t.Run(rule, func(t *testing.T) {
			graph := createInvariantGraph(t, database)
			scope := test.corrupt(t, graph)
			audited, err := database.AuditGraph(t.Context(), graph.graphID, graph.expect)
			if err != nil {
				t.Fatalf("전체 감사: %v", err)
			}
			tx, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("쓰기 검사 트랜잭션: %v", err)
			}
			written, err := database.writeInvariantViolations(t.Context(), tx, graph.graphID, scope, graph.expect)
			_ = tx.Rollback(t.Context())
			if err != nil {
				t.Fatalf("쓰기 검사: %v", err)
			}
			if !reflect.DeepEqual(audited, written) {
				t.Fatalf("쓰기 검사와 전체 감사가 다르다: %+v != %+v", written, audited)
			}
			if len(audited) == 0 {
				t.Fatal("위반을 찾지 못했다")
			}
			for _, violation := range audited {
				if violation.Rule != rule || violation.GraphID != graph.graphID.String() {
					t.Fatalf("다른 규칙이나 그래프를 보고했다: %+v", violation)
				}
			}
			if test.want != nil {
				want := test.want(graph)
				got := audited[0]
				if len(want.IDs) > 0 && want.IDs[0] == "*" {
					want.IDs[0] = got.IDs[0]
				}
				want.GraphID = graph.graphID.String()
				if len(audited) != 1 || !reflect.DeepEqual(got, want) {
					t.Fatalf("위반 위치 = %+v, want %+v", audited, want)
				}
			}
			// 감사는 읽기 전용이므로 다시 감사해도 같은 결과여야 한다.
			again, err := database.AuditGraph(t.Context(), graph.graphID, graph.expect)
			if err != nil || !reflect.DeepEqual(again, audited) {
				t.Fatalf("감사가 저장 상태를 바꿨다: %+v, %v", again, err)
			}
		})
	}
}

// TestInvariantViolationBlocksWriteIntegration은 영향 주변부가 깨진 컨텍스트를 건드리는 쓰기가
// 커밋되지 않고 그 뒤 상태가 쓰기 전과 같은지 확인한다.
func TestInvariantViolationBlocksWriteIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph := createInvariantGraph(t, database)
	corruptEdge(t, database, graph.graphID, "PRECEDES", graph.late.ID, graph.early.ID, newTestID(t).String())
	_, err := database.ConfirmRelation(t.Context(), graph.graphID, confirmable(t, graph.graphID, model.RelationTypeRelatesTo, minID(graph.early.ID, graph.late.ID), maxID(graph.early.ID, graph.late.ID)), nil)
	if !errors.Is(err, ErrInvariantViolation) {
		t.Fatalf("깨진 주변부의 관계 확정 = %v, want 불변식 위반", err)
	}
	relations, _, err := database.ListContextRelations(t.Context(), graph.graphID, graph.early.ID, nil, []model.RelationType{model.RelationTypeRelatesTo}, "", 10)
	if err != nil || len(relations) != 0 {
		t.Fatalf("거부한 쓰기가 남았다: %d개, %v", len(relations), err)
	}
}

func minID(left, right model.ID) model.ID {
	if left.String() < right.String() {
		return left
	}
	return right
}

func maxID(left, right model.ID) model.ID {
	if left.String() < right.String() {
		return right
	}
	return left
}
