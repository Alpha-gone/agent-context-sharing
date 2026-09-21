package store

import (
	"context"
	"testing"

	"agent_context_sharing/internal/model"
)

// TestIndexTargetsWithoutSourceSkipsSourceIntegration은 원천 제외 구성이 원천의 색인
// 작업을 등록하지 않고 파생은 그대로 등록하는지 확인한다. 「색인 대상 비교」가 두
// 구성을 견주라고 했고, 등록 조건 말고 다른 것이 달라지면 비교가 성립하지 않는다.
func TestIndexTargetsWithoutSourceSkipsSourceIntegration(t *testing.T) {
	database := newIntegrationStoreWithTargets(t, IndexTargetsWithoutSource)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)

	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/index-targets-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}
	cleanupIndexTasks(t, database, source.ID, derived.ID)

	if queued := queuedContextIDs(t, database, graphID); len(queued) != 1 || queued[0] != derived.ID.String() {
		t.Fatalf("파생 하나만 등록되어야 한다: %v", queued)
	}

	// 원천이 색인에서 빠져도 근거 간선은 남아야 한다. 「색인 대상 비교」가 원천을
	// 제외해도 derived_from으로 근거 추적이 유지된다는 것을 전제로 한다.
	stored, err := database.Context(t.Context(), graphID, derived.ID)
	if err != nil {
		t.Fatalf("파생 조회: %v", err)
	}
	if len(stored.Derived.DerivedFrom) != 1 || stored.Derived.DerivedFrom[0] != source.ID {
		t.Fatalf("근거 간선이 남아 있어야 한다: %v", stored.Derived.DerivedFrom)
	}
}

// TestIndexTargetsAllLayersQueuesSourceIntegration은 기본 구성이 원천을 그대로
// 등록하는지 확인한다. 기본값이 바뀌면 두 구성의 비교 기준이 사라진다.
func TestIndexTargetsAllLayersQueuesSourceIntegration(t *testing.T) {
	database := newIntegrationStoreWithTargets(t, IndexTargetsAllLayers)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)

	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/index-targets-all-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	cleanupIndexTasks(t, database, source.ID)

	if queued := queuedContextIDs(t, database, graphID); len(queued) != 1 || queued[0] != source.ID.String() {
		t.Fatalf("원천이 등록되어야 한다: %v", queued)
	}
}

// TestReindexGraphDropsExcludedEmbeddingsIntegration은 재색인이 대상에서 빠진 계층의
// 임베딩과 대기 작업을 지우는지 확인한다. 남겨 두면 예전 구성이 넣은 원천 임베딩이
// 의미 유사도 채널에 계속 올라와 두 구성이 등록 조건 말고도 달라진다.
func TestReindexGraphDropsExcludedEmbeddingsIntegration(t *testing.T) {
	included := newIntegrationStoreWithTargets(t, IndexTargetsAllLayers)
	actorID := newTestID(t)
	createTestAccount(t, included, actorID)
	graphID := createTestGraph(t, included, actorID)

	source, err := included.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/index-targets-reindex-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	cleanupIndexTasks(t, included, source.ID)

	// 원천을 포함한 구성에서 임베딩까지 저장한 상태를 만든다.
	processor := scopeTestProcessor(embeddingDimension(t, included))
	for {
		result, err := included.ProcessNextIndexTaskInGraph(t.Context(), graphID, processor)
		if err != nil {
			t.Fatalf("색인 작업 처리: %v", err)
		}
		if !result.Found {
			break
		}
	}
	if count := embeddingCount(t, included, graphID); count != 1 {
		t.Fatalf("원천 임베딩이 하나 있어야 한다: %d", count)
	}

	excluded := newIntegrationStoreWithTargets(t, IndexTargetsWithoutSource)
	if err := excluded.ReindexGraph(t.Context(), graphID); err != nil {
		t.Fatalf("재색인: %v", err)
	}
	if count := embeddingCount(t, excluded, graphID); count != 0 {
		t.Fatalf("대상에서 빠진 임베딩이 남았다: %d", count)
	}
	if queued := queuedContextIDs(t, excluded, graphID); len(queued) != 0 {
		t.Fatalf("대상에서 빠진 대기 작업이 남았다: %v", queued)
	}
}

func newIntegrationStoreWithTargets(t *testing.T, targets IndexTargets) *Store {
	t.Helper()
	database := newIntegrationStore(t)
	database.indexTargets = targets
	return database
}

// cleanupIndexTasks는 검사가 남긴 대기 작업을 지워 다음 회차의 확보 순서를 흔들지 않게 한다.
func cleanupIndexTasks(t *testing.T, database *Store, contextIDs ...model.ID) {
	t.Helper()
	targets := make([]string, 0, len(contextIDs))
	for _, contextID := range contextIDs {
		targets = append(targets, contextID.String())
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := database.pool.Exec(ctx, `DELETE FROM public.index_task WHERE context_id = ANY($1::uuid[])`, targets); err != nil {
			t.Errorf("색인 작업 정리: %v", err)
		}
	})
}

func queuedContextIDs(t *testing.T, database *Store, graphID model.ID) []string {
	t.Helper()
	rows, err := database.pool.Query(t.Context(), `SELECT context_id::text FROM public.index_task WHERE graph_id = $1 ORDER BY enqueued_at`, graphID.String())
	if err != nil {
		t.Fatalf("색인 대기 작업 조회: %v", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("색인 대기 작업 행 해석: %v", err)
		}
		ids = append(ids, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("색인 대기 작업 행 읽기: %v", err)
	}
	return ids
}

func embeddingCount(t *testing.T, database *Store, graphID model.ID) int {
	t.Helper()
	var count int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.context_embedding WHERE graph_id = $1`, graphID.String()).Scan(&count); err != nil {
		t.Fatalf("임베딩 수 조회: %v", err)
	}
	return count
}
