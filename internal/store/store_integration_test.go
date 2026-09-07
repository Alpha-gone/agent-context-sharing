package store

import (
	"errors"
	"os"
	"strings"
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
	createTestAccount(t, store, actorID)
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
	grantAccount(t, store, graphID, actorID, model.GraphGradeViewer)

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
	foreignActorID := newTestID(t)
	createTestAccount(t, store, foreignActorID)
	foreignGraph := graph
	foreignGraph.ID = foreignGraphID
	foreignGraph.Name = "foreign graph"
	foreignGraph.CreatedBy = foreignActorID
	if _, err := store.CreateGraph(t.Context(), foreignGraph); err != nil {
		t.Fatalf("다른 그래프 생성: %v", err)
	}
	grantAccount(t, store, foreignGraphID, foreignActorID, model.GraphGradeOwner)
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
	teamGraphID := newTestID(t)
	teamGraph := graph
	teamGraph.ID = teamGraphID
	teamGraph.Name = "team alpha"
	teamGraph.LastActivityAt = now.Add(time.Second)
	if _, err := store.CreateGraph(t.Context(), teamGraph); err != nil {
		t.Fatalf("팀 그래프 생성: %v", err)
	}
	teamID := newTestID(t)
	createTestTeam(t, store, teamID, actorID, false)
	grantTeam(t, store, teamGraphID, teamID, model.GraphGradeOwner)
	addTestTeamMember(t, store, teamID, actorID)
	grantTeam(t, store, graphID, teamID, model.GraphGradeOwner)

	deletedTeamGraphID := newTestID(t)
	deletedTeamGraph := graph
	deletedTeamGraph.ID = deletedTeamGraphID
	deletedTeamGraph.Name = "deleted team graph"
	if _, err := store.CreateGraph(t.Context(), deletedTeamGraph); err != nil {
		t.Fatalf("삭제된 팀 그래프 생성: %v", err)
	}
	deletedTeamID := newTestID(t)
	createTestTeam(t, store, deletedTeamID, actorID, true)
	grantTeam(t, store, deletedTeamGraphID, deletedTeamID, model.GraphGradeOwner)
	addTestTeamMember(t, store, deletedTeamID, actorID)

	listedGraphs, nextGraphCursor, err := store.ListGraphs(t.Context(), actorID, model.GraphListFilter{}, "", 10)
	if err != nil {
		t.Fatalf("계정 범위 그래프 목록 조회: %v", err)
	}
	if nextGraphCursor != "" || len(listedGraphs) != 2 {
		t.Fatalf("계정 범위 그래프 목록이 예상과 다르다: %#v, 다음 커서 %q", listedGraphs, nextGraphCursor)
	}
	grades := make(map[model.ID]model.GraphGrade, len(listedGraphs))
	for _, listedGraph := range listedGraphs {
		grades[listedGraph.ID] = listedGraph.Grade
	}
	if grades[graphID] != model.GraphGradeOwner || grades[teamGraphID] != model.GraphGradeOwner {
		t.Fatalf("직접 부여와 팀 상속의 최고 등급이 반환되지 않았다: %#v", grades)
	}
	filteredGraphs, _, err := store.ListGraphs(t.Context(), actorID, model.GraphListFilter{
		Name:   "TEAM",
		Grades: []model.GraphGrade{model.GraphGradeOwner},
	}, "", 10)
	if err != nil {
		t.Fatalf("이름·등급 필터 그래프 목록 조회: %v", err)
	}
	if len(filteredGraphs) != 1 || filteredGraphs[0].ID != teamGraphID {
		t.Fatalf("이름·등급 필터가 예상과 다르다: %#v", filteredGraphs)
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

// createTestAccount는 권한 목록 통합 테스트에 필요한 계정 행을 만든다.
func createTestAccount(t *testing.T, store *Store, accountID model.ID) {
	t.Helper()
	loginID := "test_" + strings.ReplaceAll(accountID.String(), "-", "")[5:]
	_, err := store.pool.Exec(t.Context(), `
		INSERT INTO public.account (account_id, login_id, password_hash, created_at)
		VALUES ($1, $2, 'test-password-hash', $3)`,
		accountID.String(), loginID, time.Now().UTC())
	if err != nil {
		t.Fatalf("테스트 계정 생성: %v", err)
	}
}

// grantAccount는 계정 직접 부여를 만든다.
func grantAccount(t *testing.T, store *Store, graphID, accountID model.ID, grade model.GraphGrade) {
	t.Helper()
	_, err := store.pool.Exec(t.Context(), `
		INSERT INTO public.graph_grant (graph_id, subject_type, subject_id, grade)
		VALUES ($1, 'account', $2, $3)`, graphID.String(), accountID.String(), string(grade))
	if err != nil {
		t.Fatalf("계정 등급 부여: %v", err)
	}
}

// createTestTeam은 활성 또는 삭제된 팀을 만든다.
func createTestTeam(t *testing.T, store *Store, teamID, managerID model.ID, deleted bool) {
	t.Helper()
	var deletedAt any
	if deleted {
		deletedAt = time.Now().UTC()
	}
	_, err := store.pool.Exec(t.Context(), `
		INSERT INTO public.team (team_id, name, manager_account_id, created_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5)`,
		teamID.String(), "team_"+teamID.String()[:8], managerID.String(), time.Now().UTC(), deletedAt)
	if err != nil {
		t.Fatalf("테스트 팀 생성: %v", err)
	}
}

// grantTeam은 팀 상속 등급을 만든다.
func grantTeam(t *testing.T, store *Store, graphID, teamID model.ID, grade model.GraphGrade) {
	t.Helper()
	_, err := store.pool.Exec(t.Context(), `
		INSERT INTO public.graph_grant (graph_id, subject_type, subject_id, grade)
		VALUES ($1, 'team', $2, $3)`, graphID.String(), teamID.String(), string(grade))
	if err != nil {
		t.Fatalf("팀 등급 부여: %v", err)
	}
}

// addTestTeamMember는 계정을 팀에 넣는다.
func addTestTeamMember(t *testing.T, store *Store, teamID, accountID model.ID) {
	t.Helper()
	_, err := store.pool.Exec(t.Context(), `
		INSERT INTO public.team_member (team_id, account_id) VALUES ($1, $2)`, teamID.String(), accountID.String())
	if err != nil {
		t.Fatalf("테스트 팀 구성원 추가: %v", err)
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
