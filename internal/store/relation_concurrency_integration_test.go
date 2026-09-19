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

// TestRelationRejectsDiscardedEventIntegration은 폐기된 사건을 끝으로 갖는 관계 확정이
// 거부되는지 확인한다. FR-AGENT_CONTEXT-076이 사건을 폐기하면 그 확정 관계도 함께
// 폐기하기로 했으므로, 새로 만들면 그 규칙이 곧바로 깨진 상태가 된다.
func TestRelationRejectsDiscardedEventIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graphID, first, second := relationTestEvents(t, database)
	if _, err := database.DiscardContext(t.Context(), graphID, second.ID, nil, WriteLimits{}); err != nil {
		t.Fatalf("사건 폐기: %v", err)
	}
	err := func() error {
		_, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, first.ID, second.ID), nil)
		return err
	}()
	if !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("폐기된 사건을 끝으로 하는 확정 = %v, want ErrInvalidRelation", err)
	}
}

// TestHopResultsCarryReferencesIntegration은 홉 확장으로 가져온 파생과 사건이 근거와
// 구성원 목록을 담는지 확인한다. 비우면 흐름 응답의 확장 노드가 빈 목록으로 나가고
// 중복 파생 접기가 근거를 구분하지 못한다.
func TestHopResultsCarryReferencesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/hop-ref-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}
	event, err := database.CreateContext(t.Context(), graphID, testEventContext(t, graphID, actorID, source.ID), nil)
	if err != nil {
		t.Fatalf("사건 생성: %v", err)
	}

	// 원천에서 들어오는 방향으로 확장하면 파생과 사건이 확장분으로 들어온다.
	hops, err := database.HopContexts(t.Context(), graphID, source.ID, 2, "both", nil, 0)
	if err != nil {
		t.Fatalf("홉 탐색: %v", err)
	}
	seenDerived, seenEvent := false, false
	for _, value := range hops.Contexts {
		switch value.ID {
		case derived.ID:
			seenDerived = true
			if got := value.Derived.DerivedFrom; len(got) != 1 || got[0] != source.ID {
				t.Fatalf("확장 파생의 근거 = %v, want [%s]", got, source.ID)
			}
		case event.ID:
			seenEvent = true
			if got := value.Event.MemberIDs; len(got) != 1 || got[0] != source.ID {
				t.Fatalf("확장 사건의 구성원 = %v, want [%s]", got, source.ID)
			}
		}
	}
	if !seenDerived || !seenEvent {
		t.Fatalf("확장 결과에 파생 %t, 사건 %t", seenDerived, seenEvent)
	}
}
