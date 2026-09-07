package store

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestStoreIntegration은 마이그레이션이 적용된 실제 AGE에서 저장소의 핵심 경계를 확인한다.
func TestStoreIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 AGE 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	store, err := New(t.Context(), databaseURL, graphName)
	if err != nil {
		t.Fatalf("저장소 준비: %v", err)
	}
	defer store.Close()

	actorID := newTestID(t)
	graphID := newTestID(t)
	now := time.Now().UTC()
	graph := model.Graph{
		ID:             graphID,
		Name:           "store integration",
		CreatedBy:      actorID,
		CreatedAt:      now,
		LastActivityAt: now,
		Version:        1,
	}
	if _, err := store.CreateGraph(t.Context(), graph); err != nil {
		t.Fatalf("그래프 생성: %v", err)
	}

	source := testSourceContext(t, graphID, actorID, "https://example.test/source")
	createdSource, err := store.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	duplicate := testSourceContext(t, graphID, actorID, source.Source.Reference.Locator)
	duplicateSource, err := store.CreateContext(t.Context(), graphID, duplicate, nil)
	if err != nil {
		t.Fatalf("중복 원천 생성: %v", err)
	}
	if duplicateSource.ID != createdSource.ID {
		t.Fatalf("같은 source_ref가 기존 원천을 반환하지 않았다: %s != %s", duplicateSource.ID, createdSource.ID)
	}

	foreignGraphID := newTestID(t)
	foreignGraph := graph
	foreignGraph.ID = foreignGraphID
	foreignGraph.Name = "foreign graph"
	if _, err := store.CreateGraph(t.Context(), foreignGraph); err != nil {
		t.Fatalf("다른 그래프 생성: %v", err)
	}
	if _, err := store.Context(t.Context(), foreignGraphID, createdSource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("다른 그래프 정점이 not found로 격리되지 않았다: %v", err)
	}

	derived := testDerivedContext(t, graphID, actorID)
	createdDerived, err := store.CreateContext(t.Context(), graphID, derived, []model.ID{createdSource.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}
	firstEvent := testEventContext(t, graphID, actorID, createdSource.ID)
	createdFirstEvent, err := store.CreateContext(t.Context(), graphID, firstEvent, nil)
	if err != nil {
		t.Fatalf("첫 사건 생성: %v", err)
	}
	secondEvent := testEventContext(t, graphID, actorID, createdDerived.ID)
	secondEvent.Event.Start = createdFirstEvent.Event.Start.Add(time.Second)
	createdSecondEvent, err := store.CreateContext(t.Context(), graphID, secondEvent, nil)
	if err != nil {
		t.Fatalf("두 번째 사건 생성: %v", err)
	}
	relation := model.Relation{
		ID:            newTestID(t),
		GraphID:       graphID,
		Type:          model.RelationTypePrecedes,
		FromContextID: createdFirstEvent.ID,
		ToContextID:   createdSecondEvent.ID,
		State:         model.RelationStateProposed,
		ProposedBy:    model.ProposalSourceSystem,
		ProposedAt:    time.Now().UTC(),
	}
	if _, err := store.CreateRelation(t.Context(), graphID, relation); err != nil {
		t.Fatalf("사건 관계 생성: %v", err)
	}
	relations, nextCursor, err := store.ListRelations(t.Context(), graphID, "", 10)
	if err != nil {
		t.Fatalf("관계 목록 조회: %v", err)
	}
	if len(relations) != 1 || relations[0].GraphID != graphID || nextCursor != "" {
		t.Fatalf("요청 그래프 관계 목록이 예상과 다르다: %#v, 다음 커서 %q", relations, nextCursor)
	}
	foreignRelations, _, err := store.ListRelations(t.Context(), foreignGraphID, "", 10)
	if err != nil {
		t.Fatalf("다른 그래프 관계 목록 조회: %v", err)
	}
	if len(foreignRelations) != 0 {
		t.Fatalf("다른 그래프의 관계가 반환됐다: %#v", foreignRelations)
	}
	updated := createdDerived
	updated.Body = "갱신된 파생 본문"
	updated.Version++

	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Go(func() {
			_, err := store.UpdateContext(t.Context(), graphID, createdDerived.Version, updated)
			results <- err
		})
	}
	waitGroup.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if _, ok := errors.AsType[VersionConflictError](err); ok {
			conflicts++
			continue
		}
		t.Fatalf("동시 갱신: %v", err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("동시 갱신 결과가 성공 1건과 충돌 1건이 아니다: 성공 %d, 충돌 %d", successes, conflicts)
	}
}

// newTestID는 통합 테스트마다 서로 다른 UUIDv7 식별자를 만든다.
func newTestID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("UUIDv7 생성: %v", err)
	}
	return id
}

// testSourceContext는 source_ref 중복과 격리 검증에 쓰는 유효한 원천을 만든다.
func testSourceContext(t *testing.T, graphID, actorID model.ID, locator string) model.Context {
	t.Helper()
	now := time.Now().UTC()
	return model.Context{
		ID:             newTestID(t),
		GraphID:        graphID,
		Layer:          model.LayerSource,
		Body:           "원천 본문",
		RecordedAt:     now,
		CreatedBy:      actorID,
		CreatedByAgent: actorID,
		Version:        1,
		Source: &model.SourceAttributes{
			Reference:  model.SourceReference{Channel: model.SourceChannelAPI, Locator: locator},
			OccurredAt: now,
			OriginKind: model.OriginKindExternalContent,
		},
	}
}

// testDerivedContext는 낙관적 잠금 검증에 쓰는 유효한 파생을 만든다.
func testDerivedContext(t *testing.T, graphID, actorID model.ID) model.Context {
	t.Helper()
	return model.Context{
		ID:             newTestID(t),
		GraphID:        graphID,
		Layer:          model.LayerDerived,
		Body:           "파생 본문",
		RecordedAt:     time.Now().UTC(),
		CreatedBy:      actorID,
		CreatedByAgent: actorID,
		Version:        1,
		Derived: &model.DerivedAttributes{
			Kind:          model.DerivationKindProposition,
			EvidenceState: model.EvidenceStateObservation,
		},
	}
}

// testEventContext는 참조 간선과 사건 관계 검증에 쓰는 유효한 사건을 만든다.
func testEventContext(t *testing.T, graphID, actorID, memberID model.ID) model.Context {
	t.Helper()
	now := time.Now().UTC()
	end := new(time.Time)
	*end = now.Add(time.Minute)
	return model.Context{
		ID:             newTestID(t),
		GraphID:        graphID,
		Layer:          model.LayerEvent,
		Body:           "사건 본문",
		RecordedAt:     now,
		CreatedBy:      actorID,
		CreatedByAgent: actorID,
		Version:        1,
		Event: &model.EventAttributes{
			MemberIDs: []model.ID{memberID},
			Start:     now.Add(-time.Minute),
			End:       end,
		},
	}
}
