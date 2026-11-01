package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 순환 조회의 반환 행과 RawValues 필드 바이트 합을 측정한다.
type cycleReadStats struct{ queries, rows, bytes, fullRows int }
type cycleReadTx struct {
	pgx.Tx
	stats *cycleReadStats
}

func (tx cycleReadTx) Begin(ctx context.Context) (pgx.Tx, error) {
	nested, err := tx.Tx.Begin(ctx)
	return cycleReadTx{Tx: nested, stats: tx.stats}, err
}
func (tx cycleReadTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	full := strings.Contains(sql, "RETURN edge, startNode(edge), endNode(edge) ORDER BY")
	ids := strings.Contains(sql, "RETURN a.context_id, b.context_id")
	if err != nil || (!full && !ids) {
		return rows, err
	}
	tx.stats.queries++
	return cycleReadRows{Rows: rows, stats: tx.stats, full: full}, nil
}

type cycleReadRows struct {
	pgx.Rows
	stats *cycleReadStats
	full  bool
}

func (rows cycleReadRows) Next() bool {
	if !rows.Rows.Next() {
		return false
	}
	rows.stats.rows++
	if rows.full {
		rows.stats.fullRows++
	}
	for _, value := range rows.RawValues() {
		rows.stats.bytes += len(value)
	}
	return true
}

// 배경 사건을 대상 끝점과 분리하여 순환 검사용 전체 조회의 행·필드 바이트를 측정한다.
func cycleReadFixture(t *testing.T, database *Store, kind model.RelationType, size int) (model.ID, model.Context, model.Context, model.Context) {
	t.Helper()
	graph, from, to := relationTestEvents(t, database)
	centre := from
	centre.ID = newTestID(t)
	centre.Body = strings.Repeat("가", 8000)
	encoded, err := encodeProperties(contextProperties(centre))
	if err != nil {
		t.Fatal(err)
	}
	source := from.Event.MemberIDs[0]
	query := "MATCH (source:Context) WHERE source.context_id = " + cypherString(source.String()) + " CREATE (event:Context) SET event = " + encoded + " CREATE (event)-[:HAS_MEMBER {graph_id: " + cypherString(graph.String()) + "}]->(source)"
	if _, err := database.pool.Exec(t.Context(), database.cypherSQL(query, "created agtype"), pgx.QueryExecModeExec); err != nil {
		t.Fatal(err)
	}
	label, err := relationLabel(kind)
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < size; offset += 100 {
		var nodes, edges []string
		for range min(100, size-offset) {
			leaf := centre
			leaf.ID = newTestID(t)
			props, err := encodeProperties(contextProperties(leaf))
			if err != nil {
				t.Fatal(err)
			}
			nodes = append(nodes, props)
			relation := normalizeRelation(confirmable(t, graph, kind, centre.ID, leaf.ID))
			props, err = encodeProperties(relationProperties(relation))
			if err != nil {
				t.Fatal(err)
			}
			edges = append(edges, props)
		}
		query = "MATCH (source:Context) WHERE source.context_id = " + cypherString(source.String()) + " WITH source UNWIND [" + strings.Join(nodes, ",") + "] AS props CREATE (event:Context) SET event = props CREATE (event)-[:HAS_MEMBER {graph_id: " + cypherString(graph.String()) + "}]->(source)"
		if _, err := database.pool.Exec(t.Context(), database.cypherSQL(query, "created agtype"), pgx.QueryExecModeExec); err != nil {
			t.Fatal(err)
		}
		query = "UNWIND [" + strings.Join(edges, ",") + "] AS props MATCH (a:Context), (b:Context) WHERE a.context_id = props.from_context_id AND b.context_id = props.to_context_id AND a.graph_id = " + cypherString(graph.String()) + " AND b.graph_id = " + cypherString(graph.String()) + " CREATE (a)-[edge:" + label + "]->(b) SET edge = props"
		if _, err := database.pool.Exec(t.Context(), database.cypherSQL(query, "created agtype"), pgx.QueryExecModeExec); err != nil {
			t.Fatal(err)
		}
	}
	return graph, from, to, centre
}

func measuredCycleConfirm(t *testing.T, database *Store, graph model.ID, relation model.Relation) (model.Relation, cycleReadStats) {
	t.Helper()
	base, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Rollback(context.WithoutCancel(t.Context()))
	var stats cycleReadStats
	ctx := context.WithValue(t.Context(), writeTransactionContextKey{}, pgx.Tx(cycleReadTx{Tx: base, stats: &stats}))
	stored, err := database.ConfirmRelation(ctx, graph, relation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return stored, stats
}

func TestRelationCycleReadsIntegration(t *testing.T) {
	for _, kind := range []model.RelationType{model.RelationTypeRelatesTo, model.RelationTypePrecedes, model.RelationTypeCauses, model.RelationTypePartOf} {
		t.Run(string(kind), func(t *testing.T) {
			database := newIsolatedIntegrationStore(t)
			graph, from, to, centre := cycleReadFixture(t, database, kind, 1001)
			request := confirmable(t, graph, kind, from.ID, to.ID)
			var stored model.Relation
			check := func(name string, input model.Relation) {
				t.Helper()
				var stats cycleReadStats
				stored, stats = measuredCycleConfirm(t, database, graph, input)
				t.Logf("%s queries=%d rows=%d field_bytes=%d full_rows=%d", name, stats.queries, stats.rows, stats.bytes, stats.fullRows)
				want := 3
				if kind == model.RelationTypeRelatesTo {
					want = 0
				}
				if stats.queries != want || stats.fullRows != 0 {
					t.Errorf("순환 조회 중복/본문 적재: %+v want_queries=%d", stats, want)
				}
			}
			check("new", request)
			firstID := stored.ID
			check("confirmed", request)
			if stored.ID != firstID {
				t.Fatal("정체성 재확정 ID 변경")
			}
			if _, err := database.DiscardRelation(t.Context(), graph, stored.ID, nil); err != nil {
				t.Fatal(err)
			}
			check("discarded", request)
			if stored.ID != firstID {
				t.Fatal("폐기 재확정 ID 변경")
			}
			proposal := confirmable(t, graph, kind, from.ID, centre.ID)
			proposal.State = model.RelationStateProposed
			proposal.ConfirmedBy = model.ID{}
			proposal.ConfirmedByAgent = model.ID{}
			proposal.ConfirmedAt = nil
			proposalTx, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.createRelation(t.Context(), proposalTx, graph, normalizeRelation(proposal)); err != nil {
				proposalTx.Rollback(t.Context())
				t.Fatal(err)
			}
			if err := proposalTx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			check("proposed", confirmable(t, graph, kind, from.ID, centre.ID))
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestRelationCycleFieldsIntegration(t *testing.T) {
	for _, kind := range []model.RelationType{model.RelationTypePrecedes, model.RelationTypeCauses, model.RelationTypePartOf} {
		t.Run(string(kind), func(t *testing.T) {
			database := newIsolatedIntegrationStore(t)
			graph, from, to := relationTestEvents(t, database)
			next := attachReferences(to, nil, to.Event.MemberIDs)
			next.Version++
			next.Event.End = from.Event.End
			var err error
			to, err = database.UpdateContext(t.Context(), graph, to.Version, next)
			if err != nil {
				t.Fatal(err)
			}
			first, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, kind, from.ID, to.ID), nil)
			if err != nil {
				t.Fatal(err)
			}
			reject := func(fromID, toID model.ID) {
				t.Helper()
				_, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, kind, fromID, toID), nil)
				field, ok := errors.AsType[model.FieldError](err)
				if !errors.Is(err, ErrInvalidRelation) || !ok || field.Field != "relation_type" {
					t.Fatalf("순환 오류/필드 변경: %v", err)
				}
			}
			reject(to.ID, from.ID)
			if _, err := database.DiscardRelation(t.Context(), graph, first.ID, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, kind, to.ID, from.ID), nil); err != nil {
				t.Fatal(err)
			}
			reject(from.ID, to.ID)
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestRelationCycleSnapshotChecksOtherTypesIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, from, to := relationTestEvents(t, database)
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	for _, pair := range [][2]model.ID{{from.ID, to.ID}, {to.ID, from.ID}} {
		if _, err := database.createRelation(t.Context(), tx, graph, confirmable(t, graph, model.RelationTypeCauses, pair[0], pair[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypePrecedes, from.ID, to.ID), nil)
	if !errors.Is(err, ErrInvariantViolation) {
		t.Fatalf("다른 유형의 기존 순환을 놓침: %v", err)
	}
	if _, err := database.relationByIdentity(t.Context(), database.pool, graph, model.RelationTypePrecedes, from.ID, to.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("불변식 실패 뒤 확정 간선 잔류", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestRelationCycleReconfirmationScopesIdentityIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, a, b := relationTestEvents(t, database)
	c := a
	c.ID = newTestID(t)
	d := a
	d.ID = newTestID(t)
	var err error
	c, err = database.CreateContext(t.Context(), graph, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err = database.CreateContext(t.Context(), graph, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := confirmable(t, graph, model.RelationTypeCauses, a.ID, b.ID)
	first.State = model.RelationStateProposed
	first.ConfirmedBy = model.ID{}
	first.ConfirmedByAgent = model.ID{}
	first.ConfirmedAt = nil
	second := first
	second.FromContextID, second.ToContextID = c.ID, d.ID
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	for _, relation := range []model.Relation{first, second, confirmable(t, graph, model.RelationTypeCauses, b.ID, c.ID), confirmable(t, graph, model.RelationTypeCauses, d.ID, a.ID)} {
		if _, err := database.createRelation(t.Context(), tx, graph, relation); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypeCauses, a.ID, b.ID), nil); err != nil {
		t.Fatal(err)
	}
	untouched, err := database.relationByIdentity(t.Context(), database.pool, graph, model.RelationTypeCauses, c.ID, d.ID)
	if err != nil || untouched.State != model.RelationStateProposed {
		t.Fatal("같은 ID의 다른 정체성까지 확정됐다", err)
	}
	violations, err := database.AuditGraph(t.Context(), graph, EmbeddingExpectation{})
	if err != nil || len(violations) != 0 {
		t.Fatalf("재확정 뒤 불변식 위반: %+v/%v", violations, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}
