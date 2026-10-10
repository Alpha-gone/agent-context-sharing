package store

import (
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestCheckGraphIndexReadyScopeAndTargetsIntegration(t *testing.T) {
	database := newIntegrationStoreWithTargets(t, IndexTargetsWithoutSource)
	graph, actor, source := boundaryGraph(t, database)
	derived, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	cleanupIndexTasks(t, database, source.ID, derived.ID)
	processor := scopeTestProcessor(embeddingDimension(t, database))
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.index_task SET next_attempt_at = now() - interval '1 second' WHERE graph_id = $1`, graph.String()); err != nil {
		t.Fatal(err)
	}
	result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, processor)
	if err != nil || !result.Succeeded {
		t.Fatalf("파생 색인: %+v, %v", result, err)
	}
	modelID := processor(t.Context(), IndexTask{}).ModelID
	otherGraph, otherActor, otherSource := boundaryGraph(t, database)
	otherDerived, err := database.CreateContext(t.Context(), otherGraph, testDerivedContext(t, otherGraph, otherActor), []model.ID{otherSource.ID})
	if err != nil {
		t.Fatal(err)
	}
	cleanupIndexTasks(t, database, otherSource.ID, otherDerived.ID)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.index_task SET state = 'failed', last_error = '다른 그래프 실패' WHERE graph_id = $1`, otherGraph.String()); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckGraphIndexReady(t.Context(), graph, modelID); err != nil {
		t.Fatalf("원천 제외 또는 다른 그래프 실패로 평가가 거부됐다: %v", err)
	}
	// 원천 포함 구성에서는 같은 그래프의 빠진 원천 임베딩을 요구한다.
	database.indexTargets = IndexTargetsAllLayers
	if err := database.CheckGraphIndexReady(t.Context(), graph, modelID); err == nil || !strings.Contains(err.Error(), "임베딩 누락=1") {
		t.Fatalf("원천 포함의 준비 검사: %v", err)
	}
	if _, err := database.SetContextDeleted(t.Context(), graph, source.ID, actor, true, 0); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckGraphIndexReady(t.Context(), graph, modelID); err != nil {
		t.Fatalf("삭제된 원천의 임베딩을 요구했다: %v", err)
	}
}
