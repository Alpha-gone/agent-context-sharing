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

// queueHeadSentinel은 이 검사가 자기 작업을 대기열 맨 앞에 두려고 쓰는 시각이다.
//
// `readyIndexTasks`가 쓰는 값보다 더 먼 과거로 둔다. 두 값이 같으면 순서가 `task_id`로
// 갈려 다른 검사가 남긴 행이 앞설 수 있고, 그러면 범위를 지정하지 않은 확보가 남의
// 작업을 집는다.
const queueHeadSentinel = "to_timestamp(-6000000000)"

// TestProcessNextIndexTaskSpansGraphsIntegration은 범위를 지정하지 않은 확보가 그래프를
// 가리지 않고 대기 작업을 집는지 확인한다.
//
// 이 경로는 운영 색인 작업자가 쓰는 것이다. 확보 조건이 깨지면 작업자가 조용히 일을 찾지
// 못해 색인이 멈추는데, 그래프로 좁힌 확보만 검사하면 그 사실이 드러나지 않는다.
//
// 공유 데이터베이스에서 이 진입점을 부르는 것이 「테스트 사이의 격리」와 부딪히지 않게
// 순서로 푼다. 자기 행만 대기열 맨 앞으로 당기고, 확보 직전에 선두가 실제로 자기 것인지
// 확인한다. 남의 행은 건드리지 않으며, 선두를 보장할 수 없으면 집지 않고 건너뛴다.
func TestProcessNextIndexTaskSpansGraphsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)

	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/index-unscoped-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	cleanupIndexTasks(t, database, source.ID)
	headIndexTask(t, database, source.ID)

	if head := readyQueueHead(t, database); head != source.ID.String() {
		t.Skipf("대기열 선두가 이 검사의 작업이 아니라 남의 작업을 집게 된다: %s", head)
	}

	result, err := database.ProcessNextIndexTask(t.Context(), scopeTestProcessor(embeddingDimension(t, database)))
	if err != nil {
		t.Fatalf("범위 미지정 확보: %v", err)
	}
	if !result.Found {
		t.Fatal("대기 작업이 있는데 범위를 지정하지 않은 확보가 아무것도 찾지 못했다")
	}
	if result.Task.ContextID != source.ID {
		t.Fatalf("확보한 작업 = %s, want %s", result.Task.ContextID, source.ID)
	}
	if result.Task.GraphID != graphID {
		t.Fatalf("확보한 작업의 그래프 = %s, want %s", result.Task.GraphID, graphID)
	}
	if !result.Succeeded {
		t.Fatalf("확보 결과 = %+v", result)
	}
}

// headIndexTask는 지정한 작업만 대기열 맨 앞으로 당긴다. 자기 행 외에는 건드리지 않는다.
func headIndexTask(t *testing.T, database *Store, contextID model.ID) {
	t.Helper()
	const query = `UPDATE public.index_task
		SET enqueued_at = ` + queueHeadSentinel + `, next_attempt_at = ` + queueHeadSentinel + `
		WHERE context_id = $1`
	if _, err := database.pool.Exec(t.Context(), query, contextID.String()); err != nil {
		t.Fatalf("색인 작업을 대기열 앞으로 당기기: %v", err)
	}
}

// readyQueueHead는 지금 확보 가능한 대기 작업 중 확보 질의가 가장 먼저 고를 것을 돌려준다.
// 확보 질의와 같은 조건과 정렬을 쓴다.
func readyQueueHead(t *testing.T, database *Store) string {
	t.Helper()
	var contextID string
	err := database.pool.QueryRow(t.Context(), `
		SELECT context_id::text
		FROM public.index_task
		WHERE state = 'pending' AND next_attempt_at <= now()
		ORDER BY enqueued_at, task_id
		LIMIT 1`).Scan(&contextID)
	if err != nil {
		t.Fatalf("대기열 선두 조회: %v", err)
	}
	return contextID
}
