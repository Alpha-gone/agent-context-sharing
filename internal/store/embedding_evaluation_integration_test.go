package store

import (
	"context"
	"slices"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestEmbeddingEvaluationScopeIntegration(t *testing.T) {
	for _, vectorType := range []string{"vector", "halfvec"} {
		t.Run(vectorType, func(t *testing.T) {
			database := newEmbeddingContractStore(t, vectorType, 8)
			actor := newTestID(t)
			createTestAccount(t, database, actor)
			graph, other := createTestGraph(t, database, actor), createTestGraph(t, database, actor)
			var own, foreign, old model.ID
			var tied []model.ID
			for i, scope := range []model.ID{graph, graph, graph, graph, other} {
				node, err := database.CreateContext(t.Context(), scope, testSourceContext(t, scope, actor, "urn:test09:"+newTestID(t).String()), nil)
				if err != nil {
					t.Fatal(err)
				}
				modelID := "eval-current"
				if i == 3 {
					modelID, old = "eval-old", node.ID
				}
				result, err := database.ProcessNextIndexTaskInGraph(t.Context(), scope, func(context.Context, IndexTask) IndexTaskResult {
					return IndexTaskResult{Embedding: []float64{1, 0, 0, 0, 0, 0, 0, 0}, ModelID: modelID}
				})
				if err != nil || !result.Succeeded {
					t.Fatalf("시험 벡터 적재: %+v %v", result, err)
				}
				switch i {
				case 0:
					own = node.ID
				case 1, 2:
					tied = append(tied, node.ID)
				case 4:
					foreign = node.ID
				}
			}
			claimExec(t, database, `UPDATE public.context_embedding SET storage_tier='cold' WHERE context_id=$1`, tied[0].String())
			footprint, err := database.EmbeddingFootprint(t.Context(), graph, "eval-current")
			if err != nil || footprint.Rows != 3 || footprint.Bytes <= 0 {
				t.Fatalf("그래프·모델·hot/cold 저장량: %+v %v", footprint, err)
			}
			if empty, err := database.EmbeddingFootprint(t.Context(), graph, "missing"); err != nil || empty != (EmbeddingFootprint{}) {
				t.Fatalf("없는 모델 저장량: %+v %v", empty, err)
			}
			candidates := []model.ID{tied[1], foreign, old, tied[0]}
			scores, err := database.EmbeddingSimilarities(t.Context(), graph, "eval-current", own, candidates)
			slices.SortFunc(tied, func(a, b model.ID) int { return a.Compare(b) })
			if err != nil || len(scores) != 2 || scores[0].ContextID != tied[0] || scores[1].ContextID != tied[1] || scores[0].Similarity != 1 || scores[1].Similarity != 1 {
				t.Fatalf("후보 격리·UUID 동률 순서: %+v %v", scores, err)
			}
			if scores, err := database.EmbeddingSimilarities(t.Context(), graph, "eval-current", foreign, candidates); err != nil || len(scores) != 0 {
				t.Fatalf("다른 그래프 기준 벡터 격리: %+v %v", scores, err)
			}
		})
	}
}
