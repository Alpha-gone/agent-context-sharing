package store

import (
	"errors"
	"os"
	"slices"
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
	store, err := New(t.Context(), databaseURL, graphName, nil)
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
	if _, err := store.Graph(t.Context(), newTestID(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("없는 그래프 조회가 not found로 변환되지 않았다: %v", err)
	}
	if _, err := store.UpdateGraph(t.Context(), newTestID(t), 1, "없는 그래프", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("없는 그래프 갱신이 not found로 변환되지 않았다: %v", err)
	}
	grantAccount(t, store, graphID, actorID, model.GraphGradeViewer)

	source := testSourceContext(t, graphID, actorID, "https://example.test/source")
	createdSource, err := store.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	if count := indexTaskCount(t, store, createdSource.ID); count != 1 {
		t.Fatalf("원천 생성이 색인 작업을 한 행 등록하지 않았다: %d", count)
	}
	duplicate := testSourceContext(t, graphID, actorID, source.Source.Reference.Locator)
	duplicateSource, err := store.CreateContext(t.Context(), graphID, duplicate, nil)
	if err != nil {
		t.Fatalf("중복 원천 생성: %v", err)
	}
	if duplicateSource.ID != createdSource.ID {
		t.Fatalf("같은 source_ref가 기존 원천을 반환하지 않았다: %s != %s", duplicateSource.ID, createdSource.ID)
	}
	if count := indexTaskCount(t, store, createdSource.ID); count != 1 {
		t.Fatalf("원천 중복 응답이 색인 작업을 다시 등록했다: %d", count)
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
	if count := indexTaskCount(t, store, createdDerived.ID); count != 1 {
		t.Fatalf("파생 생성이 색인 작업을 한 행 등록하지 않았다: %d", count)
	}
	hops, err := store.HopContexts(t.Context(), graphID, createdSource.ID, 1, "in", []string{"derived_from"}, 10)
	if err != nil {
		t.Fatalf("참조 홉 조회: %v", err)
	}
	if len(hops.Contexts) != 2 || len(hops.Edges) != 1 || hops.Contexts[0].ID != createdSource.ID {
		t.Fatalf("참조 홉 조회 결과가 예상과 다르다: %#v", hops)
	}
	// 호출자는 평평한 목록의 위치로 거리를 복원할 수 없으므로 노드별 홉 거리를 함께 받는다.
	if hops.Distances[createdSource.ID] != 0 || hops.Distances[createdDerived.ID] != 1 {
		t.Fatalf("참조 홉 거리가 예상과 다르다: %#v", hops.Distances)
	}
	operation := OperationRecord{
		Kind: OperationDiscard, GraphID: graphID, ContextID: createdSource.ID, TargetVersion: 1,
		JudgmentInput: "integration discard", AccountID: actorID, AgentID: actorID,
	}
	discarded, err := store.DiscardContext(t.Context(), graphID, createdSource.ID, &operation)
	if err != nil {
		t.Fatalf("컨텍스트 폐기: %v", err)
	}
	if discarded.DeletedAt == nil {
		t.Fatal("폐기한 컨텍스트에 deleted_at이 없다")
	}
	if found, err := store.HasAppliedDiscard(t.Context(), graphID, createdSource.ID); err != nil || !found {
		t.Fatalf("적용된 폐기 기록을 찾지 못했다: found=%t err=%v", found, err)
	}
	invalidated, err := store.Context(t.Context(), graphID, createdDerived.ID)
	if err != nil {
		t.Fatalf("근거 폐기 뒤 파생 조회: %v", err)
	}
	if invalidated.Derived == nil || !invalidated.Derived.EvidenceInvalidated || invalidated.Version != createdDerived.Version+1 {
		t.Fatalf("근거 폐기가 파생에 무효 표시를 전파하지 않았다: %#v", invalidated.Derived)
	}
	operation.Kind = OperationUpdate
	restored, err := store.RestoreContext(t.Context(), graphID, createdSource.ID, &operation)
	if err != nil {
		t.Fatalf("컨텍스트 복구: %v", err)
	}
	if restored.DeletedAt != nil {
		t.Fatal("복구한 컨텍스트에 deleted_at이 남아 있다")
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
	truncated, err := store.HopContexts(t.Context(), graphID, createdSource.ID, 2, "", nil, 1)
	if err != nil {
		t.Fatalf("홉 조회 절단: %v", err)
	}
	if len(truncated.Contexts) != 1 || !truncated.Truncated || truncated.Boundary != 1 {
		t.Fatalf("노드 상한 초과 시 절단 상태가 예상과 다르다: contexts=%d truncated=%t boundary=%d",
			len(truncated.Contexts), truncated.Truncated, truncated.Boundary)
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
	currentDerived, err := store.Context(t.Context(), graphID, createdDerived.ID)
	if err != nil {
		t.Fatalf("갱신 전 파생 조회: %v", err)
	}
	updated := currentDerived
	updated.Body = "갱신된 파생 본문"
	updated.Version++

	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Go(func() {
			_, err := store.UpdateContext(t.Context(), graphID, currentDerived.Version, updated)
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
	if count := indexTaskCount(t, store, createdDerived.ID); count != 1 {
		t.Fatalf("본문 갱신이 색인 작업을 컨텍스트당 한 행으로 유지하지 않았다: %d", count)
	}
}

// indexTaskCount는 컨텍스트에 남은 색인 작업 행 수를 센다.
func indexTaskCount(t *testing.T, store *Store, contextID model.ID) int {
	t.Helper()
	var count int
	if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.index_task WHERE context_id = $1`, contextID.String()).Scan(&count); err != nil {
		t.Fatalf("색인 작업 행 수 조회: %v", err)
	}
	return count
}

// TestEventRelationProposals는 사건 저장 커밋 뒤 시간 인접과 구성원 진부분집합 후보가
// 제안되고 같은 정체성을 중복으로 만들지 않는지 실제 AGE에서 확인한다.
func TestEventRelationProposals(t *testing.T) {
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
	store, err := New(t.Context(), databaseURL, graphName, &RelationProposalConfig{AdjacencyWindow: time.Hour, SimilarityThreshold: 0.8, Limit: 10})
	if err != nil {
		t.Fatalf("후보 제안 저장소 준비: %v", err)
	}
	defer store.Close()

	actorID, graphID := newTestID(t), newTestID(t)
	createTestAccount(t, store, actorID)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := store.CreateGraph(t.Context(), model.Graph{ID: graphID, Name: "relation proposals", CreatedBy: actorID, CreatedAt: now, LastActivityAt: now, Version: 1}); err != nil {
		t.Fatalf("그래프 생성: %v", err)
	}
	grantAccount(t, store, graphID, actorID, model.GraphGradeEditor)

	firstSource := testSourceContext(t, graphID, actorID, "api://relation-proposal/first")
	secondSource := testSourceContext(t, graphID, actorID, "api://relation-proposal/second")
	thirdSource := testSourceContext(t, graphID, actorID, "api://relation-proposal/third")
	thirdSource.Source.OccurredAt = now.Add(2*time.Minute + 30*time.Second)
	for _, source := range []model.Context{firstSource, secondSource, thirdSource} {
		if _, err := store.CreateContext(t.Context(), graphID, source, nil); err != nil {
			t.Fatalf("후보 제안 원천 생성: %v", err)
		}
	}

	whole := testEventContext(t, graphID, actorID, firstSource.ID)
	whole.Event.MemberIDs = []model.ID{firstSource.ID, secondSource.ID}
	whole.Event.Start = now.Add(-time.Minute)
	wholeEnd := now.Add(time.Minute)
	whole.Event.End = &wholeEnd
	if _, err := store.CreateContext(t.Context(), graphID, whole, nil); err != nil {
		t.Fatalf("전체 사건 생성: %v", err)
	}
	part := testEventContext(t, graphID, actorID, firstSource.ID)
	part.Event.Start = now.Add(-30 * time.Second)
	partEnd := now.Add(30 * time.Second)
	part.Event.End = &partEnd
	createdPart, err := store.CreateContext(t.Context(), graphID, part, nil)
	if err != nil {
		t.Fatalf("부분 사건 생성: %v", err)
	}
	following := testEventContext(t, graphID, actorID, thirdSource.ID)
	following.Event.Start = now.Add(2 * time.Minute)
	followingEnd := now.Add(3 * time.Minute)
	following.Event.End = &followingEnd
	createdFollowing, err := store.CreateContext(t.Context(), graphID, following, nil)
	if err != nil {
		t.Fatalf("후속 사건 생성: %v", err)
	}

	relations, _, err := store.ListRelations(t.Context(), graphID, "", 20)
	if err != nil {
		t.Fatalf("후보 관계 조회: %v", err)
	}
	partRelation, found := proposedRelationByIdentity(relations, model.RelationTypePartOf, createdPart.ID, whole.ID)
	if !found {
		t.Fatalf("구성원 진부분집합 part_of 후보가 없다: %#v", relations)
	}
	precedesRelation, found := proposedRelationByIdentity(relations, model.RelationTypePrecedes, whole.ID, createdFollowing.ID)
	if !found {
		t.Fatalf("시간 인접 precedes 후보가 없다: %#v", relations)
	}
	if _, err := store.DiscardRelation(t.Context(), graphID, partRelation.ID, nil); err != nil {
		t.Fatalf("부분집합 후보 폐기: %v", err)
	}
	confirmedAt := time.Now().UTC()
	if _, err := store.ConfirmRelation(t.Context(), graphID, model.Relation{
		ID: newTestID(t), GraphID: graphID, Type: model.RelationTypePrecedes, FromContextID: whole.ID, ToContextID: createdFollowing.ID,
		State: model.RelationStateConfirmed, ProposedBy: model.ProposalSourceAgent, ProposedAt: confirmedAt,
		ConfirmedBy: actorID, ConfirmedByAgent: actorID, ConfirmedAt: &confirmedAt,
	}, nil); err != nil {
		t.Fatalf("시간 인접 후보 확정: %v", err)
	}
	before := len(relations)
	if err := store.ProposeEventRelations(t.Context(), graphID, createdFollowing.ID); err != nil {
		t.Fatalf("후속 사건 후보 재계산: %v", err)
	}
	relations, _, err = store.ListRelations(t.Context(), graphID, "", 20)
	if err != nil {
		t.Fatalf("후보 관계 재조회: %v", err)
	}
	if len(relations) != before {
		t.Fatalf("동일한 후보를 중복 생성했다: before=%d after=%d", before, len(relations))
	}
	if relation, found := relationByID(relations, partRelation.ID); !found || relation.State != model.RelationStateDiscarded {
		t.Fatalf("폐기한 후보가 다시 제안됐다: %#v", relation)
	}
	if relation, found := relationByID(relations, precedesRelation.ID); !found || relation.State != model.RelationStateConfirmed {
		t.Fatalf("확정한 후보가 다시 제안됐다: %#v", relation)
	}
}

func proposedRelationByIdentity(relations []model.Relation, relationType model.RelationType, fromID, toID model.ID) (model.Relation, bool) {
	for _, relation := range relations {
		if relation.Type == relationType && relation.FromContextID == fromID && relation.ToContextID == toID && relation.State == model.RelationStateProposed && relation.ProposedBy == model.ProposalSourceSystem {
			return relation, true
		}
	}
	return model.Relation{}, false
}

func relationByID(relations []model.Relation, relationID model.ID) (model.Relation, bool) {
	for _, relation := range relations {
		if relation.ID == relationID {
			return relation, true
		}
	}
	return model.Relation{}, false
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

// TestHopEdgeOrderIsDeterministic은 홉 응답의 참조·관계 순서가 양 끝 context_id
// 오름차순으로 고정되고 같은 요청이 같은 순서를 주는지 확인한다. 순서를 고정하지
// 않으면 SRS의 「정렬」이 근거로 든 반복 측정과 통제 비교가 성립하지 않는다.
func TestHopEdgeOrderIsDeterministic(t *testing.T) {
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
	store, err := New(t.Context(), databaseURL, graphName, nil)
	if err != nil {
		t.Fatalf("저장소 준비: %v", err)
	}
	defer store.Close()

	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	graphID := newTestID(t)
	now := time.Now().UTC()
	if _, err := store.CreateGraph(t.Context(), model.Graph{
		ID: graphID, Name: "hop order", CreatedBy: actorID, CreatedAt: now, LastActivityAt: now, Version: 1,
	}); err != nil {
		t.Fatalf("그래프 생성: %v", err)
	}
	source, err := store.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "api://hop-order"), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	// 간선이 하나면 맵 순회 순서가 드러나지 않으므로 여럿을 만든다.
	for range 5 {
		if _, err := store.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID}); err != nil {
			t.Fatalf("파생 생성: %v", err)
		}
	}

	first, err := store.HopContexts(t.Context(), graphID, source.ID, 1, "in", []string{"derived_from"}, 10)
	if err != nil {
		t.Fatalf("홉 조회: %v", err)
	}
	if len(first.Edges) != 5 {
		t.Fatalf("참조 간선 수 = %d, want 5", len(first.Edges))
	}
	if !slices.IsSortedFunc(first.Edges, compareHopEdges) {
		t.Fatalf("참조 간선이 양 끝 context_id 오름차순이 아니다: %s", formatHopEdges(first.Edges))
	}
	for range 4 {
		repeated, err := store.HopContexts(t.Context(), graphID, source.ID, 1, "in", []string{"derived_from"}, 10)
		if err != nil {
			t.Fatalf("홉 재조회: %v", err)
		}
		if !slices.Equal(repeated.Edges, first.Edges) {
			t.Fatalf("같은 요청이 다른 간선 순서를 돌려줬다: %s != %s", formatHopEdges(repeated.Edges), formatHopEdges(first.Edges))
		}
		if !slices.EqualFunc(repeated.Contexts, first.Contexts, func(left, right model.Context) bool { return left.ID == right.ID }) {
			t.Fatal("같은 요청이 다른 컨텍스트 순서를 돌려줬다")
		}
	}
}

// formatHopEdges는 실패 메시지에 식별자를 바이트가 아니라 읽을 수 있는 형태로 남긴다.
func formatHopEdges(edges []HopEdge) string {
	values := make([]string, 0, len(edges))
	for _, edge := range edges {
		values = append(values, edge.FromID.String()+"->"+edge.ToID.String()+":"+edge.Kind)
	}
	return "[" + strings.Join(values, " ") + "]"
}

// TestEventRelationProposalRules는 제안 규칙 중 기존 테스트가 덮지 않던 넷을 확인한다.
// 구성원이 같은 사건을 part_of 후보로 올리지 않는지, 신호별 후보 수 상한이 자르는지,
// 진행 중 사건이 후속 후보가 되는지, 갱신 경로에서 제안이 다시 도는지다.
func TestEventRelationProposalRules(t *testing.T) {
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
	// 상한이 자르는지 보려면 상한이 후보 수보다 작아야 하므로 1로 둔다.
	store, err := New(t.Context(), databaseURL, graphName, &RelationProposalConfig{
		AdjacencyWindow: time.Hour, SimilarityThreshold: 0.8, Limit: 1,
	})
	if err != nil {
		t.Fatalf("후보 제안 저장소 준비: %v", err)
	}
	defer store.Close()

	actorID, graphID := newTestID(t), newTestID(t)
	createTestAccount(t, store, actorID)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := store.CreateGraph(t.Context(), model.Graph{
		ID: graphID, Name: "proposal rules", CreatedBy: actorID, CreatedAt: now, LastActivityAt: now, Version: 1,
	}); err != nil {
		t.Fatalf("그래프 생성: %v", err)
	}
	grantAccount(t, store, graphID, actorID, model.GraphGradeEditor)

	// 구성원 원천의 발생 시각은 그 구성원을 담는 사건의 시간 범위 안에 있어야 한다.
	first := proposalSource(t, store, graphID, actorID, "first", now)
	second := proposalSource(t, store, graphID, actorID, "second", now.Add(30*time.Second))
	third := proposalSource(t, store, graphID, actorID, "third", now.Add(2*time.Minute+30*time.Second))
	fourth := proposalSource(t, store, graphID, actorID, "fourth", now.Add(4*time.Minute+30*time.Second))

	whole := proposalEvent(t, store, graphID, actorID, []model.ID{first, second}, now, ptrTime(now.Add(time.Minute)))
	twin := proposalEvent(t, store, graphID, actorID, []model.ID{first, second}, now, ptrTime(now.Add(time.Minute)))
	relations := proposalRelations(t, store, graphID)
	for _, relation := range relations {
		if relation.Type == model.RelationTypePartOf {
			t.Fatalf("구성원이 같은 사건을 part_of 후보로 올렸다: %#v", relation)
		}
	}

	// 앞의 두 사건과 모두 인접한 사건을 만든다. 후보는 둘인데 상한이 1이므로 하나만 남는다.
	before := len(relations)
	following := proposalEvent(t, store, graphID, actorID, []model.ID{third}, now.Add(2*time.Minute), ptrTime(now.Add(3*time.Minute)))
	relations = proposalRelations(t, store, graphID)
	if added := len(relations) - before; added != 1 {
		t.Fatalf("신호별 후보 수 상한이 적용되지 않았다: 새 후보 %d개", added)
	}

	// 진행 중 사건은 종료 시각이 없어도 앞선 사건의 후속 후보가 된다.
	ongoing := proposalEvent(t, store, graphID, actorID, []model.ID{fourth}, now.Add(4*time.Minute), nil)
	relations = proposalRelations(t, store, graphID)
	if _, found := proposedRelationByIdentity(relations, model.RelationTypePrecedes, following.ID, ongoing.ID); !found {
		t.Fatalf("진행 중 사건이 후속 후보로 오르지 않았다: %#v", relations)
	}

	// 구성원을 줄이면 진부분집합이 되므로 갱신 경로가 제안을 다시 돌려야 새 후보가 생긴다.
	if _, found := proposedRelationByIdentity(relations, model.RelationTypePartOf, twin.ID, whole.ID); found {
		t.Fatal("갱신 전에 이미 part_of 후보가 있다")
	}
	narrowed := twin
	narrowed.Version = twin.Version + 1
	narrowed.Event = &model.EventAttributes{MemberIDs: []model.ID{first}, Start: twin.Event.Start, End: twin.Event.End}
	if _, err := store.UpdateContext(t.Context(), graphID, twin.Version, narrowed); err != nil {
		t.Fatalf("사건 구성원 갱신: %v", err)
	}
	relations = proposalRelations(t, store, graphID)
	if _, found := proposedRelationByIdentity(relations, model.RelationTypePartOf, twin.ID, whole.ID); !found {
		t.Fatalf("갱신 경로에서 제안이 다시 돌지 않았다: %#v", relations)
	}
}

// proposalSource는 지정한 발생 시각을 가진 원천을 만들어 식별자를 돌려준다.
func proposalSource(t *testing.T, store *Store, graphID, actorID model.ID, name string, occurredAt time.Time) model.ID {
	t.Helper()
	source := testSourceContext(t, graphID, actorID, "api://proposal-rules/"+name)
	source.Source.OccurredAt = occurredAt
	created, err := store.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("%s 원천 생성: %v", name, err)
	}
	return created.ID
}

// proposalEvent는 지정한 구성원과 시간 범위를 가진 사건을 만든다.
func proposalEvent(t *testing.T, store *Store, graphID, actorID model.ID, members []model.ID, start time.Time, end *time.Time) model.Context {
	t.Helper()
	event := testEventContext(t, graphID, actorID, members[0])
	event.Event = &model.EventAttributes{MemberIDs: members, Start: start, End: end}
	created, err := store.CreateContext(t.Context(), graphID, event, nil)
	if err != nil {
		t.Fatalf("사건 생성: %v", err)
	}
	return created
}

// proposalRelations는 그래프의 모든 관계를 읽는다.
func proposalRelations(t *testing.T, store *Store, graphID model.ID) []model.Relation {
	t.Helper()
	relations, err := store.allRelations(t.Context(), store.pool, graphID, model.ID{}, nil, nil)
	if err != nil {
		t.Fatalf("후보 조회: %v", err)
	}
	return relations
}

func ptrTime(value time.Time) *time.Time { return &value }

// TestHopContextsTreatsZeroLimitAsNoCap은 플랜의 max_hop_nodes 0이 한도 없음으로
// 동작하는지 확인한다. 거부하면 한도를 푸는 설정이 node_get을 internal로 만든다.
func TestHopContextsTreatsZeroLimitAsNoCap(t *testing.T) {
	store := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	graphID := createTestGraph(t, store, actorID)

	source := testSourceContext(t, graphID, actorID, "api://zero-limit/"+newTestID(t).String())
	createdSource, err := store.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	for range 3 {
		if _, err := store.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{createdSource.ID}); err != nil {
			t.Fatalf("파생 생성: %v", err)
		}
	}

	capped, err := store.HopContexts(t.Context(), graphID, createdSource.ID, 1, "in", []string{"derived_from"}, 2)
	if err != nil {
		t.Fatalf("상한 조회: %v", err)
	}
	if len(capped.Contexts) != 2 || !capped.Truncated {
		t.Fatalf("상한이 적용되지 않았다: %d개, 절단 %v", len(capped.Contexts), capped.Truncated)
	}
	unlimited, err := store.HopContexts(t.Context(), graphID, createdSource.ID, 1, "in", []string{"derived_from"}, 0)
	if err != nil {
		t.Fatalf("무제한 조회: %v", err)
	}
	if len(unlimited.Contexts) != 4 || unlimited.Truncated {
		t.Fatalf("무제한 조회 결과 = %d개, 절단 %v", len(unlimited.Contexts), unlimited.Truncated)
	}
}

// TestHopContextsFromSharesOneCapAcrossStarts는 시작 노드를 모아 한 번에 확장할 때
// 결과 상한과 절단이 시작 노드마다 겹치지 않고 하나로 적용되는지 확인한다. 거리는
// 가장 가까운 시작 노드까지의 최단 홉 거리다.
func TestHopContextsFromSharesOneCapAcrossStarts(t *testing.T) {
	store := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	graphID := createTestGraph(t, store, actorID)

	starts := make([]model.Context, 0, 2)
	derivedIDs := make([]model.ID, 0, 4)
	for range 2 {
		source := testSourceContext(t, graphID, actorID, "api://shared-cap/"+newTestID(t).String())
		createdSource, err := store.CreateContext(t.Context(), graphID, source, nil)
		if err != nil {
			t.Fatalf("원천 생성: %v", err)
		}
		starts = append(starts, createdSource)
		for range 2 {
			derived, err := store.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{createdSource.ID})
			if err != nil {
				t.Fatalf("파생 생성: %v", err)
			}
			derivedIDs = append(derivedIDs, derived.ID)
		}
	}

	all, err := store.HopContextsFrom(t.Context(), graphID, starts, 1, "in", []string{"derived_from"}, 0)
	if err != nil {
		t.Fatalf("다중 시작 확장: %v", err)
	}
	if len(all.Contexts) != 6 || all.Truncated {
		t.Fatalf("다중 시작 확장 결과 = %d개, 절단 %v", len(all.Contexts), all.Truncated)
	}
	for _, start := range starts {
		if all.Distances[start.ID] != 0 {
			t.Fatalf("시작 노드 거리 = %d, want 0", all.Distances[start.ID])
		}
	}
	for _, derivedID := range derivedIDs {
		if all.Distances[derivedID] != 1 {
			t.Fatalf("파생 거리 = %d, want 1", all.Distances[derivedID])
		}
	}

	// 상한 셋은 시작 노드 둘을 담고 남은 한 자리만 확장에 쓴다. 시작 노드마다 상한을
	// 따로 적용하면 여섯 개가 모두 들어와 어느 것이 잘랐는지 알 수 없게 된다.
	capped, err := store.HopContextsFrom(t.Context(), graphID, starts, 1, "in", []string{"derived_from"}, 3)
	if err != nil {
		t.Fatalf("상한 적용 확장: %v", err)
	}
	if len(capped.Contexts) != 3 || !capped.Truncated || capped.Boundary != 1 {
		t.Fatalf("공유 상한 결과 = %d개, 절단 %v, 경계 %d", len(capped.Contexts), capped.Truncated, capped.Boundary)
	}

	// 시작 노드 수가 이미 상한을 넘으면 확장 전에 자른 것이므로 경계는 0이다.
	startsOnly, err := store.HopContextsFrom(t.Context(), graphID, starts, 1, "in", []string{"derived_from"}, 1)
	if err != nil {
		t.Fatalf("시작 노드 상한 확장: %v", err)
	}
	if len(startsOnly.Contexts) != 1 || !startsOnly.Truncated || startsOnly.Boundary != 0 {
		t.Fatalf("시작 노드 상한 결과 = %d개, 절단 %v, 경계 %d", len(startsOnly.Contexts), startsOnly.Truncated, startsOnly.Boundary)
	}
}
