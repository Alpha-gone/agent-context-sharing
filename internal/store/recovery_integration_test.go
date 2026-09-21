package store

import (
	"context"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestEmbeddingLossRecoversFromBodyIntegration은 「백업과 복구」가 임베딩을 복구 우선순위에서
// 낮게 둔 근거를 실제로 확인한다.
//
// 임베딩은 원본이 아니라 색인이므로 유실되어도 `body`에서 다시 만들 수 있다. 복구 절차는
// 「재색인」이 정한 것과 같다. 그래프의 컨텍스트를 전부 다시 등록하고 작업자가 소비한다.
// 확인할 것은 셋이다. 유실이 의미 유사도 채널에서만 드러나는지, 재등록이 대기 작업을
// 되살리는지, 작업자에게 넘어오는 입력이 저장된 `body`인지다.
func TestEmbeddingLossRecoversFromBodyIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)

	const body = "임베딩 복구 절차를 확인하는 원천 본문"
	source := testSourceContext(t, graphID, actorID, "https://example.test/recovery-"+newTestID(t).String())
	source.Body = body
	stored, err := database.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	const modelID = "recovery-test:vector:1024"
	vector := make([]float64, embeddingDimension(t, database))
	vector[0] = 1

	// 색인을 한 번 끝내 의미 유사도 채널에 걸리는 상태를 만든다.
	indexOnce(t, database, stored.ID, modelID, vector)
	if found := semanticContains(t, database, graphID, modelID, vector, stored.ID); !found {
		t.Fatal("색인한 원천이 의미 유사도 채널에 걸리지 않는다")
	}

	// 임베딩만 지운다. 컨텍스트와 그 body는 그대로 남는다.
	if _, err := database.pool.Exec(t.Context(), `DELETE FROM public.context_embedding WHERE context_id = $1`, stored.ID.String()); err != nil {
		t.Fatalf("임베딩 유실 재현: %v", err)
	}
	if found := semanticContains(t, database, graphID, modelID, vector, stored.ID); found {
		t.Fatal("유실한 임베딩이 의미 유사도 채널에 남아 있다")
	}
	if value, err := database.Context(t.Context(), graphID, stored.ID); err != nil || value.Body != body {
		t.Fatalf("임베딩 유실이 컨텍스트에 영향을 줬다: %v, body=%q", err, value.Body)
	}

	// 복구 절차. 그래프를 재색인해 대기 작업을 되살린다.
	if err := database.ReindexGraph(t.Context(), graphID); err != nil {
		t.Fatalf("그래프 재색인 등록: %v", err)
	}
	readyIndexTasks(t, database, stored.ID)

	var indexedBody string
	result, err := database.ProcessNextIndexTask(t.Context(), func(_ context.Context, task IndexTask) IndexTaskResult {
		indexedBody = task.Body
		return IndexTaskResult{Embedding: vector, ModelID: modelID}
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("복구 색인 처리: %v, %+v", err, result)
	}
	if indexedBody != body {
		t.Fatalf("재생성 입력 = %q; 저장된 body여야 한다", indexedBody)
	}
	if found := semanticContains(t, database, graphID, modelID, vector, stored.ID); !found {
		t.Fatal("재색인 뒤에도 의미 유사도 채널에 걸리지 않는다")
	}
}

// indexOnce는 대기 작업 하나를 주어진 벡터로 처리해 색인을 끝낸 상태를 만든다.
func indexOnce(t *testing.T, database *Store, contextID model.ID, modelID string, vector []float64) {
	t.Helper()
	readyIndexTasks(t, database, contextID)
	result, err := database.ProcessNextIndexTask(t.Context(), func(_ context.Context, task IndexTask) IndexTaskResult {
		if task.ContextID != contextID {
			t.Errorf("대상이 아닌 작업을 잡았다: %s", task.ContextID)
			return IndexTaskResult{Failure: "대상 아님", Retryable: true}
		}
		return IndexTaskResult{Embedding: vector, ModelID: modelID}
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("색인 처리: %v, %+v", err, result)
	}
}

// semanticContains는 의미 유사도 채널이 대상 컨텍스트를 후보로 올리는지 본다.
func semanticContains(t *testing.T, database *Store, graphID model.ID, modelID string, vector []float64, contextID model.ID) bool {
	t.Helper()
	candidates, err := database.SemanticCandidates(t.Context(), graphID, modelID, vector, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("의미 유사도 검색: %v", err)
	}
	for _, candidate := range candidates {
		if candidate.Context.ID == contextID {
			return true
		}
	}
	return false
}
