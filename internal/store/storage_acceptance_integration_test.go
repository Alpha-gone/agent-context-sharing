package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

func TestDATA10MixedStorageContractsIntegration(t *testing.T) {
	first, second, admin := newAgentContractDatabase(t)
	graph, actor, source := boundaryGraph(t, first)
	createDerived := func(body string) model.Context {
		value := testDerivedContext(t, graph, actor)
		value.Body = body
		stored, err := first.CreateContext(t.Context(), graph, value, []model.ID{source.ID})
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	updated := createDerived("전")
	var discarded []model.Context
	for range 2 {
		value := createDerived("복")
		value, err := first.DiscardContext(t.Context(), graph, value.ID, registrationOperation(value, OperationDiscard), WriteLimits{})
		if err != nil {
			t.Fatal(err)
		}
		discarded = append(discarded, value)
	}
	var events []model.Context
	for range 2 {
		value, err := first.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, value)
	}
	var base int64
	if err := admin.QueryRow(t.Context(), "SELECT stored_chars FROM public.context_graph WHERE graph_id=$1", graph.String()).Scan(&base); err != nil {
		t.Fatal(err)
	}
	limits := WriteLimits{StoredCharsPerGraph: base + 1}
	type result struct {
		kind string
		id   model.ID
		err  error
	}
	type request struct {
		kind string
		run  func(context.Context) (model.ID, error)
	}
	var requests []request
	for i, database := range []*Store{first, second} {
		duplicate := source
		duplicate.ID, duplicate.Body = newTestID(t), "중복 본문"
		next := updated
		next.Version++
		next.Body = []string{"가", "나"}[i]
		relation, operation := lifeRelationRequest(t, events[0], events[1], model.RelationTypePrecedes)
		restored := discarded[i]
		requests = append(requests,
			request{"source", func(ctx context.Context) (model.ID, error) {
				value, err := database.CreateContextWithOperation(ctx, graph, duplicate, nil, registrationOperation(duplicate, OperationAdd), limits)
				return value.ID, err
			}},
			request{"relation", func(ctx context.Context) (model.ID, error) {
				value, err := database.ConfirmRelation(ctx, graph, relation, operation)
				return value.ID, err
			}},
			request{"update", func(ctx context.Context) (model.ID, error) {
				value, err := database.UpdateContextWithOperation(ctx, graph, updated.Version, next, registrationOperation(next, OperationUpdate), limits)
				return value.ID, err
			}},
			request{"restore", func(ctx context.Context) (model.ID, error) {
				value, err := database.RestoreContext(ctx, graph, restored.ID, registrationOperation(restored, OperationUpdate), limits)
				return value.ID, err
			}},
		)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	gate, err := admin.Begin(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var group sync.WaitGroup
	defer func() { cancel(); rollbackTransaction(ctx, gate); group.Wait() }()
	if _, err := gate.Exec(ctx, "SELECT 1 FROM public.context_graph WHERE graph_id=$1 FOR UPDATE", graph.String()); err != nil {
		t.Fatal(err)
	}
	results := make(chan result, len(requests))
	for _, task := range requests {
		group.Go(func() {
			id, err := task.run(ctx)
			results <- result{task.kind, id, err}
		})
	}
	// 두 연결의 실제 잠금 대기가 관측된 뒤 놓아 순차 실행의 우연한 통과를 막는다.
	for {
		var waiters int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'").Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters >= 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	group.Wait()
	close(results)
	successes, rejections := map[string]int{}, map[string]int{}
	var relationID model.ID
	for got := range results {
		if got.err == nil {
			successes[got.kind]++
			if got.kind == "source" && got.id != source.ID {
				t.Fatal("자연 키 중복이 다른 원천을 생성했다")
			}
			if got.kind == "relation" {
				if !relationID.IsZero() && got.id != relationID {
					t.Fatal("같은 관계 정체성이 다른 간선을 반환했다")
				}
				relationID = got.id
			}
			continue
		}
		if conflict, ok := errors.AsType[VersionConflictError](got.err); got.kind == "update" && ok && conflict.Current == updated.Version+1 {
			rejections[got.kind]++
		} else if limit, ok := errors.AsType[plan.LimitError](got.err); got.kind == "restore" && ok && limit.Current == base+1 && limit.Allowed == base+1 {
			rejections[got.kind]++
		} else {
			t.Fatalf("%s 혼합 요청: %v (원인: %v)", got.kind, got.err, errors.Unwrap(got.err))
		}
	}
	for kind, want := range map[string]int{"source": 2, "relation": 2, "update": 1, "restore": 1} {
		if successes[kind] != want {
			t.Fatalf("%s 성공=%d, want=%d", kind, successes[kind], want)
		}
	}
	if rejections["update"] != 1 || rejections["restore"] != 1 {
		t.Fatalf("판 충돌·한도 거부=%v", rejections)
	}
	assertPlanStoredChars(t, first, graph, base+1)
	relations, _, err := second.ListRelations(t.Context(), graph, "", 10)
	if err != nil || len(relations) != 1 || relations[0].ID != relationID {
		t.Fatalf("최종 관계 정체성=%+v: %v", relations, err)
	}
	current, err := second.Context(t.Context(), graph, updated.ID)
	if err != nil || current.Version != updated.Version+1 || utf8.RuneCountInString(current.Body) != 1 {
		t.Fatalf("최종 갱신=%+v: %v", current, err)
	}
	before := lifeAcceptanceFingerprint(t, first)
	cause := errors.New("DATA-10 커밋 전 장애")
	_, err = second.ExecuteWrite(t.Context(), actor, "data10_rollback_key_1", "node_update", []byte(`{"data10":"rollback"}`), writeReplayCheck, func(ctx context.Context) ([]byte, error) {
		next := current
		next.Version++
		next.Body = "장애 본문"
		if _, err := second.UpdateContextWithOperation(ctx, graph, current.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{}); err != nil {
			return nil, err
		}
		return nil, cause
	})
	if !errors.Is(err, cause) || before != lifeAcceptanceFingerprint(t, first) {
		t.Fatalf("장애 뒤 정점·간선·판·저장량·큐·감사·재생 상태 보존 실패: %v", err)
	}
	if violations, err := first.AuditGraph(t.Context(), graph, EmbeddingExpectation{}); err != nil || len(violations) != 0 {
		t.Fatalf("혼합 요청 뒤 전체 감사=%+v: %v", violations, err)
	}
	assertConnectionRecovery(t, first, nil, 0)
	assertConnectionRecovery(t, second, nil, 0)
}
