package store

import (
	"math"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestGraphVisualizationKeepsEdgesInsideResult는 시각화 응답의 연결선이 응답에 있는
// 컨텍스트만 가리키는지 확인한다. 소프트 삭제된 컨텍스트를 한쪽 끝으로 둔 간선이 남으면
// Cytoscape.js가 그 간선에서 예외를 던져 화면의 시각화 전체가 그려지지 않는다.
func TestGraphVisualizationKeepsEdgesInsideResult(t *testing.T) {
	store := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	graphID := createTestGraph(t, store, actorID)

	source := testSourceContext(t, graphID, actorID, "api://viz-edges/"+newTestID(t).String())
	createdSource, err := store.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	derived, err := store.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{createdSource.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}

	hops, err := store.GraphVisualization(t.Context(), graphID, 4, 200)
	if err != nil {
		t.Fatalf("시각화 조회: %v", err)
	}
	if len(hops.Contexts) != 2 || len(hops.Edges) != 1 {
		t.Fatalf("삭제 전 시각화 = 컨텍스트 %d개, 연결선 %d개", len(hops.Contexts), len(hops.Edges))
	}
	assertEdgesInsideResult(t, hops)

	if _, err := store.SetContextDeleted(t.Context(), graphID, createdSource.ID, actorID, true); err != nil {
		t.Fatalf("원천 소프트 삭제: %v", err)
	}
	hops, err = store.GraphVisualization(t.Context(), graphID, 4, 200)
	if err != nil {
		t.Fatalf("삭제 뒤 시각화 조회: %v", err)
	}
	// 삭제된 원천은 정점에서 빠지고, 그 원천을 가리키던 간선도 함께 빠져야 한다.
	if len(hops.Contexts) != 1 || hops.Contexts[0].ID != derived.ID {
		t.Fatalf("삭제 뒤 정점 = %#v", hops.Contexts)
	}
	if len(hops.Edges) != 0 {
		t.Fatalf("삭제된 컨텍스트를 가리키는 연결선이 남았다: %#v", hops.Edges)
	}
	assertEdgesInsideResult(t, hops)
}

// TestGraphVisualizationStartsFromGlobalSummaries는 시작점이 「웹 화면 계약」이 정한
// 전역 요약 파생인지, 전역 요약이 없으면 활성 컨텍스트로 되돌아가는지 확인한다.
func TestGraphVisualizationStartsFromGlobalSummaries(t *testing.T) {
	store := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	graphID := createTestGraph(t, store, actorID)

	source := testSourceContext(t, graphID, actorID, "api://viz-entry/"+newTestID(t).String())
	createdSource, err := store.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	// 전역 요약이 없는 그래프도 전체 보기를 만들어야 한다.
	fallback, err := store.GraphVisualization(t.Context(), graphID, 1, 200)
	if err != nil {
		t.Fatalf("되돌림 시각화 조회: %v", err)
	}
	if len(fallback.Contexts) != 1 || fallback.Contexts[0].ID != createdSource.ID {
		t.Fatalf("전역 요약이 없는 그래프의 시작점 = %#v", fallback.Contexts)
	}

	summary := testDerivedContext(t, graphID, actorID)
	summary.Derived.Kind = model.DerivationKindSummary
	summary.Derived.SummaryScope = model.SummaryScopeGlobal
	createdSummary, err := store.CreateContext(t.Context(), graphID, summary, []model.ID{createdSource.ID})
	if err != nil {
		t.Fatalf("전역 요약 생성: %v", err)
	}
	// 전역 요약이 있으면 그것이 시작점이므로 거리 0이고 원천은 한 홉 떨어져 있다.
	hops, err := store.GraphVisualization(t.Context(), graphID, 1, 200)
	if err != nil {
		t.Fatalf("전역 요약 시작 시각화 조회: %v", err)
	}
	if hops.Distances[createdSummary.ID] != 0 || hops.Distances[createdSource.ID] != 1 {
		t.Fatalf("시작점 거리 = %#v", hops.Distances)
	}
	// 0홉은 시작점만 돌려주며 전역 요약이 시작점임을 그대로 드러낸다.
	zero, err := store.GraphVisualization(t.Context(), graphID, 0, 200)
	if err != nil {
		t.Fatalf("0홉 시각화 조회: %v", err)
	}
	if len(zero.Contexts) != 1 || zero.Contexts[0].ID != createdSummary.ID {
		t.Fatalf("0홉 시작점 = %#v", zero.Contexts)
	}
	// 웹은 플랜의 홉 한도 0을 최대 정수 깊이로 바꿔 전달한다. 유한한 그래프의
	// 탐색은 시작점 밖의 근거와 간선을 반환한 뒤 방문할 노드가 없어 끝나야 한다.
	unlimited, err := store.GraphVisualization(t.Context(), graphID, math.MaxInt, 200)
	if err != nil {
		t.Fatalf("홉 한도 없는 시각화 조회: %v", err)
	}
	if len(unlimited.Contexts) != 2 || len(unlimited.Edges) != 1 || unlimited.Distances[createdSource.ID] != 1 || unlimited.Truncated {
		t.Fatalf("홉 한도 없는 시각화 = %#v", unlimited)
	}
}

// TestGraphVisualizationAppliesHopNodeLimit는 화면이 홉 조회의 결과 상한과 절단 경계를
// 그대로 받는지 확인한다. 상한이 없으면 그래프 전체가 한 응답과 브라우저로 실린다.
func TestGraphVisualizationAppliesHopNodeLimit(t *testing.T) {
	store := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	graphID := createTestGraph(t, store, actorID)

	source := testSourceContext(t, graphID, actorID, "api://viz-limit/"+newTestID(t).String())
	createdSource, err := store.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	summary := testDerivedContext(t, graphID, actorID)
	summary.Derived.Kind = model.DerivationKindSummary
	summary.Derived.SummaryScope = model.SummaryScopeGlobal
	if _, err := store.CreateContext(t.Context(), graphID, summary, []model.ID{createdSource.ID}); err != nil {
		t.Fatalf("전역 요약 생성: %v", err)
	}
	for range 3 {
		if _, err := store.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{createdSource.ID}); err != nil {
			t.Fatalf("파생 생성: %v", err)
		}
	}

	capped, err := store.GraphVisualization(t.Context(), graphID, math.MaxInt, 2)
	if err != nil {
		t.Fatalf("상한 적용 시각화 조회: %v", err)
	}
	if len(capped.Contexts) != 2 || !capped.Truncated {
		t.Fatalf("상한 적용 결과 = %d개, 절단 %v", len(capped.Contexts), capped.Truncated)
	}
	assertEdgesInsideResult(t, capped)

	// 상한 0은 「계정 플랜」이 선언한 대로 한도 없음이므로 절단하지 않는다.
	unlimited, err := store.GraphVisualization(t.Context(), graphID, math.MaxInt, 0)
	if err != nil {
		t.Fatalf("무제한 시각화 조회: %v", err)
	}
	if len(unlimited.Contexts) != 5 || unlimited.Truncated {
		t.Fatalf("무제한 결과 = %d개, 절단 %v", len(unlimited.Contexts), unlimited.Truncated)
	}
}

// TestContextDeletionAndRestoreRoundTrip은 노드 복구 화면이 쓰는 조회와 상태 변경이
// 맞물리는지 확인한다. 삭제된 컨텍스트는 복구 화면의 조회에만 나타나야 한다.
func TestContextDeletionAndRestoreRoundTrip(t *testing.T) {
	store := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	graphID := createTestGraph(t, store, actorID)

	source := testSourceContext(t, graphID, actorID, "api://node-restore/"+newTestID(t).String())
	createdSource, err := store.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	if _, err := store.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{createdSource.ID}); err != nil {
		t.Fatalf("파생 생성: %v", err)
	}

	// 삭제 전 안내에 쓰는 연쇄 영향은 이 원천을 근거로 둔 파생을 센다.
	impact, err := store.DeletionImpact(t.Context(), graphID, createdSource.ID)
	if err != nil {
		t.Fatalf("컨텍스트 삭제 영향 조회: %v", err)
	}
	if impact.Derived != 1 {
		t.Fatalf("컨텍스트 삭제 영향 = %#v, want 파생 1개", impact)
	}

	if _, err := store.SetContextDeleted(t.Context(), graphID, createdSource.ID, actorID, true); err != nil {
		t.Fatalf("컨텍스트 소프트 삭제: %v", err)
	}
	active, _, err := store.ListActiveContexts(t.Context(), graphID, 50)
	if err != nil {
		t.Fatalf("활성 컨텍스트 조회: %v", err)
	}
	if containsContext(active, createdSource.ID) {
		t.Fatalf("삭제된 컨텍스트가 활성 목록에 남았다")
	}
	deleted, _, err := store.ListDeletedContexts(t.Context(), graphID, 50)
	if err != nil {
		t.Fatalf("삭제된 컨텍스트 조회: %v", err)
	}
	if !containsContext(deleted, createdSource.ID) {
		t.Fatalf("삭제된 컨텍스트가 복구 목록에 없다")
	}

	if _, err := store.SetContextDeleted(t.Context(), graphID, createdSource.ID, actorID, false); err != nil {
		t.Fatalf("컨텍스트 복구: %v", err)
	}
	active, _, err = store.ListActiveContexts(t.Context(), graphID, 50)
	if err != nil {
		t.Fatalf("복구 뒤 활성 컨텍스트 조회: %v", err)
	}
	if !containsContext(active, createdSource.ID) {
		t.Fatalf("복구한 컨텍스트가 활성 목록에 없다")
	}
}

// TestListOwnedDeletedGraphsSeparatesDeletePaths는 소유자가 직접 삭제한 그래프만
// 소유자 복구 목록에 넣고 자동 삭제분은 운영자 경로에 남기는지 확인한다. 두 경로는
// grace_started_at의 유무로 갈린다.
func TestListOwnedDeletedGraphsSeparatesDeletePaths(t *testing.T) {
	store := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	ownDeleted := createTestGraph(t, store, actorID)
	autoDeleted := createTestGraph(t, store, actorID)
	// 소유자 복구 목록은 소유자 등급으로 판정하므로 등급을 먼저 부여한다.
	for _, graphID := range []model.ID{ownDeleted, autoDeleted} {
		if err := store.GrantGraph(t.Context(), graphID, actorID, actorID, GrantSubjectAccount, model.GraphGradeOwner); err != nil {
			t.Fatalf("소유자 등급 부여: %v", err)
		}
	}

	if err := store.SetGraphDeleted(t.Context(), ownDeleted, actorID, true); err != nil {
		t.Fatalf("그래프 직접 삭제: %v", err)
	}
	if _, err := store.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at = $2, grace_started_at = $2 WHERE graph_id = $1`, autoDeleted.String(), time.Now().UTC()); err != nil {
		t.Fatalf("자동 삭제 상태 설정: %v", err)
	}

	owned, err := store.ListOwnedDeletedGraphs(t.Context(), actorID)
	if err != nil {
		t.Fatalf("직접 삭제한 그래프 조회: %v", err)
	}
	if !containsGraph(owned, ownDeleted) {
		t.Fatalf("직접 삭제한 그래프가 목록에 없다: %#v", owned)
	}
	if containsGraph(owned, autoDeleted) {
		t.Fatalf("자동 삭제된 그래프가 소유자 복구 목록에 섞였다")
	}

	eligible, err := store.ListRestoreEligibleGraphs(t.Context(), actorID)
	if err != nil {
		t.Fatalf("자동 삭제 그래프 조회: %v", err)
	}
	if !containsGraph(eligible, autoDeleted) || containsGraph(eligible, ownDeleted) {
		t.Fatalf("자동 삭제 목록이 두 경로를 나누지 않는다: %#v", eligible)
	}
}

// assertEdgesInsideResult는 응답의 모든 연결선이 응답 안의 컨텍스트만 잇는지 확인한다.
func assertEdgesInsideResult(t *testing.T, hops HopResult) {
	t.Helper()
	ids := make(map[model.ID]struct{}, len(hops.Contexts))
	for _, value := range hops.Contexts {
		ids[value.ID] = struct{}{}
	}
	for _, edge := range hops.Edges {
		if _, found := ids[edge.FromID]; !found {
			t.Errorf("응답에 없는 시작 컨텍스트를 가리키는 연결선: %s -[%s]-> %s", edge.FromID, edge.Kind, edge.ToID)
		}
		if _, found := ids[edge.ToID]; !found {
			t.Errorf("응답에 없는 끝 컨텍스트를 가리키는 연결선: %s -[%s]-> %s", edge.FromID, edge.Kind, edge.ToID)
		}
	}
}

func containsContext(contexts []model.Context, contextID model.ID) bool {
	for _, value := range contexts {
		if value.ID == contextID {
			return true
		}
	}
	return false
}

// TestGraphGrantsAndDeletionImpactExecute는 권한 화면과 삭제 화면이 쓰는 두 질의를 실제
// 데이터베이스에서 실행한다. 두 질의는 8단계 검사 전까지 어느 테스트도 지나지 않아
// 예약어 별칭, agtype 캐스트 누락, 파라미터 타입 충돌이 함께 숨어 있었다.
func TestGraphGrantsAndDeletionImpactExecute(t *testing.T) {
	store := newIntegrationStore(t)
	ownerID, memberID := newTestID(t), newTestID(t)
	createTestAccount(t, store, ownerID)
	createTestAccount(t, store, memberID)
	graphID := createTestGraph(t, store, ownerID)

	if err := store.GrantGraph(t.Context(), graphID, ownerID, ownerID, GrantSubjectAccount, model.GraphGradeOwner); err != nil {
		t.Fatalf("소유자 등급 부여: %v", err)
	}
	team, err := store.CreateTeam(t.Context(), ownerID, "검사 팀 "+newTestID(t).String())
	if err != nil {
		t.Fatalf("팀 생성: %v", err)
	}
	if err := store.AddTeamMember(t.Context(), team.ID, ownerID, memberID); err != nil {
		t.Fatalf("팀 구성원 추가: %v", err)
	}
	if err := store.GrantGraph(t.Context(), graphID, ownerID, team.ID, GrantSubjectTeam, model.GraphGradeViewer); err != nil {
		t.Fatalf("팀 등급 부여: %v", err)
	}
	source := testSourceContext(t, graphID, ownerID, "api://impact/"+newTestID(t).String())
	if _, err := store.CreateContext(t.Context(), graphID, source, nil); err != nil {
		t.Fatalf("원천 생성: %v", err)
	}

	grants, err := store.ListGraphGrants(t.Context(), graphID)
	if err != nil {
		t.Fatalf("그래프 등급 목록 조회: %v", err)
	}
	var direct, inherited *model.GrantSubject
	for index, grant := range grants {
		switch {
		case grant.Inherited && grant.ID == memberID:
			inherited = &grants[index]
		case !grant.Inherited && grant.ID == ownerID:
			direct = &grants[index]
		}
	}
	if direct == nil || inherited == nil {
		t.Fatalf("직접 부여와 팀 상속 등급이 함께 나오지 않았다: %#v", grants)
	}
	// 마지막 소유자는 회수 수단을 주지 않고, 상속 등급은 이 화면에서 회수하지 않는다.
	if direct.CanRevoke {
		t.Fatalf("마지막 소유자에게 회수 수단이 열렸다: %#v", direct)
	}
	if inherited.CanRevoke || inherited.Grade != model.GraphGradeViewer {
		t.Fatalf("팀 상속 등급 = %#v", inherited)
	}

	impact, err := store.DeletionImpact(t.Context(), graphID, model.ID{})
	if err != nil {
		t.Fatalf("그래프 삭제 영향 조회: %v", err)
	}
	// 접근이 차단될 계정은 직접 부여 하나와 팀 구성원 하나다.
	if impact.Contexts != 1 || impact.Accounts != 2 {
		t.Fatalf("그래프 삭제 영향 = %#v, want 컨텍스트 1개와 계정 2개", impact)
	}

	entries, err := store.ListAuditEntries(t.Context(), graphID, 50)
	if err != nil {
		t.Fatalf("감사 기록 조회: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("등급 부여가 감사 기록에 남지 않았다")
	}
}
