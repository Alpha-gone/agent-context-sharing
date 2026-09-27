package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestConcurrentTouchEmbeddingsDoesNotDeadlockIntegration은 동시 검색이 겹치는 임베딩의 접근
// 시각을 서로 다른 순서로 기록해도 교착이나 대기 없이 끝나는지 확인한다. 흐름 응답마다 담긴
// 컨텍스트 전부를 한 문장으로 기록하므로, 동시 요청이 늘면 같은 행을 서로 다른 순서로 잠근다.
func TestConcurrentTouchEmbeddingsDoesNotDeadlockIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)

	const contexts = 60
	ids := make([]model.ID, 0, contexts)
	for index := range contexts {
		created, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, accountID, fmt.Sprintf("https://example.test/touch-%s-%d", graphID, index)), nil)
		if err != nil {
			t.Fatalf("컨텍스트 생성: %v", err)
		}
		ids = append(ids, created.ID)
	}
	readyIndexTasks(t, database, graphID, ids...)
	processor := scopeTestProcessor(embeddingDimension(t, database))
	for {
		result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, processor)
		if err != nil {
			t.Fatalf("색인: %v", err)
		}
		if !result.Found {
			break
		}
	}

	// 대기가 쌓이면 끝나지 않으므로 제한 시간을 넘기면 실패로 본다.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const workers, rounds = 32, 20
	var group sync.WaitGroup
	errs := make(chan error, workers*rounds)
	started := time.Now()
	for worker := range workers {
		group.Go(func() {
			random := rand.New(rand.NewPCG(uint64(worker), 7))
			for range rounds {
				subset := make([]model.ID, 0, contexts)
				for _, index := range random.Perm(contexts)[:40] {
					subset = append(subset, ids[index])
				}
				if err := database.touchEmbeddings(ctx, graphID, subset, time.Now().UTC()); err != nil {
					errs <- err
				}
			}
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("동시 접근 기록 실패: %v", err)
	}
	// 교착 감지는 1초를 기다린 뒤 한쪽을 되돌리므로, 대기가 없으면 전체가 그보다 훨씬 짧다.
	t.Logf("동시 접근 기록 %d회: %s", workers*rounds, time.Since(started))
}
