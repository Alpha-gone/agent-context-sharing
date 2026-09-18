package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// relationTestEvents는 관계 확정 검사에 쓸 그래프와 두 사건을 만든다.
//
// 두 사건의 시작 시각을 같게 둔다. precedes의 시간 제약은 시작이 대상보다 늦을 때만
// 거부하므로, 같은 시각이면 양방향이 모두 시간 검증을 지나 순환 검사만 남는다.
func relationTestEvents(t *testing.T, database *Store) (model.ID, model.Context, model.Context) {
	t.Helper()
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/relation-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	first, err := database.CreateContext(t.Context(), graphID, testEventContext(t, graphID, actorID, source.ID), nil)
	if err != nil {
		t.Fatalf("첫 사건 생성: %v", err)
	}
	secondValue := testEventContext(t, graphID, actorID, source.ID)
	secondValue.Event.Start = first.Event.Start
	second, err := database.CreateContext(t.Context(), graphID, secondValue, nil)
	if err != nil {
		t.Fatalf("두 번째 사건 생성: %v", err)
	}
	return graphID, first, second
}

// confirmable은 두 사건을 잇는 확정 요청을 만든다.
func confirmable(t *testing.T, graphID model.ID, relationType model.RelationType, fromID, toID model.ID) model.Relation {
	t.Helper()
	now := time.Now().UTC()
	actorID := newTestID(t)
	confirmedAt := new(time.Time)
	*confirmedAt = now
	return model.Relation{
		ID: newTestID(t), GraphID: graphID, Type: relationType, FromContextID: fromID, ToContextID: toID,
		State: model.RelationStateConfirmed, ProposedBy: model.ProposalSourceAgent, ProposedAt: now,
		ConfirmedBy: actorID, ConfirmedByAgent: actorID, ConfirmedAt: confirmedAt,
	}
}

// TestConcurrentRelationConfirmCreatesOneEdgeIntegration은 같은 정체성의 동시 확정이
// 간선을 하나만 만드는지 확인한다. AGE 간선에는 유일 인덱스가 없어 조회 후 생성만으로는
// 중복이 생기며, SRS는 정체성 재확정을 멱등으로 확정했다.
func TestConcurrentRelationConfirmCreatesOneEdgeIntegration(t *testing.T) {
	database := newIntegrationStore(t)

	// 경합 구간이 짧으므로 회차를 반복한다. 한 번만 돌리면 잠금 없이도 우연히 통과한다.
	const rounds, requests = 10, 4
	for round := range rounds {
		graphID, first, second := relationTestEvents(t, database)
		var waitGroup sync.WaitGroup
		results := make(chan error, requests)
		start := make(chan struct{})
		for range requests {
			waitGroup.Go(func() {
				<-start
				_, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, first.ID, second.ID), nil)
				results <- err
			})
		}
		close(start)
		waitGroup.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatalf("%d회차 동시 관계 확정: %v", round, err)
			}
		}

		relations, _, err := database.ListRelations(t.Context(), graphID, "", 50)
		if err != nil {
			t.Fatalf("%d회차 관계 목록 조회: %v", round, err)
		}
		confirmed := 0
		for _, relation := range relations {
			if relation.Type == model.RelationTypePrecedes && relation.FromContextID == first.ID && relation.ToContextID == second.ID {
				confirmed++
			}
		}
		if confirmed != 1 {
			t.Fatalf("%d회차 동시 확정 뒤 같은 정체성의 간선 수 = %d, want 1", round, confirmed)
		}
	}
}

// TestConcurrentOppositeRelationConfirmRejectsCycleIntegration은 A→B와 B→A를 동시에
// 확정해도 순환이 확정되지 않는지 확인한다. 두 검사가 서로의 미커밋 간선을 보지 못하면
// 둘 다 통과한다.
func TestConcurrentOppositeRelationConfirmRejectsCycleIntegration(t *testing.T) {
	database := newIntegrationStore(t)

	const rounds = 10
	for round := range rounds {
		graphID, first, second := relationTestEvents(t, database)
		var waitGroup sync.WaitGroup
		results := make(chan error, 2)
		start := make(chan struct{})
		for _, pair := range [][2]model.Context{{first, second}, {second, first}} {
			waitGroup.Go(func() {
				<-start
				_, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, pair[0].ID, pair[1].ID), nil)
				results <- err
			})
		}
		close(start)
		waitGroup.Wait()
		close(results)

		succeeded, rejected := 0, 0
		for err := range results {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrInvalidRelation):
				rejected++
			default:
				t.Fatalf("%d회차 반대 방향 동시 확정: %v", round, err)
			}
		}
		if succeeded != 1 || rejected != 1 {
			t.Fatalf("%d회차 반대 방향 확정 = 성공 %d, 거부 %d; want 각각 1", round, succeeded, rejected)
		}
	}
}

// TestReconfirmDiscardedRelationChecksCycleIntegration은 폐기한 관계의 재확정도 순환
// 검사를 지나는지 확인한다. 이 분기를 건너뛰면 폐기와 재확정의 조합으로 순환이 들어온다.
func TestReconfirmDiscardedRelationChecksCycleIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graphID, first, second := relationTestEvents(t, database)

	forward, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, first.ID, second.ID), nil)
	if err != nil {
		t.Fatalf("정방향 확정: %v", err)
	}
	if _, err := database.DiscardRelation(t.Context(), graphID, forward.ID, nil); err != nil {
		t.Fatalf("정방향 폐기: %v", err)
	}
	if _, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, second.ID, first.ID), nil); err != nil {
		t.Fatalf("역방향 확정: %v", err)
	}
	// 정방향을 다시 확정하면 순환이 된다. 새 관계였다면 거부되는 형태이므로 재확정도
	// 같은 결과여야 한다.
	err = func() error {
		_, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, first.ID, second.ID), nil)
		return err
	}()
	if !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("순환이 되는 재확정 = %v, want ErrInvalidRelation", err)
	}
}
