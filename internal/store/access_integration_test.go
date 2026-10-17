package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAccessAllLayersRoundTripIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	now := time.Now().UTC().Truncate(time.Microsecond)
	graph := model.Graph{ID: newTestID(t), Name: "DATA-03 graph", Description: "설명 ' 한글", CreatedBy: actor, CreatedAt: now, LastActivityAt: now, Version: 1}
	gotGraph, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{})
	if err != nil || !reflect.DeepEqual(gotGraph, graph) {
		t.Fatalf("그래프 왕복 = %+v, %v", gotGraph, err)
	}
	values := make([]model.Context, 0, 7)
	for _, channel := range []model.SourceChannel{model.SourceChannelConversation, model.SourceChannelFile, model.SourceChannelWeb, model.SourceChannelAPI, model.SourceChannelOther} {
		value := testSourceContext(t, graph.ID, actor, "custom://data03/"+string(channel)+"/'quoted'/$store0$/한글")
		value.Source.Reference.Channel = channel
		value.Body = "첫 줄 ' 따옴표 \\ 역슬래시 $store0$\n한글 😀"
		value.CreatedByAgent = newTestID(t)
		stored, err := database.CreateContext(t.Context(), graph.ID, value, nil)
		if err != nil || !reflect.DeepEqual(stored, value) {
			t.Fatalf("원천 쓰기 왕복 = %+v, %v", stored, err)
		}
		values = append(values, stored)
	}
	derived := testDerivedContext(t, graph.ID, actor)
	derived.Derived.ValidFrom, derived.Derived.ValidTo = new(now), new(now.Add(time.Hour))
	references := []model.ID{values[1].ID, values[0].ID}
	storedDerived, err := database.CreateContext(t.Context(), graph.ID, derived, references)
	if err != nil {
		t.Fatal(err)
	}
	derived.Derived.DerivedFrom = references
	if !reflect.DeepEqual(canonicalReferences(storedDerived), canonicalReferences(derived)) {
		t.Fatalf("파생 쓰기 왕복 = %+v", storedDerived)
	}
	values = append(values, storedDerived)
	event := testEventContext(t, graph.ID, actor, values[0].ID)
	event.Event.MemberIDs = []model.ID{values[1].ID, values[0].ID}
	storedEvent, err := database.CreateContext(t.Context(), graph.ID, event, nil)
	if err != nil || !reflect.DeepEqual(canonicalReferences(storedEvent), canonicalReferences(event)) {
		t.Fatalf("사건 쓰기 왕복 = %+v, %v", storedEvent, err)
	}
	values = append(values, storedEvent)
	ids := make([]model.ID, len(values))
	for i, want := range values {
		ids[i] = want.ID
		got, err := database.Context(t.Context(), graph.ID, want.ID)
		if err != nil || !reflect.DeepEqual(canonicalReferences(got), canonicalReferences(want)) {
			t.Fatalf("단건 %s 왕복 = %+v, %v", want.Layer, got, err)
		}
	}
	batch, err := database.ContextsByIDs(t.Context(), graph.ID, ids)
	if err != nil || len(batch) != len(values) {
		t.Fatalf("묶음 읽기 = %d, %v", len(batch), err)
	}
	for i, want := range values {
		if !reflect.DeepEqual(canonicalReferences(batch[i]), canonicalReferences(want)) {
			t.Fatalf("묶음 순서/속성 %d = %+v", i, batch[i])
		}
	}
	next, err := database.UpdateGraph(t.Context(), graph.ID, 1, "새 이름", "새 설명")
	if err != nil || next.Version != 2 {
		t.Fatalf("조건부 그래프 갱신 = %+v, %v", next, err)
	}
	if _, err := database.UpdateGraph(t.Context(), graph.ID, 1, "충돌", ""); err == nil {
		t.Fatal("과거 그래프 판이 갱신됐다")
	} else if conflict, ok := errors.AsType[VersionConflictError](err); !ok || conflict.Current != 2 {
		t.Fatalf("그래프 판 충돌 = %v", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestAccessReadsShareSnapshotIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	writer := newIntegrationStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	graph, actor, source := boundaryGraph(t, database)
	first, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	proposal := proposedRelation(graph, model.RelationTypeRelatesTo, first.ID, second.ID)
	proposal.ID = newTestID(t)
	_, err = database.WithReadSnapshot(ctx, func(scoped context.Context) error {
		if _, err := writer.CreateRelation(ctx, graph, normalizeRelation(proposal)); err != nil {
			return err
		}
		later, err := writer.CreateContext(ctx, graph, testSourceContext(t, graph, actor, "api://later/"+graph.String()), nil)
		if err != nil {
			return err
		}
		if _, err := database.Context(scoped, graph, later.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("단건이 뒤 커밋을 읽었다: %v", err)
		}
		if _, err := database.ContextsByIDs(scoped, graph, []model.ID{later.ID}); !errors.Is(err, ErrNotFound) {
			t.Errorf("묶음이 뒤 커밋을 읽었다: %v", err)
		}
		relations, _, err := database.ListRelations(scoped, graph, "", 10)
		if err != nil || len(relations) != 0 {
			t.Errorf("관계 목록이 뒤 커밋을 읽었다: %+v, %v", relations, err)
		}
		relations, _, err = database.ListContextRelations(scoped, graph, first.ID, nil, nil, "", 10)
		if err != nil || len(relations) != 0 {
			t.Errorf("사건 관계가 뒤 커밋을 읽었다: %+v, %v", relations, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	relations, _, err := database.ListRelations(t.Context(), graph, "", 10)
	if err != nil || len(relations) != 1 {
		t.Fatalf("경계 밖 관계 = %+v, %v", relations, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestAccessWriteFailureRollsBackAndHidesPayloadIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, _ := boundaryGraph(t, database)
	const payload = "data03-private-payload"
	_, err := database.pool.Exec(t.Context(), `CREATE FUNCTION public.data03_fail_index() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'data03-private-payload'; END $$; CREATE TRIGGER data03_fail_index BEFORE INSERT ON public.index_task FOR EACH ROW EXECUTE FUNCTION public.data03_fail_index()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := database.pool.Exec(context.Background(), `DROP TRIGGER data03_fail_index ON public.index_task; DROP FUNCTION public.data03_fail_index()`)
		if err != nil {
			t.Error(err)
		}
	})
	before, err := database.Graph(t.Context(), graph)
	if err != nil {
		t.Fatal(err)
	}
	value := testSourceContext(t, graph, actor, "api://rollback/"+graph.String())
	got, err := database.CreateContext(t.Context(), graph, value, nil)
	pg, pgOK := errors.AsType[*pgconn.PgError](err)
	if _, ok := errors.AsType[*StorageError](err); !ok || !pgOK || pg.Code != "P0001" || !got.ID.IsZero() {
		t.Fatalf("쓰기 장애 = %+v, %v", got, err)
	}
	if strings.Contains(err.Error(), payload) {
		t.Fatalf("DB 원문 노출: %v", err)
	}
	if _, err := database.Context(t.Context(), graph, value.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("롤백 정점이 남았다: %v", err)
	}
	after, err := database.Graph(t.Context(), graph)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("롤백 뒤 그래프 = %+v, %v", after, err)
	}
	var tasks int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.index_task WHERE context_id=$1`, value.ID.String()).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("실패한 색인 작업 = %d, %v", tasks, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}
