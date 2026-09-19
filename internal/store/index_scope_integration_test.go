package store

import (
	"context"
	"testing"

	"agent_context_sharing/internal/model"
)

// TestProcessNextIndexTaskInGraphStaysInGraphIntegration은 그래프로 좁힌 확보가 다른
// 그래프의 대기 작업을 집지 않는지 확인한다. 「검증」의 측정은 잰 그래프의 색인이 끝난
// 상태에서 시작해야 하며, 평가가 남의 대기 작업을 대신 처리하면 시작 상태가 회차마다
// 달라진다.
func TestProcessNextIndexTaskInGraphStaysInGraphIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)

	target := createIndexScopeGraph(t, database, actorID, "target")
	other := createIndexScopeGraph(t, database, actorID, "other")
	processor := scopeTestProcessor(embeddingDimension(t, database))

	// 좁힌 확보가 내주는 작업은 모두 대상 그래프의 것이어야 한다. 공유 개발
	// 데이터베이스에는 다른 그래프의 대기 작업이 남아 있으므로, 가리지 않는 확보였다면
	// 첫 회차부터 다른 그래프의 작업이 나온다.
	processed := 0
	for {
		result, err := database.ProcessNextIndexTaskInGraph(t.Context(), target, processor)
		if err != nil {
			t.Fatalf("대상 그래프 색인 작업 확보: %v", err)
		}
		if !result.Found {
			break
		}
		if result.Task.GraphID != target {
			t.Fatalf("다른 그래프의 작업을 집었다: %s", result.Task.GraphID.String())
		}
		processed++
	}
	if processed != 2 {
		t.Fatalf("대상 그래프의 작업 두 건을 기대했으나 %d건", processed)
	}

	// 대상 그래프를 비운 뒤에도 다른 그래프의 작업은 그대로 남아 있어야 한다.
	result, err := database.ProcessNextIndexTaskInGraph(t.Context(), other, processor)
	if err != nil {
		t.Fatalf("다른 그래프 색인 작업 확보: %v", err)
	}
	if !result.Found || result.Task.GraphID != other {
		t.Fatalf("다른 그래프의 작업이 남아 있어야 한다: %+v", result)
	}
}

// TestProcessNextIndexTaskInGraphRejectsInvalidGraph는 식별자 검증을 확인한다. 빈
// 식별자를 그대로 받으면 조건이 사라져 그래프를 가리지 않는 확보가 된다.
func TestProcessNextIndexTaskInGraphRejectsInvalidGraph(t *testing.T) {
	database := newIntegrationStore(t)
	if _, err := database.ProcessNextIndexTaskInGraph(t.Context(), model.ID{}, scopeTestProcessor(1)); err == nil {
		t.Fatal("UUIDv7이 아닌 그래프 식별자를 받아들였다")
	}
}

// scopeTestProcessor는 제공자를 부르지 않고 배포 구성 차원의 영벡터로 성공을 흉내낸다.
// 이 검사가 보는 것은 확보 범위이므로 임베딩 값 자체는 중요하지 않지만, 저장 열의 차원은
// 「임베딩 스키마」가 배포 구성으로 두었으므로 그 값에 맞춘다.
func scopeTestProcessor(dimension int) IndexTaskProcessor {
	return func(_ context.Context, _ IndexTask) IndexTaskResult {
		return IndexTaskResult{Embedding: make([]float64, dimension), ModelID: "scope-test"}
	}
}

// createIndexScopeGraph는 원천과 파생을 하나씩 담은 그래프를 만들고 두 건의 색인 대기
// 작업을 남긴다. 남긴 작업은 다음 회차를 가로채지 않도록 검사가 끝나면 지운다.
func createIndexScopeGraph(t *testing.T, database *Store, actorID model.ID, label string) model.ID {
	t.Helper()
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/index-scope-"+label+"-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := database.pool.Exec(ctx, `DELETE FROM public.index_task WHERE context_id = ANY($1::uuid[])`, []string{source.ID.String(), derived.ID.String()}); err != nil {
			t.Errorf("색인 작업 정리: %v", err)
		}
	})
	return graphID
}
