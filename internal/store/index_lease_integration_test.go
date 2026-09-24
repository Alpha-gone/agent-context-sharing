package store

import (
	"context"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestIndexProviderCallDoesNotBlockSavesIntegration은 제공자 호출이 저장 경로를 막지
// 않는지 확인한다. 작업 행 잠금을 쥔 채 제공자를 부르면 같은 컨텍스트의 저장이 제공자
// 응답까지 기다리게 되어 「색인 처리」의 "대역이 응답을 늦춰도 저장 지연이 늘지 않아야
// 한다"가 성립하지 않는다.
func TestIndexProviderCallDoesNotBlockSavesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/lease-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	// 색인 대상은 본문을 고칠 수 있는 파생으로 둔다. 같은 컨텍스트의 수정이 색인 작업을
	// 다시 등록하므로 작업 행에서 실제로 부딪힌다.
	target, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}

	// 공유 개발 데이터베이스에는 다른 테스트가 남긴 대기 작업이 있다. 대상 작업만 앞으로
	// 옮기면 남은 행 중에 더 앞선 시각을 가진 것이 있을 때 그쪽이 먼저 잡힌다. 나머지
	// 대기 작업을 함께 미루는 readyIndexTasks로 확보 순서를 고정한다.
	//
	// 옮긴 표식은 실패한 회차가 남기면 다음 회차를 가로채므로 반드시 되돌린다. 이 테스트가
	// 만든 컨텍스트의 행만 지우므로 다른 테스트의 대기 작업은 건드리지 않는다.
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := database.pool.Exec(ctx, `DELETE FROM public.index_task WHERE context_id = ANY($1::uuid[])`, []string{source.ID.String(), target.ID.String()}); err != nil {
			t.Errorf("색인 작업 정리: %v", err)
		}
	})
	readyIndexTasks(t, database, graphID, target.ID)

	const providerDelay = 2 * time.Second
	saved := make(chan time.Duration, 1)
	processor := func(ctx context.Context, task IndexTask) IndexTaskResult {
		if task.ContextID != target.ID {
			t.Errorf("대상이 아닌 작업을 잡았다: %s", task.ContextID)
			return IndexTaskResult{Failure: "대상 아님", Retryable: true}
		}
		// 제공자가 응답하는 동안 같은 컨텍스트의 수정이 진행되는지 잰다. 수정은 색인
		// 작업을 다시 등록하므로 작업 행 잠금을 쥐고 있으면 여기에서 막힌다.
		go func() {
			started := time.Now()
			next := target
			next.Body = "제공자 호출 중 수정한 본문"
			next.Version = target.Version + 1
			if _, err := database.UpdateContext(context.WithoutCancel(ctx), graphID, target.Version, next); err != nil {
				t.Errorf("제공자 호출 중 저장: %v", err)
			}
			saved <- time.Since(started)
		}()
		time.Sleep(providerDelay)
		// 배포 구성의 차원과 맞춰야 임베딩 열에 들어간다.
		embedding := make([]float64, embeddingDimension(t, database))
		embedding[0] = 1
		return IndexTaskResult{Embedding: embedding, ModelID: "lease-test"}
	}

	result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, processor)
	if err != nil {
		t.Fatalf("색인 작업 처리: %v", err)
	}
	if !result.Found || result.Task.ContextID != target.ID {
		t.Fatalf("확보한 작업 = %#v; 대상 %s를 잡아야 한다", result, target.ID)
	}

	select {
	case elapsed := <-saved:
		// 제공자 지연의 절반을 넘으면 저장이 제공자 응답을 기다린 것이다.
		if elapsed > providerDelay/2 {
			t.Fatalf("제공자 호출 중 저장에 %v가 걸렸다; 제공자 지연 %v를 기다렸다", elapsed, providerDelay)
		}
	case <-time.After(providerDelay * 3):
		t.Fatal("제공자 호출 중 저장이 끝나지 않았다")
	}
}

// embeddingDimension은 배포가 만든 임베딩 열의 차원을 읽는다.
func embeddingDimension(t *testing.T, database *Store) int {
	t.Helper()
	var dimension int
	if err := database.pool.QueryRow(t.Context(), `
		SELECT COALESCE(substring(format_type(atttypid, atttypmod) FROM '[0-9]+')::int, 0)
		FROM pg_attribute
		WHERE attrelid = 'public.context_embedding'::regclass AND attname = 'embedding'`).Scan(&dimension); err != nil || dimension <= 0 {
		t.Fatalf("임베딩 차원 조회 = %d, %v", dimension, err)
	}
	return dimension
}
